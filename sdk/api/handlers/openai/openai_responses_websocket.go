package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	codexcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsRequestTypeCreate    = "response.create"
	wsRequestTypeAppend    = "response.append"
	wsRequestTypeProcessed = "response.processed"
	wsEventTypeError       = "error"
	wsEventTypeCompleted   = "response.completed"
	wsDoneMarker           = "[DONE]"
	wsTurnStateHeader      = "x-codex-turn-state"
	wsTimelineBodyKey      = "WEBSOCKET_TIMELINE_OVERRIDE"

	codexLocalCompactionSummaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"

	maxResponsesWebsocketTimelineBytes      = 1 << 20
	maxResponsesWebsocketErrorTimelineBytes = 4 << 10
	maxResponsesWebsocketInboundBytes       = 64 << 20
	responsesWebsocketWriteTimeout          = 30 * time.Second
	responsesWebsocketPingInterval          = 25 * time.Second
	responsesWebsocketPingWriteTimeout      = 10 * time.Second
	responsesWebsocketRetryPreludeMaxDelay  = 50 * time.Millisecond

	wsCompactResponseTypeKindOffset = len(`{"type":"response.`)
)

const responsesWebsocketTimelineTruncatedMarker = "\n...[websocket timeline truncated]...\n"

var errResponsesWebsocketNilStreamChannels = errors.New("responses websocket forwarder received nil data and error channels")
var errResponsesWebsocketRetryFullTranscript = errors.New("responses websocket retry with full transcript")

var (
	wsDataPrefixBytes             = []byte("data:")
	wsDoneMarkerBytes             = []byte(wsDoneMarker)
	wsEventPrefixBytes            = []byte("event:")
	wsEventTypeCompletedBytes     = []byte(wsEventTypeCompleted)
	wsEventTypeErrorBytes         = []byte(wsEventTypeError)
	wsEventTypeOutputItemAdded    = []byte("response.output_item.added")
	wsEventTypeOutputItemDone     = []byte("response.output_item.done")
	wsEventTypeOutputTextDelta    = []byte("response.output_text.delta")
	wsEventTypeOutputTextDone     = []byte("response.output_text.done")
	wsRequestTypeProcessedBytes   = []byte(wsRequestTypeProcessed)
	wsTypeKeyBytes                = []byte("type")
	wsEventTypeContentPartAdded   = []byte("response.content_part.added")
	wsEventTypeContentPartDone    = []byte("response.content_part.done")
	wsEventTypeResponseCreated    = []byte("response.created")
	wsEventTypeResponseInProgress = []byte("response.in_progress")

	wsCompactEventTypeOutputTextDelta = []byte(`{"type":"response.output_text.delta"`)
	wsCompactEventTypeOutputTextDone  = []byte(`{"type":"response.output_text.done"`)
	wsCompactEventTypeOutputItemAdded = []byte(`{"type":"response.output_item.added"`)
	wsCompactEventTypeOutputItemDone  = []byte(`{"type":"response.output_item.done"`)
)

type websocketTimelineBuilder struct {
	strings.Builder
	maxBytes  int
	errorOnly bool
}

func newWebsocketTimelineBuilder(maxBytes int) websocketTimelineBuilder {
	if maxBytes <= 0 {
		maxBytes = maxResponsesWebsocketTimelineBytes
	}
	return websocketTimelineBuilder{maxBytes: maxBytes}
}

func newResponsesWebsocketTimelineBuilder(h *OpenAIResponsesAPIHandler) websocketTimelineBuilder {
	builder := newWebsocketTimelineBuilder(responsesWebsocketTimelineLimit(h))
	if h != nil && h.Cfg != nil && !h.Cfg.RequestLog {
		builder.errorOnly = true
	}
	return builder
}

func responsesWebsocketTimelineLimit(h *OpenAIResponsesAPIHandler) int {
	if h == nil || h.Cfg == nil || h.Cfg.RequestLog {
		return maxResponsesWebsocketTimelineBytes
	}
	return maxResponsesWebsocketErrorTimelineBytes
}

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:    1024,
	WriteBufferSize:   1024,
	WriteBufferPool:   &responsesWebsocketWriteBufferPool,
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		return responsesWebsocketOriginAllowed(r)
	},
}

var responsesWebsocketWriteBufferPool sync.Pool

func responsesWebsocketOriginAllowed(r *http.Request) bool {
	if r == nil {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsedOrigin, err := url.Parse(origin)
	if err != nil || parsedOrigin == nil {
		return false
	}
	originScheme := strings.TrimSpace(parsedOrigin.Scheme)
	if !strings.EqualFold(originScheme, "http") && !strings.EqualFold(originScheme, "https") {
		return false
	}
	if parsedOrigin.User != nil || parsedOrigin.Path != "" || parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" || parsedOrigin.Opaque != "" {
		return false
	}
	originHost, ok := parseWebsocketAuthority(parsedOrigin.Host)
	if !ok {
		return false
	}
	if websocketAuthorityMatchesOrigin(originHost, originScheme, r.Host) {
		return true
	}
	return false
}

type websocketAuthority struct {
	host string
	port string
}

func websocketAuthorityMatchesOrigin(origin websocketAuthority, originScheme string, rawRequestHost string) bool {
	requestHost, ok := parseWebsocketAuthority(rawRequestHost)
	if !ok || requestHost.host != origin.host {
		return false
	}
	originPort := origin.port
	if originPort == "" {
		originPort = defaultWebsocketOriginPort(originScheme)
	}
	if requestHost.port != "" {
		return requestHost.port == originPort
	}
	return originPort != "" && originPort == defaultWebsocketOriginPort(originScheme)
}

func parseWebsocketAuthority(raw string) (websocketAuthority, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || strings.Contains(raw, "@") {
		return websocketAuthority{}, false
	}
	if host, port, err := net.SplitHostPort(raw); err == nil {
		host = normalizeWebsocketAuthorityHost(host)
		port = strings.TrimSpace(port)
		if host == "" || !validWebsocketAuthorityPort(port) {
			return websocketAuthority{}, false
		}
		return websocketAuthority{host: host, port: port}, true
	}
	host := normalizeWebsocketAuthorityHost(raw)
	if host == "" {
		return websocketAuthority{}, false
	}
	if strings.Count(host, ":") == 1 {
		return websocketAuthority{}, false
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return websocketAuthority{}, false
	}
	return websocketAuthority{host: host}, true
}

func normalizeWebsocketAuthorityHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimSuffix(host, ".")
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	}
	return host
}

func validWebsocketAuthorityPort(port string) bool {
	if port == "" {
		return false
	}
	value, err := strconv.Atoi(port)
	return err == nil && value > 0 && value <= 65535
}

func defaultWebsocketOriginPort(scheme string) string {
	scheme = strings.TrimSpace(scheme)
	switch {
	case strings.EqualFold(scheme, "http"):
		return "80"
	case strings.EqualFold(scheme, "https"):
		return "443"
	default:
		return ""
	}
}

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	conn.SetReadLimit(maxResponsesWebsocketInboundBytes)
	connectionID := uuid.NewString()
	generatedExecutionSessionID := uuid.NewString()
	activeExecutionSessionID := ""
	headerExecutionSessionID := responsesExplicitExecutionSessionID(c.Request, nil)
	downstreamSessionKey := websocketDownstreamSessionKey(c.Request)
	if downstreamSessionKey == "" {
		downstreamSessionKey = connectionID
	}
	toolOutputCache, toolCallCache, toolSessionRefs := currentDefaultWebsocketToolCaches()
	retainResponsesWebsocketToolCachesWithRefs(toolSessionRefs, downstreamSessionKey)
	clientIP := websocketClientAddress(c)
	log.Infof("responses websocket: client connected id=%s remote=%s", connectionID, clientIP)
	wsDone := make(chan struct{})
	defer close(wsDone)
	go keepResponsesWebsocketAlive(conn, wsDone, responsesWebsocketPingInterval)
	disconnectMonitor := newResponsesWebsocketDisconnectMonitor(h, conn, wsDone)
	disconnectSessionID := headerExecutionSessionID
	if disconnectSessionID == "" {
		disconnectSessionID = generatedExecutionSessionID
	}
	disconnectMonitor.setActive(disconnectSessionID)
	disconnectMonitor.subscribe(disconnectSessionID)
	var wsTerminateErr error
	wsTimelineLog := newResponsesWebsocketTimelineBuilder(h)
	defer func() {
		releaseResponsesWebsocketToolCachesWithCaches(toolOutputCache, toolCallCache, toolSessionRefs, downstreamSessionKey)
		if wsTerminateErr != nil {
			appendWebsocketTimelineDisconnect(&wsTimelineLog, wsTerminateErr, time.Now())
			// log.Infof("responses websocket: session closing id=%s reason=%v", connectionID, wsTerminateErr)
		} else {
			log.Infof("responses websocket: session closing id=%s", connectionID)
		}
		if h != nil && h.AuthManager != nil {
			closeExecutionSessionID := strings.TrimSpace(activeExecutionSessionID)
			if closeExecutionSessionID == "" {
				if headerExecutionSessionID != "" {
					closeExecutionSessionID = headerExecutionSessionID
				} else {
					closeExecutionSessionID = generatedExecutionSessionID
				}
			}
			h.AuthManager.CloseExecutionSession(closeExecutionSessionID)
			log.Infof("responses websocket: upstream execution session closed id=%s", closeExecutionSessionID)
		}
		setWebsocketTimelineBody(c, wsTimelineLog.String())
		if errClose := conn.Close(); errClose != nil {
			log.Warnf("responses websocket: close connection error: %v", errClose)
		}
	}()

	var lastRequest []byte
	lastResponseOutput := []byte("[]")
	lastResponseID := ""
	lastUpstreamResponseID := ""
	var lastResponsePendingToolCallIDs []string
	lastResponseIDIncrementalEligible := false
	pinnedAuthID := ""
	pinnedAuthModelKey := ""
	incrementalInputSupportByModel := make(map[string]bool)
	requireFreshFullTranscriptBeforeIncremental := false
	resetIncrementalInputSupportCache := func() {
		incrementalInputSupportByModel = make(map[string]bool)
	}
	releasePinnedAuth := func(sessionID, reason string) bool {
		if pinnedAuthID == "" {
			return false
		}
		authID := pinnedAuthID
		pinnedAuthID = ""
		pinnedAuthModelKey = ""
		resetIncrementalInputSupportCache()
		requireFreshFullTranscriptBeforeIncremental = true
		lastResponseIDIncrementalEligible = false
		sessionID = strings.TrimSpace(sessionID)
		if h != nil && h.AuthManager != nil && sessionID != "" {
			disconnectMonitor.suppressNext(sessionID)
			disconnectMonitor.setActive("")
			h.AuthManager.ResetExecutionSession(sessionID)
			disconnectMonitor.setActive(sessionID)
		}
		log.Infof("responses websocket: unpinned auth id=%s session=%s reason=%s", authID, sessionID, reason)
		return true
	}
	clearPinnedAuthIfUnusable := func(sessionID, modelName, reason string) bool {
		if pinnedAuthID == "" {
			return false
		}
		if h != nil && h.responsesWebsocketPinnedAuthReusableForModel(sessionID, pinnedAuthID, modelName, pinnedAuthModelKey) {
			return false
		}
		return releasePinnedAuth(sessionID, reason)
	}

	for {
		msgType, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			if websocket.IsCloseError(errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				log.Infof("responses websocket: client disconnected id=%s error=%v", connectionID, errReadMessage)
			} else {
				// log.Warnf("responses websocket: read message failed id=%s error=%v", connectionID, errReadMessage)
			}
			return
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		// log.Infof(
		// 	"responses websocket: downstream_in id=%s type=%d event=%s payload=%s",
		// 	passthroughSessionID,
		// 	msgType,
		// 	websocketPayloadEventType(payload),
		// 	websocketPayloadPreview(payload),
		// )
		appendWebsocketTimelineEvent(&wsTimelineLog, "request", payload, time.Now())
		if isResponsesWebsocketControlAck(payload) {
			continue
		}
		replacesTranscript := shouldReplaceWebsocketTranscript(payload, gjson.GetBytes(payload, "input"))

		currentExecutionSessionID := headerExecutionSessionID
		if currentExecutionSessionID == "" {
			currentExecutionSessionID = responsesExplicitExecutionSessionID(c.Request, payload)
		}
		if currentExecutionSessionID == "" {
			if activeExecutionSessionID != "" {
				currentExecutionSessionID = activeExecutionSessionID
			} else {
				currentExecutionSessionID = generatedExecutionSessionID
			}
		}
		sessionChanged := activeExecutionSessionID != "" && activeExecutionSessionID != currentExecutionSessionID
		normalizationLastRequest := lastRequest
		normalizationLastResponseOutput := lastResponseOutput
		normalizationLastResponseID := lastResponseID
		normalizationLastUpstreamResponseID := lastUpstreamResponseID
		normalizationLastResponsePendingToolCallIDs := lastResponsePendingToolCallIDs
		if sessionChanged {
			normalizationLastRequest = nil
			normalizationLastResponseOutput = []byte("[]")
			normalizationLastResponseID = ""
			normalizationLastUpstreamResponseID = ""
			normalizationLastResponsePendingToolCallIDs = nil
			lastResponseIDIncrementalEligible = false
		}
		requestModelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
		if requestModelName == "" {
			requestModelName = strings.TrimSpace(gjson.GetBytes(normalizationLastRequest, "model").String())
		}
		pinnedAuthClearedBeforeNormalize := clearPinnedAuthIfUnusable(currentExecutionSessionID, requestModelName, "pre_normalize")

		allowIncrementalInputWithPreviousResponseID := false
		payloadPreviousResponseID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
		if pinnedAuthClearedBeforeNormalize {
			allowIncrementalInputWithPreviousResponseID = false
		} else if requireFreshFullTranscriptBeforeIncremental {
			allowIncrementalInputWithPreviousResponseID = false
		} else if payloadPreviousResponseID != "" {
			allowIncrementalInputWithPreviousResponseID = pinnedAuthID != "" &&
				lastResponseIDIncrementalEligible &&
				payloadPreviousResponseID == normalizationLastResponseID
		} else if pinnedAuthID != "" {
			allowIncrementalInputWithPreviousResponseID = true
		} else {
			allowIncrementalInputWithPreviousResponseID = cachedResponsesWebsocketIncrementalInputSupport(
				incrementalInputSupportByModel,
				requestModelName,
				h.websocketUpstreamSupportsIncrementalInputForModel,
			)
		}
		allowCompactionReplayBypass := false
		if pinnedAuthID != "" {
			if pinnedAuth, ok := responsesWebsocketSessionAuthByID(h, currentExecutionSessionID, pinnedAuthID); ok {
				allowCompactionReplayBypass = responsesWebsocketAuthSupportsCompactionReplay(pinnedAuth)
			}
		} else {
			allowCompactionReplayBypass = h.websocketUpstreamSupportsCompactionReplayForModel(requestModelName)
		}
		if allowCompactionReplayBypass &&
			strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) == "" &&
			inputContainsFullTranscript(gjson.GetBytes(payload, "input")) {
			replacesTranscript = true
		}

		var requestJSON []byte
		var updatedLastRequest []byte
		var errMsg *interfaces.ErrorMessage
		requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketRequestWithState(
			payload,
			normalizationLastRequest,
			normalizationLastResponseOutput,
			normalizationLastResponsePendingToolCallIDs,
			allowIncrementalInputWithPreviousResponseID,
			allowCompactionReplayBypass,
		)
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, &wsTimelineLog, errMsg)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				connectionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					connectionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				return
			}
			continue
		}
		if !allowIncrementalInputWithPreviousResponseID {
			if stripped, errDelete := sjson.DeleteBytes(requestJSON, "previous_response_id"); errDelete == nil {
				requestJSON = stripped
				updatedLastRequest = stripped
			}
		}

		toolCachesReset := false
		disconnectMonitor.setActive(currentExecutionSessionID)
		if sessionChanged {
			if h != nil && h.AuthManager != nil {
				h.AuthManager.ResetExecutionSession(activeExecutionSessionID)
			}
			resetResponsesWebsocketToolCachesWithCaches(toolOutputCache, toolCallCache, downstreamSessionKey)
			toolCachesReset = true
		}
		activeExecutionSessionID = currentExecutionSessionID

		if shouldHandleResponsesWebsocketPrewarmLocally(payload, normalizationLastRequest, allowIncrementalInputWithPreviousResponseID) {
			if updated, errDelete := sjson.DeleteBytes(requestJSON, "generate"); errDelete == nil {
				requestJSON = updated
			}
			if updated, errDelete := sjson.DeleteBytes(updatedLastRequest, "generate"); errDelete == nil {
				updatedLastRequest = updated
			}
			lastRequest = updatedLastRequest
			lastResponseOutput = []byte("[]")
			lastResponseID = ""
			lastUpstreamResponseID = ""
			lastResponsePendingToolCallIDs = nil
			if errWrite := writeResponsesWebsocketSyntheticPrewarm(c, conn, requestJSON, &wsTimelineLog, connectionID); errWrite != nil {
				wsTerminateErr = errWrite
				return
			}
			continue
		}

		if replacesTranscript {
			if !toolCachesReset {
				resetResponsesWebsocketToolCachesWithCaches(toolOutputCache, toolCallCache, downstreamSessionKey)
			}
			requestJSON = repairResponsesWebsocketToolCallsWithCaches(toolOutputCache, toolCallCache, downstreamSessionKey, requestJSON)
		} else {
			requestJSON = repairResponsesWebsocketToolCallsWithCaches(toolOutputCache, toolCallCache, downstreamSessionKey, requestJSON)
		}
		updatedLastRequest = requestJSON
		requestStateToCommit := updatedLastRequest
		// Keep a replayable logical transcript even while the upstream fast path only
		// receives the incremental input. A previous_response_id can become invalid
		// after an upstream websocket reconnect or provider-side state eviction; the
		// recovery request must then contain every earlier turn, not only the most
		// recently committed delta.
		if allowIncrementalInputWithPreviousResponseID &&
			strings.TrimSpace(gjson.GetBytes(requestJSON, "previous_response_id").String()) != "" {
			_, fullTranscriptRequest, fullTranscriptErr := normalizeResponsesWebsocketRequestWithState(
				payload,
				normalizationLastRequest,
				normalizationLastResponseOutput,
				normalizationLastResponsePendingToolCallIDs,
				false,
				allowCompactionReplayBypass,
			)
			if fullTranscriptErr != nil {
				h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), fullTranscriptErr)
				markAPIResponseTimestamp(c)
				if _, errWrite := writeResponsesWebsocketError(conn, &wsTimelineLog, fullTranscriptErr); errWrite != nil {
					wsTerminateErr = errWrite
					return
				}
				continue
			}
			requestStateToCommit = fullTranscriptRequest
		}
		// A transparent full-transcript retry can keep an already-emitted downstream
		// response ID stable while the replacement upstream response receives a new
		// ID. Translate that stable client-visible ID back to the live upstream ID on
		// the next incremental turn.
		if allowIncrementalInputWithPreviousResponseID &&
			normalizationLastResponseID != "" &&
			normalizationLastUpstreamResponseID != "" &&
			normalizationLastResponseID != normalizationLastUpstreamResponseID &&
			strings.TrimSpace(gjson.GetBytes(requestJSON, "previous_response_id").String()) == normalizationLastResponseID {
			if remapped, errSet := sjson.SetBytes(requestJSON, "previous_response_id", normalizationLastUpstreamResponseID); errSet == nil {
				requestJSON = remapped
			}
		}

		modelName := gjson.GetBytes(requestJSON, "model").String()
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		cliCtx = handlers.WithExecutionSessionID(cliCtx, currentExecutionSessionID)
		clearPinnedAuthIfUnusable(currentExecutionSessionID, modelName, "pre_request")
		requestSelectedIncrementalAuth := false
		if pinnedAuthID != "" {
			requestSelectedIncrementalAuth = h.responsesWebsocketPinnedAuthReusableForModel(currentExecutionSessionID, pinnedAuthID, modelName, pinnedAuthModelKey)
			cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
			if requestSelectedIncrementalAuth &&
				strings.TrimSpace(gjson.GetBytes(requestJSON, "previous_response_id").String()) != "" {
				cliCtx = handlers.WithMaxRetryCredentials(cliCtx, 1)
			}
		} else {
			cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
				requestSelectedIncrementalAuth = false
				authID = strings.TrimSpace(authID)
				if authID == "" || h == nil || h.AuthManager == nil {
					return
				}
				selectedAuth, ok := responsesWebsocketSessionAuthByID(h, currentExecutionSessionID, authID)
				if !ok || selectedAuth == nil {
					return
				}
				if selectedAuth.Disabled || selectedAuth.Unavailable || selectedAuth.Status == coreauth.StatusDisabled {
					return
				}
				if responsesWebsocketAuthSupportsIncrementalInput(selectedAuth) {
					pinnedAuthID = authID
					_, pinnedAuthModelKey = responsesWebsocketModelRoute(modelName)
					if !requireFreshFullTranscriptBeforeIncremental {
						requestSelectedIncrementalAuth = true
					}
				}
			})
		}
		dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")
		disconnectMonitor.subscribe(currentExecutionSessionID)

		hasIncrementalPreviousResponseID := strings.TrimSpace(gjson.GetBytes(requestJSON, "previous_response_id").String()) != ""
		retryFullTranscript := allowIncrementalInputWithPreviousResponseID && hasIncrementalPreviousResponseID
		retryCredentialFailoverWithFullTranscript := requestSelectedIncrementalAuth && hasIncrementalPreviousResponseID
		forwardRetryState := newResponsesWebsocketForwardRetryState()
		completedOutput, completedResponseID, completedForward, errForward := h.forwardResponsesWebsocketWithOptions(
			c,
			conn,
			cliCancel,
			dataChan,
			errChan,
			&wsTimelineLog,
			connectionID,
			downstreamSessionKey,
			responsesWebsocketForwardOptions{
				allowFullTranscriptRetry: retryFullTranscript,
				bufferRetryPrelude:       retryFullTranscript,
				retryCredentialFailover:  retryCredentialFailoverWithFullTranscript,
				retryState:               forwardRetryState,
			},
			toolCallCache,
		)
		if errors.Is(errForward, errResponsesWebsocketRetryFullTranscript) &&
			allowIncrementalInputWithPreviousResponseID &&
			strings.TrimSpace(gjson.GetBytes(requestJSON, "previous_response_id").String()) != "" {
			if forwardRetryState != nil {
				forwardRetryState.beginRetry()
			}
			if h != nil && h.AuthManager != nil {
				disconnectMonitor.suppressNext(currentExecutionSessionID)
				disconnectMonitor.setActive("")
				h.AuthManager.ResetExecutionSession(currentExecutionSessionID)
				disconnectMonitor.setActive(currentExecutionSessionID)
			}
			retryJSON, _, retryErrMsg := normalizeResponsesWebsocketRequestWithState(
				payload,
				normalizationLastRequest,
				normalizationLastResponseOutput,
				normalizationLastResponsePendingToolCallIDs,
				false,
				allowCompactionReplayBypass,
			)
			if retryErrMsg != nil {
				h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), retryErrMsg)
				markAPIResponseTimestamp(c)
				if _, errWrite := writeResponsesWebsocketError(conn, &wsTimelineLog, retryErrMsg); errWrite != nil {
					wsTerminateErr = errWrite
					return
				}
				continue
			}
			if stripped, errDelete := sjson.DeleteBytes(retryJSON, "previous_response_id"); errDelete == nil {
				retryJSON = stripped
			}
			retryJSON = repairResponsesWebsocketToolCallsWithCaches(toolOutputCache, toolCallCache, downstreamSessionKey, retryJSON)
			requestStateToCommit = retryJSON
			modelName = gjson.GetBytes(retryJSON, "model").String()
			cliCtx, cliCancel = h.GetContextWithCancel(h, c, context.Background())
			cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
			cliCtx = handlers.WithExecutionSessionID(cliCtx, currentExecutionSessionID)
			clearPinnedAuthIfUnusable(currentExecutionSessionID, modelName, "pre_retry")
			if pinnedAuthID != "" && h.responsesWebsocketPinnedAuthReusableForModel(currentExecutionSessionID, pinnedAuthID, modelName, pinnedAuthModelKey) {
				requestSelectedIncrementalAuth = true
				cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
			}
			dataChan, _, errChan = h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, retryJSON, "")
			disconnectMonitor.subscribe(currentExecutionSessionID)
			completedOutput, completedResponseID, completedForward, errForward = h.forwardResponsesWebsocketWithOptions(
				c,
				conn,
				cliCancel,
				dataChan,
				errChan,
				&wsTimelineLog,
				connectionID,
				downstreamSessionKey,
				responsesWebsocketForwardOptions{retryState: forwardRetryState},
				toolCallCache,
			)
		}
		if errForward != nil {
			wsTerminateErr = errForward
			log.Warnf("responses websocket: forward failed id=%s error=%v", connectionID, errForward)
			return
		}
		if shouldReleaseResponsesWebsocketPinnedAuth(forwardRetryState.terminalError) {
			releasePinnedAuth(currentExecutionSessionID, "terminal_upstream_error")
			requestSelectedIncrementalAuth = false
		} else if clearPinnedAuthIfUnusable(currentExecutionSessionID, modelName, "post_forward") {
			requestSelectedIncrementalAuth = false
		}
		if completedForward {
			lastRequest = requestStateToCommit
			lastResponseOutput = completedOutput
			lastResponsePendingToolCallIDs = sortedStringSet(forwardRetryState.pendingToolCallIDs)
			if requestSelectedIncrementalAuth {
				lastResponseID = strings.TrimSpace(completedResponseID)
				lastUpstreamResponseID = lastResponseID
				if forwardRetryState != nil && forwardRetryState.retrying {
					if upstreamResponseID := strings.TrimSpace(forwardRetryState.retryUpstreamResponseID); upstreamResponseID != "" {
						lastUpstreamResponseID = upstreamResponseID
					}
				}
				lastResponseIDIncrementalEligible = lastResponseID != ""
				requireFreshFullTranscriptBeforeIncremental = false
			} else {
				lastResponseID = ""
				lastUpstreamResponseID = ""
				lastResponseIDIncrementalEligible = false
			}
		}
	}
}

func keepResponsesWebsocketAlive(conn *websocket.Conn, done <-chan struct{}, interval time.Duration) {
	if conn == nil || done == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, now.Add(responsesWebsocketPingWriteTimeout)); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func isResponsesWebsocketControlAck(payload []byte) bool {
	return websocketPayloadEventTypeValue(payload) == wsRequestTypeProcessed
}

func responsesWebsocketSessionAuthByID(h *OpenAIResponsesAPIHandler, sessionID, authID string) (*coreauth.Auth, bool) {
	if h == nil || h.AuthManager == nil {
		return nil, false
	}
	if auth, ok := h.AuthManager.GetExecutionSessionAuthByID(sessionID, authID); ok {
		return auth, true
	}
	return h.AuthManager.GetByID(authID)
}

func websocketClientAddress(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return strings.TrimSpace(c.ClientIP())
}

func cachedResponsesWebsocketIncrementalInputSupport(cache map[string]bool, modelName string, resolve func(string) bool) bool {
	if resolve == nil {
		return false
	}

	modelName = strings.TrimSpace(modelName)
	if modelName == "" || cache == nil {
		return resolve(modelName)
	}

	if supported, ok := cache[modelName]; ok {
		return supported
	}

	supported := resolve(modelName)
	cache[modelName] = supported
	return supported
}

func websocketUpgradeHeaders(req *http.Request) http.Header {
	headers := http.Header{}
	if req == nil {
		return headers
	}

	// Keep the same sticky turn-state across reconnects when provided by the client.
	turnState := strings.TrimSpace(req.Header.Get(wsTurnStateHeader))
	if turnState != "" {
		headers.Set(wsTurnStateHeader, turnState)
	}
	return headers
}

func normalizeResponsesWebsocketRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithMode(rawJSON, lastRequest, lastResponseOutput, true)
}

func normalizeResponsesWebsocketRequestWithMode(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithState(rawJSON, lastRequest, lastResponseOutput, nil, allowIncrementalInputWithPreviousResponseID, true)
}

func normalizeResponsesWebsocketRequestWithState(
	rawJSON []byte,
	lastRequest []byte,
	lastResponseOutput []byte,
	lastResponsePendingToolCallIDs []string,
	allowIncrementalInputWithPreviousResponseID bool,
	allowCompactionReplayBypass bool,
) ([]byte, []byte, *interfaces.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		// log.Infof("responses websocket: response.create request")
		if len(lastRequest) == 0 {
			normalized, next, errMsg := normalizeResponseCreateRequest(rawJSON)
			if errMsg == nil && !allowCompactionReplayBypass {
				normalized = stripResponsesWebsocketCompactionItems(normalized)
				next = normalized
			}
			return normalized, next, errMsg
		}
		return normalizeResponseSubsequentRequestWithState(rawJSON, lastRequest, lastResponseOutput, lastResponsePendingToolCallIDs, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
	case wsRequestTypeAppend:
		// log.Infof("responses websocket: response.append request")
		return normalizeResponseSubsequentRequestWithState(rawJSON, lastRequest, lastResponseOutput, lastResponsePendingToolCallIDs, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
	default:
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
}

func normalizeResponseCreateRequest(rawJSON []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = rawJSON
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	if input := gjson.GetBytes(normalized, "input"); !input.Exists() || input.Type == gjson.Null {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	}
	normalized = codexcommon.NormalizeResponseInputItems(normalized)

	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if modelName == "" {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("missing model in response.create request"),
		}
	}
	return normalized, normalized, nil
}

func normalizeResponseSubsequentRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponseSubsequentRequestWithState(rawJSON, lastRequest, lastResponseOutput, nil, allowIncrementalInputWithPreviousResponseID, true)
}

func normalizeResponseSubsequentRequestWithState(
	rawJSON []byte,
	lastRequest []byte,
	lastResponseOutput []byte,
	lastResponsePendingToolCallIDs []string,
	allowIncrementalInputWithPreviousResponseID bool,
	allowCompactionReplayBypass bool,
) ([]byte, []byte, *interfaces.ErrorMessage) {
	if len(lastRequest) == 0 {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request received before response.create"),
		}
	}

	nextInput := gjson.GetBytes(rawJSON, "input")
	if !nextInput.Exists() || !nextInput.IsArray() {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}

	compactReplay := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) == "" && inputContainsFullTranscript(nextInput)
	// Compaction can cause clients to replace local websocket history with a new
	// compact transcript on the next `response.create`. When the input already
	// contains historical model output items, treating it as an incremental append
	// duplicates stale turn-state and can leave late orphaned function_call items.
	if compactReplay && allowCompactionReplayBypass {
		normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
		return normalized, normalized, nil
	}
	if !compactReplay && shouldReplaceWebsocketTranscript(rawJSON, nextInput) {
		normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
		return normalized, normalized, nil
	}

	// Websocket v2 mode uses response.create with previous_response_id + incremental input.
	// Do not expand it into a full input transcript; upstream expects the incremental payload.
	if allowIncrementalInputWithPreviousResponseID {
		if prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()); prev != "" {
			normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
			if errDelete != nil {
				normalized = rawJSON
			}
			normalized = inheritResponsesWebsocketRequestConfiguration(normalized, lastRequest)
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			normalized = codexcommon.NormalizeResponseInputItems(normalized)
			if inputSatisfiesPendingToolCalls(gjson.GetBytes(normalized, "input"), lastResponsePendingToolCallIDs) &&
				websocketIncrementalToolOutputsKnown(gjson.GetBytes(normalized, "input"), gjson.ParseBytes(lastResponseOutput)) {
				return normalized, normalized, nil
			}
		}
	}

	existingInput := gjson.GetBytes(lastRequest, "input")
	mergedInput := mergeJSONArrayRawTrusted(existingInput.Raw, trustedJSONArrayRawString(lastResponseOutput))
	appendInputRaw := nextInput.Raw
	if compactReplay {
		appendInputRaw = inputWithoutCompactionItems(nextInput)
	}
	mergedInput = mergeJSONArrayRawTrusted(mergedInput, appendInputRaw)
	dedupedInput, errDedupeFunctionCalls := dedupeFunctionCallsByCallID(mergedInput)
	if errDedupeFunctionCalls == nil {
		mergedInput = dedupedInput
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = rawJSON
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("failed to merge websocket input: %w", errSet),
		}
	}
	normalized = inheritResponsesWebsocketRequestConfiguration(normalized, lastRequest)
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	normalized = codexcommon.NormalizeResponseInputItems(normalized)
	return normalized, normalized, nil
}

// inheritResponsesWebsocketRequestConfiguration carries connection-level
// request configuration forward when a subsequent response.create only sends
// incremental input. Explicit values, including null or an empty tools array,
// always win. Tool choice and parallelism are inherited as a group with the
// previous tools so a replacement tool list cannot retain a stale forced tool.
func inheritResponsesWebsocketRequestConfiguration(current, previous []byte) []byte {
	if len(current) == 0 || len(previous) == 0 {
		return current
	}
	inheritField := func(field string) {
		if gjson.GetBytes(current, field).Exists() {
			return
		}
		value := gjson.GetBytes(previous, field)
		if !value.Exists() {
			return
		}
		if updated, err := sjson.SetRawBytes(current, field, []byte(value.Raw)); err == nil {
			current = updated
		}
	}

	inheritField("model")
	inheritField("instructions")
	if !gjson.GetBytes(current, "tools").Exists() {
		inheritField("tools")
		inheritField("tool_choice")
		inheritField("parallel_tool_calls")
	}
	return current
}

func websocketIncrementalToolOutputsKnown(input gjson.Result, lastResponseOutput gjson.Result) bool {
	if !input.IsArray() {
		return false
	}
	if !strings.Contains(input.Raw, `_output"`) {
		return true
	}

	knownCalls := make(map[string]string)
	if lastResponseOutput.IsArray() {
		lastResponseOutput.ForEach(func(_, item gjson.Result) bool {
			itemType := strings.TrimSpace(item.Get("type").String())
			if !isResponsesToolCallType(itemType) {
				return true
			}
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID != "" {
				knownCalls[callID] = itemType
			}
			return true
		})
	}

	allKnown := true
	input.ForEach(func(_, item gjson.Result) bool {
		itemType := strings.TrimSpace(item.Get("type").String())
		if isResponsesToolCallType(itemType) {
			if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
				knownCalls[callID] = itemType
			}
			return true
		}
		if !isResponsesToolCallOutputType(itemType) {
			return true
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		if itemType == "tool_search_output" {
			execution := strings.TrimSpace(item.Get("execution").String())
			if callID == "" || strings.EqualFold(execution, "server") {
				return true
			}
		}
		if callID == "" {
			allKnown = false
			return false
		}
		callType, ok := knownCalls[callID]
		if !ok || !toolOutputTypeMatchesCallType(itemType, callType) {
			allKnown = false
			return false
		}
		return true
	})
	return allKnown
}

func inputSatisfiesPendingToolCalls(input gjson.Result, pendingCallIDs []string) bool {
	if len(pendingCallIDs) == 0 {
		return true
	}
	if !input.IsArray() {
		return false
	}
	outputs := make(map[string]struct{}, len(pendingCallIDs))
	input.ForEach(func(_, item gjson.Result) bool {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call_output", "custom_tool_call_output":
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID != "" {
				outputs[callID] = struct{}{}
			}
		}
		return true
	})
	for _, callID := range pendingCallIDs {
		callID = strings.TrimSpace(callID)
		if callID == "" {
			continue
		}
		if _, ok := outputs[callID]; !ok {
			return false
		}
	}
	return true
}

func shouldReplaceWebsocketTranscript(rawJSON []byte, nextInput gjson.Result) bool {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return false
	}
	previousResponseID := gjson.GetBytes(rawJSON, "previous_response_id")
	if strings.TrimSpace(previousResponseID.String()) != "" {
		return false
	}
	if !nextInput.Exists() || !nextInput.IsArray() {
		return false
	}
	if requestType == wsRequestTypeCreate && !previousResponseID.Exists() && inputHasCodexLocalCompactionSummary(nextInput) {
		return true
	}

	replace := false
	nextInput.ForEach(func(_, item gjson.Result) bool {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call":
			replace = true
			return false
		case "message":
			role := strings.TrimSpace(item.Get("role").String())
			if role == "assistant" {
				replace = true
				return false
			}
		}
		return true
	})

	return replace
}

// inputContainsFullTranscript identifies compact replay markers. Such input is
// already a canonical transcript for Codex; merging it with stale local history
// duplicates function-call state. Other upstreams cannot consume these items,
// so callers remove the markers and replay their locally accumulated history.
func inputContainsFullTranscript(input gjson.Result) bool {
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "compaction", "compaction_summary", "context_compaction":
			found = true
			return false
		default:
			return true
		}
	})
	return found
}

func inputWithoutCompactionItems(input gjson.Result) string {
	if !input.IsArray() {
		return "[]"
	}
	filtered := make([]string, 0, int(input.Get("#").Int()))
	input.ForEach(func(_, item gjson.Result) bool {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "compaction", "compaction_summary", "context_compaction":
			return true
		default:
			filtered = append(filtered, item.Raw)
			return true
		}
	})
	return joinJSONArrayRaw(filtered)
}

func stripResponsesWebsocketCompactionItems(request []byte) []byte {
	input := gjson.GetBytes(request, "input")
	if !inputContainsFullTranscript(input) {
		return request
	}
	updated, err := sjson.SetRawBytes(request, "input", []byte(inputWithoutCompactionItems(input)))
	if err != nil {
		return request
	}
	return updated
}

func inputHasCodexLocalCompactionSummary(input gjson.Result) bool {
	if !input.IsArray() {
		return false
	}

	valid := true
	hasSummary := false
	index := 0
	input.ForEach(func(_, item gjson.Result) bool {
		currentIndex := index
		index++
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType == "additional_tools" {
			tools := item.Get("tools")
			if currentIndex != 0 || strings.TrimSpace(item.Get("role").String()) != "developer" || !tools.IsArray() {
				valid = false
				return false
			}
			tools.ForEach(func(_, tool gjson.Result) bool {
				if !tool.IsObject() || strings.TrimSpace(tool.Get("type").String()) == "" {
					valid = false
					return false
				}
				return true
			})
			return valid
		}
		if itemType != "" && itemType != "message" {
			valid = false
			return false
		}

		role := strings.TrimSpace(item.Get("role").String())
		if role != "user" && role != "developer" {
			valid = false
			return false
		}
		if role == "user" && codexLocalCompactionMessageHasSummary(item) {
			hasSummary = true
		}
		return true
	})
	return valid && hasSummary
}

func codexLocalCompactionMessageHasSummary(message gjson.Result) bool {
	const summaryStart = codexLocalCompactionSummaryPrefix + "\n"

	content := message.Get("content")
	if content.Type == gjson.String {
		return strings.HasPrefix(content.String(), summaryStart)
	}
	if !content.IsArray() {
		return false
	}

	matched := 0
	valid := true
	content.ForEach(func(_, part gjson.Result) bool {
		if strings.TrimSpace(part.Get("type").String()) != "input_text" {
			return true
		}
		text := part.Get("text").String()
		if text == "" {
			return true
		}
		remaining := summaryStart[matched:]
		if len(text) >= len(remaining) {
			if strings.HasPrefix(text, remaining) {
				matched = len(summaryStart)
				return false
			}
			valid = false
			return false
		}
		if !strings.HasPrefix(remaining, text) {
			valid = false
			return false
		}
		matched += len(text)
		return true
	})
	return valid && matched == len(summaryStart)
}

func normalizeResponseTranscriptReplacement(rawJSON []byte, lastRequest []byte) []byte {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = rawJSON
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	normalized = inheritResponsesWebsocketRequestConfiguration(normalized, lastRequest)
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	normalized = codexcommon.NormalizeResponseInputItems(normalized)
	return normalized
}

func dedupeFunctionCallsByCallID(rawArray string) (string, error) {
	rawArray, err := validatedJSONArrayRawString(rawArray)
	if err != nil {
		return "", err
	}

	result := gjson.Parse(rawArray)
	itemCount := int(result.Get("#").Int())
	var seenCallIDs map[string]struct{}
	duplicated := false
	result.ForEach(func(_, item gjson.Result) bool {
		itemType := strings.TrimSpace(item.Get("type").String())
		if isResponsesToolCallType(itemType) {
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID != "" {
				if seenCallIDs == nil {
					seenCallIDs = make(map[string]struct{}, itemCount)
				}
				if _, ok := seenCallIDs[callID]; ok {
					duplicated = true
					return false
				}
				seenCallIDs[callID] = struct{}{}
			}
		}
		return true
	})
	if !duplicated {
		return rawArray, nil
	}

	seenCallIDs = nil
	filtered := make([]string, 0, itemCount)
	result.ForEach(func(_, item gjson.Result) bool {
		itemRaw := strings.TrimSpace(item.Raw)
		if itemRaw == "" {
			return true
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		if isResponsesToolCallType(itemType) {
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID != "" {
				if seenCallIDs == nil {
					seenCallIDs = make(map[string]struct{}, itemCount)
				}
				if _, ok := seenCallIDs[callID]; ok {
					return true
				}
				seenCallIDs[callID] = struct{}{}
			}
		}
		filtered = append(filtered, itemRaw)
		return true
	})
	return joinJSONArrayRaw(filtered), nil
}

func websocketUpstreamSupportsIncrementalInput(attributes map[string]string, metadata map[string]any) bool {
	if len(attributes) > 0 {
		for _, key := range []string{"websockets", "websocket"} {
			if raw := strings.TrimSpace(attributes[key]); raw != "" {
				parsed, errParse := strconv.ParseBool(raw)
				if errParse == nil {
					return parsed
				}
			}
		}
	}
	if len(metadata) == 0 {
		return false
	}
	for _, key := range []string{"websockets", "websocket"} {
		raw, ok := metadata[key]
		if !ok || raw == nil {
			continue
		}
		switch value := raw.(type) {
		case bool:
			return value
		case string:
			parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
			if errParse == nil {
				return parsed
			}
		default:
		}
	}
	return false
}

func responsesWebsocketAuthSupportsIncrementalInput(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	// Keep capability detection aligned with the executors registered by the
	// current runtime. xAI may carry a community/newer `websockets` flag, but
	// this branch registers XAIExecutor (HTTP), not an xAI websocket executor.
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata)
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketPinnedAuthReusable(authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || h == nil || h.AuthManager == nil {
		return false
	}
	auth, ok := h.AuthManager.GetByID(authID)
	if !ok || auth == nil {
		return false
	}
	if auth.Disabled || auth.Unavailable || auth.Status == coreauth.StatusDisabled {
		return false
	}
	return responsesWebsocketAuthSupportsIncrementalInput(auth)
}

func responsesWebsocketModelRoute(modelName string) ([]string, string) {
	resolvedModelName := modelName
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		} else {
			resolvedModelName = resolvedBase
		}
	} else {
		resolvedModelName = util.ResolveAutoModel(modelName)
	}

	parsed := thinking.ParseSuffix(resolvedModelName)
	modelKey := strings.TrimSpace(parsed.ModelName)
	providers := util.GetProviderName(modelKey)
	if len(providers) == 0 && modelKey != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}
	if modelKey == "" {
		modelKey = strings.TrimSpace(resolvedModelName)
	}
	return providers, modelKey
}

func responsesWebsocketProviderMatches(auth *coreauth.Auth, providers []string) bool {
	if auth == nil {
		return false
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	for _, provider := range providers {
		if providerKey == strings.ToLower(strings.TrimSpace(provider)) {
			return true
		}
	}
	return false
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketPinnedAuthReusableForModel(sessionID, authID, modelName, pinnedModelKey string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || h == nil || h.AuthManager == nil {
		return false
	}
	providers, modelKey := responsesWebsocketModelRoute(modelName)
	if len(providers) == 0 || modelKey == "" {
		return false
	}

	if auth, ok := h.AuthManager.GetExecutionSessionAuthByID(sessionID, authID); ok {
		return !auth.Unavailable &&
			responsesWebsocketProviderMatches(auth, providers) &&
			coreauth.AuthAvailableForModel(auth, modelKey, time.Now()) &&
			strings.EqualFold(strings.TrimSpace(pinnedModelKey), modelKey) &&
			responsesWebsocketAuthSupportsIncrementalInput(auth)
	}

	return h.AuthManager.AnyAvailableAuthForModel(providers, modelKey, func(auth *coreauth.Auth) bool {
		return auth != nil && auth.ID == authID && !auth.Unavailable && responsesWebsocketAuthSupportsIncrementalInput(auth)
	})
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsIncrementalInputForModel(modelName string) bool {
	if h == nil || h.AuthManager == nil {
		return false
	}
	providers, modelKey := responsesWebsocketModelRoute(modelName)
	if len(providers) == 0 {
		return false
	}
	return h.AuthManager.AnyAvailableAuthForModel(providers, modelKey, func(auth *coreauth.Auth) bool {
		if auth == nil || auth.Disabled || auth.Unavailable || auth.Status == coreauth.StatusDisabled {
			return false
		}
		return responsesWebsocketAuthSupportsIncrementalInput(auth)
	})
}

func responsesWebsocketAuthSupportsCompactionReplay(auth *coreauth.Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsCompactionReplayForModel(modelName string) bool {
	if h == nil || h.AuthManager == nil {
		return false
	}
	providers, modelKey := responsesWebsocketModelRoute(modelName)
	if len(providers) == 0 || modelKey == "" {
		return false
	}
	for _, provider := range providers {
		if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
			return false
		}
	}
	return h.AuthManager.AnyAvailableAuthForModel(providers, modelKey, func(auth *coreauth.Auth) bool {
		return responsesWebsocketAuthSupportsCompactionReplay(auth)
	})
}

func shouldHandleResponsesWebsocketPrewarmLocally(rawJSON []byte, lastRequest []byte, allowIncrementalInputWithPreviousResponseID bool) bool {
	if allowIncrementalInputWithPreviousResponseID || len(lastRequest) != 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String()) != wsRequestTypeCreate {
		return false
	}
	generateResult := gjson.GetBytes(rawJSON, "generate")
	return generateResult.Exists() && !generateResult.Bool()
}

func writeResponsesWebsocketSyntheticPrewarm(
	c *gin.Context,
	conn *websocket.Conn,
	requestJSON []byte,
	wsTimelineLog *websocketTimelineBuilder,
	sessionID string,
) error {
	payloads, errPayloads := syntheticResponsesWebsocketPrewarmPayloads(requestJSON)
	if errPayloads != nil {
		return errPayloads
	}
	for i := 0; i < len(payloads); i++ {
		markAPIResponseTimestamp(c)
		// log.Infof(
		// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
		// 	sessionID,
		// 	websocket.TextMessage,
		// 	websocketPayloadEventType(payloads[i]),
		// 	websocketPayloadPreview(payloads[i]),
		// )
		if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now()); errWrite != nil {
			log.Warnf(
				"responses websocket: downstream_out write failed id=%s event=%s error=%v",
				sessionID,
				websocketPayloadEventType(payloads[i]),
				errWrite,
			)
			return errWrite
		}
	}
	return nil
}

func syntheticResponsesWebsocketPrewarmPayloads(requestJSON []byte) ([][]byte, error) {
	responseID := "resp_prewarm_" + uuid.NewString()
	createdAt := time.Now().Unix()
	modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String())

	createdPayload := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
	var errSet error
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		createdPayload, errSet = sjson.SetBytes(createdPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	completedPayload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		completedPayload, errSet = sjson.SetBytes(completedPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	return [][]byte{createdPayload, completedPayload}, nil
}

func mergeJSONArrayRaw(existingRaw, appendRaw string) (string, error) {
	existingRaw, err := validatedJSONArrayRawString(existingRaw)
	if err != nil {
		return "", err
	}
	appendRaw, err = validatedJSONArrayRawString(appendRaw)
	if err != nil {
		return "", err
	}
	if existingRaw == "[]" {
		return appendRaw, nil
	}
	if appendRaw == "[]" {
		return existingRaw, nil
	}

	existingBody := strings.TrimSpace(existingRaw[1 : len(existingRaw)-1])
	appendBody := strings.TrimSpace(appendRaw[1 : len(appendRaw)-1])
	if existingBody == "" {
		return appendRaw, nil
	}
	if appendBody == "" {
		return existingRaw, nil
	}

	var builder strings.Builder
	builder.Grow(len(existingBody) + len(appendBody) + 3)
	builder.WriteByte('[')
	builder.WriteString(existingBody)
	builder.WriteByte(',')
	builder.WriteString(appendBody)
	builder.WriteByte(']')
	return builder.String(), nil
}

func mergeJSONArrayRawTrusted(existingRaw, appendRaw string) string {
	existingRaw = trustedJSONArrayRawStringFromString(existingRaw)
	appendRaw = trustedJSONArrayRawStringFromString(appendRaw)
	if existingRaw == "[]" {
		return appendRaw
	}
	if appendRaw == "[]" {
		return existingRaw
	}

	existingBody := strings.TrimSpace(existingRaw[1 : len(existingRaw)-1])
	appendBody := strings.TrimSpace(appendRaw[1 : len(appendRaw)-1])
	if existingBody == "" {
		return appendRaw
	}
	if appendBody == "" {
		return existingRaw
	}

	var builder strings.Builder
	builder.Grow(len(existingBody) + len(appendBody) + 3)
	builder.WriteByte('[')
	builder.WriteString(existingBody)
	builder.WriteByte(',')
	builder.WriteString(appendBody)
	builder.WriteByte(']')
	return builder.String()
}

func trustedJSONArrayRawString(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return "[]"
	}
	return string(trimmed)
}

func trustedJSONArrayRawStringFromString(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return "[]"
	}
	return trimmed
}

func normalizeJSONArrayRaw(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "[]"
	}
	if trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return "[]"
	}
	if !gjson.ValidBytes(trimmed) {
		return "[]"
	}
	return string(trimmed)
}

func validatedJSONArrayRawString(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "[]", nil
	}
	if trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return "", fmt.Errorf("expected JSON array")
	}
	if !gjson.Valid(trimmed) {
		return "", fmt.Errorf("invalid JSON array")
	}
	return trimmed, nil
}

func joinJSONArrayRaw(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	size := 2
	for _, item := range items {
		size += len(item) + 1
	}
	var builder strings.Builder
	builder.Grow(size)
	builder.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(item)
	}
	builder.WriteByte(']')
	return builder.String()
}

type responsesWebsocketForwardOptions struct {
	allowFullTranscriptRetry bool
	bufferRetryPrelude       bool
	retryCredentialFailover  bool
	retryState               *responsesWebsocketForwardRetryState
}

// responsesWebsocketForwardRetryState keeps a downstream response coherent
// when an upstream previous_response_id fails after response.created (or
// response.in_progress) has already crossed the 50 ms latency boundary. The
// full-transcript retry gets a new upstream response ID and restarts sequence
// numbers, so replacement events are remapped onto the lifecycle prelude the
// client has already observed.
type responsesWebsocketForwardRetryState struct {
	retrying                bool
	emittedPreludeTypes     map[string]struct{}
	emittedSubstantive      bool
	visibleResponseID       string
	retryUpstreamResponseID string
	lastEmittedSequence     int64
	hasLastEmittedSequence  bool
	retrySequenceOffset     int64
	retrySequenceOffsetSet  bool
	terminalError           *interfaces.ErrorMessage
	pendingToolCallIDs      map[string]struct{}
}

func newResponsesWebsocketForwardRetryState() *responsesWebsocketForwardRetryState {
	return &responsesWebsocketForwardRetryState{
		emittedPreludeTypes: make(map[string]struct{}, 2),
		pendingToolCallIDs:  make(map[string]struct{}),
	}
}

func (s *responsesWebsocketForwardRetryState) beginRetry() {
	if s == nil {
		return
	}
	s.retrying = true
	s.retryUpstreamResponseID = ""
	s.retrySequenceOffset = 0
	s.retrySequenceOffsetSet = false
}

func responsesWebsocketIsRetryPreludeEvent(eventType string) bool {
	switch eventType {
	case "response.created", "response.in_progress":
		return true
	default:
		return false
	}
}

func responsesWebsocketPayloadResponseID(payload []byte) string {
	if responseID := strings.TrimSpace(gjson.GetBytes(payload, "response.id").String()); responseID != "" {
		return responseID
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "response_id").String())
}

func responsesWebsocketPayloadSequenceNumber(payload []byte) (int64, bool) {
	sequence := gjson.GetBytes(payload, "sequence_number")
	if !sequence.Exists() || sequence.Type != gjson.Number {
		return 0, false
	}
	return sequence.Int(), true
}

func (s *responsesWebsocketForwardRetryState) canRetryAfterPayload(emittedPayload bool) bool {
	if !emittedPayload {
		return true
	}
	return s != nil && !s.emittedSubstantive && len(s.emittedPreludeTypes) > 0
}

func (s *responsesWebsocketForwardRetryState) observeForwarded(payload []byte, eventType string) {
	if s == nil {
		return
	}
	if responseID := responsesWebsocketPayloadResponseID(payload); s.visibleResponseID == "" && responseID != "" {
		s.visibleResponseID = responseID
	}
	if sequence, ok := responsesWebsocketPayloadSequenceNumber(payload); ok {
		if !s.hasLastEmittedSequence || sequence > s.lastEmittedSequence {
			s.lastEmittedSequence = sequence
			s.hasLastEmittedSequence = true
		}
	}
	if responsesWebsocketIsRetryPreludeEvent(eventType) && !s.emittedSubstantive {
		if s.emittedPreludeTypes == nil {
			s.emittedPreludeTypes = make(map[string]struct{}, 2)
		}
		s.emittedPreludeTypes[eventType] = struct{}{}
		return
	}
	s.emittedSubstantive = true
}

func (s *responsesWebsocketForwardRetryState) prepareRetryPayload(payload []byte, eventType string) ([]byte, bool) {
	if s == nil || !s.retrying || len(payload) == 0 || eventType == wsEventTypeError {
		return payload, false
	}

	upstreamResponseID := responsesWebsocketPayloadResponseID(payload)
	if upstreamResponseID != "" && s.retryUpstreamResponseID == "" {
		s.retryUpstreamResponseID = upstreamResponseID
	}
	if s.visibleResponseID == "" && upstreamResponseID != "" {
		s.visibleResponseID = upstreamResponseID
	}

	if responsesWebsocketIsRetryPreludeEvent(eventType) {
		if _, alreadyEmitted := s.emittedPreludeTypes[eventType]; alreadyEmitted {
			return nil, true
		}
	}

	out := payload
	if s.visibleResponseID != "" && s.retryUpstreamResponseID != "" && s.visibleResponseID != s.retryUpstreamResponseID {
		if responseID := gjson.GetBytes(out, "response.id"); responseID.Exists() && strings.TrimSpace(responseID.String()) == s.retryUpstreamResponseID {
			if remapped, errSet := sjson.SetBytes(out, "response.id", s.visibleResponseID); errSet == nil {
				out = remapped
			}
		}
		if responseID := gjson.GetBytes(out, "response_id"); responseID.Exists() && strings.TrimSpace(responseID.String()) == s.retryUpstreamResponseID {
			if remapped, errSet := sjson.SetBytes(out, "response_id", s.visibleResponseID); errSet == nil {
				out = remapped
			}
		}
	}

	if sequence, ok := responsesWebsocketPayloadSequenceNumber(out); ok {
		if !s.retrySequenceOffsetSet {
			if s.hasLastEmittedSequence && sequence <= s.lastEmittedSequence {
				s.retrySequenceOffset = s.lastEmittedSequence + 1 - sequence
			}
			s.retrySequenceOffsetSet = true
		}
		if s.retrySequenceOffset != 0 {
			if remapped, errSet := sjson.SetBytes(out, "sequence_number", sequence+s.retrySequenceOffset); errSet == nil {
				out = remapped
			}
		}
	}
	return out, false
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsTimelineLog *websocketTimelineBuilder,
	sessionID string,
	downstreamSessionKey string,
	retryCredentialFailoverWithFullTranscript bool,
	toolCallCaches ...*websocketToolOutputCache,
) ([]byte, string, bool, error) {
	var toolCallCache *websocketToolOutputCache
	if len(toolCallCaches) > 0 {
		toolCallCache = toolCallCaches[0]
	}
	return h.forwardResponsesWebsocketWithOptions(
		c,
		conn,
		cancel,
		data,
		errs,
		wsTimelineLog,
		sessionID,
		downstreamSessionKey,
		responsesWebsocketForwardOptions{
			allowFullTranscriptRetry: true,
			bufferRetryPrelude:       retryCredentialFailoverWithFullTranscript,
			retryCredentialFailover:  retryCredentialFailoverWithFullTranscript,
		},
		toolCallCache,
	)
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocketWithOptions(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsTimelineLog *websocketTimelineBuilder,
	sessionID string,
	downstreamSessionKey string,
	options responsesWebsocketForwardOptions,
	toolCallCache *websocketToolOutputCache,
) ([]byte, string, bool, error) {
	completed := false
	completedOutput := []byte("[]")
	completedResponseID := ""
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	if options.retryState != nil && options.retryState.pendingToolCallIDs == nil {
		options.retryState.pendingToolCallIDs = make(map[string]struct{})
	}
	emittedPayload := false
	noticeFilter := newResponsesNoticeFilter()
	requestCtx := c.Request.Context()
	type pendingRetryPreludePayload struct {
		payload   []byte
		eventType string
	}
	const (
		maxPendingRetryPreludeEvents = 1
		maxPendingRetryPreludeBytes  = 1 << 20
	)
	pendingRetryPrelude := make([]pendingRetryPreludePayload, 0, maxPendingRetryPreludeEvents)
	pendingRetryPreludeBytes := 0
	var pendingRetryPreludeTimer *time.Timer
	var pendingRetryPreludeTimerC <-chan time.Time
	stopPendingRetryPreludeTimer := func() {
		if pendingRetryPreludeTimer == nil {
			return
		}
		if !pendingRetryPreludeTimer.Stop() {
			select {
			case <-pendingRetryPreludeTimer.C:
			default:
			}
		}
		pendingRetryPreludeTimer = nil
		pendingRetryPreludeTimerC = nil
	}
	defer stopPendingRetryPreludeTimer()
	writeForwardedPayload := func(payload []byte, eventType string, timestamp time.Time) error {
		markAPIResponseTimestampAt(c, timestamp)
		if errWrite := writeResponsesWebsocketPayloadWithEventType(conn, wsTimelineLog, payload, timestamp, eventType); errWrite != nil {
			log.Warnf(
				"responses websocket: downstream_out write failed id=%s event=%s error=%v",
				sessionID,
				websocketPayloadEventTypeName(eventType),
				errWrite,
			)
			return errWrite
		}
		if options.retryState != nil {
			options.retryState.observeForwarded(payload, eventType)
		}
		emittedPayload = true
		return nil
	}
	flushPendingRetryPrelude := func() error {
		stopPendingRetryPreludeTimer()
		for _, pending := range pendingRetryPrelude {
			if errWrite := writeForwardedPayload(pending.payload, pending.eventType, time.Now()); errWrite != nil {
				pendingRetryPrelude = pendingRetryPrelude[:0]
				pendingRetryPreludeBytes = 0
				return errWrite
			}
		}
		pendingRetryPrelude = pendingRetryPrelude[:0]
		pendingRetryPreludeBytes = 0
		return nil
	}
	bufferRetryPrelude := func(payload []byte, eventType string) bool {
		if !options.bufferRetryPrelude || emittedPayload {
			return false
		}
		// Buffer only response.created for a short, bounded grace period. The next
		// event remains the normal acceptance boundary, while the timer prevents a
		// long upstream thinking gap from hiding all lifecycle activity downstream.
		if eventType != "response.created" {
			return false
		}
		if len(pendingRetryPrelude) >= maxPendingRetryPreludeEvents ||
			pendingRetryPreludeBytes+len(payload) > maxPendingRetryPreludeBytes {
			return false
		}
		pendingRetryPrelude = append(pendingRetryPrelude, pendingRetryPreludePayload{
			payload:   bytes.Clone(payload),
			eventType: eventType,
		})
		pendingRetryPreludeBytes += len(payload)
		if pendingRetryPreludeTimer == nil {
			pendingRetryPreludeTimer = time.NewTimer(responsesWebsocketRetryPreludeMaxDelay)
			pendingRetryPreludeTimerC = pendingRetryPreludeTimer.C
		}
		return true
	}
	failNilStreamChannels := func() error {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errResponsesWebsocketNilStreamChannels}
		recordResponsesWebsocketAPIResponseError(h, c, errMsg)
		_, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
		cancel(errResponsesWebsocketNilStreamChannels)
		if errWrite != nil {
			return errWrite
		}
		return errResponsesWebsocketNilStreamChannels
	}
	shouldRetryWithFullTranscript := func(errMsg *interfaces.ErrorMessage) bool {
		return options.allowFullTranscriptRetry &&
			errMsg != nil &&
			options.retryState.canRetryAfterPayload(emittedPayload) &&
			(responsesWebsocketShouldRetryFullTranscript(errMsg) ||
				(options.retryCredentialFailover && responsesWebsocketIsCredentialFailoverFailure(errMsg.Error)))
	}
	forwardTerminalError := func(errMsg *interfaces.ErrorMessage) error {
		if errMsg != nil {
			if options.retryState != nil {
				options.retryState.terminalError = errMsg
			}
			recordResponsesWebsocketAPIResponseError(h, c, errMsg)
			if errFlush := flushPendingRetryPrelude(); errFlush != nil {
				cancel(errFlush)
				return errFlush
			}
			errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				sessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					sessionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				if errMsg.Error != nil {
					cancel(handlers.ErrorMessageCause(errMsg))
				} else {
					cancel(errWrite)
				}
				return errWrite
			}
			cancel(handlers.ErrorMessageCause(errMsg))
			return nil
		}
		cancel(nil)
		return nil
	}
	if data == nil && errs == nil {
		return completedOutput, completedResponseID, completed, failNilStreamChannels()
	}

	for {
		select {
		case <-requestCtx.Done():
			cancel(requestCtx.Err())
			return completedOutput, completedResponseID, completed, requestCtx.Err()
		case <-pendingRetryPreludeTimerC:
			if errFlush := flushPendingRetryPrelude(); errFlush != nil {
				cancel(errFlush)
				return completedOutput, completedResponseID, completed, errFlush
			}
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				if data == nil {
					return completedOutput, completedResponseID, completed, failNilStreamChannels()
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			if shouldRetryWithFullTranscript(errMsg) {
				if errMsg.Error != nil {
					cancel(handlers.ErrorMessageCause(errMsg))
				} else {
					cancel(errResponsesWebsocketRetryFullTranscript)
				}
				return completedOutput, completedResponseID, completed, errResponsesWebsocketRetryFullTranscript
			}
			return completedOutput, completedResponseID, completed, forwardTerminalError(errMsg)
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					if errMsg, okPendingErr := handlers.PendingStreamError(errs); okPendingErr {
						if shouldRetryWithFullTranscript(errMsg) {
							if errMsg.Error != nil {
								cancel(handlers.ErrorMessageCause(errMsg))
							} else {
								cancel(errResponsesWebsocketRetryFullTranscript)
							}
							return completedOutput, completedResponseID, completed, errResponsesWebsocketRetryFullTranscript
						}
						return completedOutput, completedResponseID, completed, forwardTerminalError(errMsg)
					}
					errMsg := &interfaces.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					return completedOutput, completedResponseID, completed, forwardTerminalError(errMsg)
				}
				cancel(nil)
				return completedOutput, completedResponseID, completed, nil
			}

			var forwardErr error
			stopForward := false
			websocketJSONPayloadsFromChunkEach(chunk, func(payload []byte) bool {
				var payloadBuffer [4][]byte
				filteredPayloads := payloadBuffer[:0]
				if noticeFilter != nil {
					filteredPayloads = noticeFilter.FilterPayloadsInto(payload, filteredPayloads)
				} else {
					filteredPayloads = append(filteredPayloads, payload)
				}
				for _, filteredPayload := range filteredPayloads {
					if len(filteredPayload) == 0 {
						continue
					}
					eventType := websocketPayloadEventTypeValue(filteredPayload)
					if options.retryState != nil && options.retryState.retrying {
						preparedPayload, suppress := options.retryState.prepareRetryPayload(filteredPayload, eventType)
						if suppress {
							continue
						}
						filteredPayload = preparedPayload
						eventType = websocketPayloadEventTypeValue(filteredPayload)
					}
					now := time.Now()
					if options.allowFullTranscriptRetry &&
						options.retryState.canRetryAfterPayload(emittedPayload) &&
						responsesWebsocketPayloadShouldRetryFullTranscript(filteredPayload) {
						cancel(errResponsesWebsocketRetryFullTranscript)
						forwardErr = errResponsesWebsocketRetryFullTranscript
						stopForward = true
						return false
					}
					if eventType == wsEventTypeError {
						payloadErrMsg := responsesWebsocketErrorMessageFromPayload(filteredPayload)
						if options.retryState != nil {
							options.retryState.terminalError = payloadErrMsg
						}
						recordResponsesWebsocketAPIResponseError(h, c, payloadErrMsg)
						if errWrite := flushPendingRetryPrelude(); errWrite != nil {
							cancel(errWrite)
							forwardErr = errWrite
							stopForward = true
							return false
						}
						if errWrite := writeForwardedPayload(filteredPayload, eventType, now); errWrite != nil {
							cancel(errWrite)
							forwardErr = errWrite
							stopForward = true
							return false
						}
						cancel(nil)
						stopForward = true
						return false
					}
					collectResponsesWebsocketOutputItem(filteredPayload, outputItemsByIndex, &outputItemsFallback)
					if eventType == wsEventTypeCompleted {
						filteredPayload = restoreResponsesWebsocketCompletionOutput(filteredPayload, outputItemsByIndex, outputItemsFallback)
					}
					if options.retryState != nil {
						recordPendingToolCallIDsFromPayload(options.retryState.pendingToolCallIDs, filteredPayload)
					}
					if bufferRetryPrelude(filteredPayload, eventType) {
						continue
					}
					if errWrite := flushPendingRetryPrelude(); errWrite != nil {
						cancel(errWrite)
						forwardErr = errWrite
						stopForward = true
						return false
					}
					recordResponsesWebsocketToolCallsFromPayloadWithCacheAndType(toolCallCache, downstreamSessionKey, eventType, filteredPayload)
					if eventType == wsEventTypeCompleted {
						completed = true
						completedOutput = responseCompletedOutputFromPayloadWithItems(filteredPayload, outputItemsByIndex, outputItemsFallback)
						completedResponseID = responseCompletedIDFromPayload(filteredPayload)
					}
					// log.Infof(
					// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
					// 	sessionID,
					// 	websocket.TextMessage,
					// 	websocketPayloadEventType(payloads[i]),
					// 	websocketPayloadPreview(payloads[i]),
					// )
					if errWrite := writeForwardedPayload(filteredPayload, eventType, now); errWrite != nil {
						cancel(errWrite)
						forwardErr = errWrite
						stopForward = true
						return false
					}
				}
				return true
			})
			if stopForward {
				return completedOutput, completedResponseID, completed, forwardErr
			}
		}
	}
}

func responseCompletedIDFromPayload(payload []byte) string {
	return strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
}

func responseCompletedOutputFromPayload(payload []byte) []byte {
	return responseCompletedOutputFromPayloadWithItems(payload, nil, nil)
}

func collectResponsesWebsocketOutputItem(payload []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback *[][]byte) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() {
		return
	}
	outputIndex := gjson.GetBytes(payload, "output_index")
	if outputIndex.Exists() {
		outputItemsByIndex[outputIndex.Int()] = bytes.Clone([]byte(item.Raw))
		return
	}
	if outputItemsFallback != nil {
		*outputItemsFallback = append(*outputItemsFallback, bytes.Clone([]byte(item.Raw)))
	}
}

func restoreResponsesWebsocketCompletionOutput(payload []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback [][]byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() && len(output.Array()) > 0 {
		return payload
	}
	if len(outputItemsByIndex) == 0 && len(outputItemsFallback) == 0 {
		return payload
	}
	restored, err := sjson.SetRawBytes(payload, "response.output", responseCompletedOutputFromPayloadWithItems(payload, outputItemsByIndex, outputItemsFallback))
	if err != nil {
		return payload
	}
	return restored
}

func responseCompletedOutputFromPayloadWithItems(payload []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback [][]byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() && len(output.Array()) > 0 {
		return bytes.Clone([]byte(output.Raw))
	}
	if len(outputItemsByIndex) == 0 && len(outputItemsFallback) == 0 {
		return []byte("[]")
	}

	indexes := make([]int64, 0, len(outputItemsByIndex))
	for index := range outputItemsByIndex {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	items := make([]json.RawMessage, 0, len(outputItemsByIndex)+len(outputItemsFallback))
	for _, index := range indexes {
		items = append(items, json.RawMessage(outputItemsByIndex[index]))
	}
	for _, item := range outputItemsFallback {
		items = append(items, json.RawMessage(item))
	}
	marshaled, err := json.Marshal(items)
	if err != nil {
		return []byte("[]")
	}
	return marshaled
}

func recordPendingToolCallIDsFromPayload(pending map[string]struct{}, payload []byte) {
	if pending == nil || len(payload) == 0 {
		return
	}
	updatePendingToolCallIDsFromItem(pending, gjson.GetBytes(payload, "item"))
	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			updatePendingToolCallIDsFromItem(pending, item)
			return true
		})
	}
}

func updatePendingToolCallIDsFromItem(pending map[string]struct{}, item gjson.Result) {
	if pending == nil || !item.Exists() {
		return
	}
	switch strings.TrimSpace(item.Get("type").String()) {
	case "function_call", "custom_tool_call":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID != "" {
			pending[callID] = struct{}{}
		}
	case "function_call_output", "custom_tool_call_output":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID != "" {
			delete(pending, callID)
		}
	}
}

func sortedStringSet(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func responsesWebsocketErrorMessageFromPayload(payload []byte) *interfaces.ErrorMessage {
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "error.status").Int())
	}
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "error.status_code").Int())
	}
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	errText := strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
	if errText == "" {
		errText = strings.TrimSpace(gjson.GetBytes(payload, "message").String())
	}
	if errText == "" {
		errText = strings.TrimSpace(string(payload))
	}
	if errText == "" {
		errText = http.StatusText(status)
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", errText)}
}

func shouldReleaseResponsesWebsocketPinnedAuth(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	status := errMsg.StatusCode
	if status <= 0 && errMsg.Error != nil {
		if statusError, ok := errMsg.Error.(interface{ StatusCode() int }); ok && statusError != nil {
			status = statusError.StatusCode()
		}
	}
	switch status {
	case http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	if errMsg.Error == nil {
		return false
	}
	message := strings.ToLower(errMsg.Error.Error())
	return strings.Contains(message, "stream closed before response.completed") ||
		strings.Contains(message, "previous_response_not_found") ||
		strings.Contains(message, "ws_failed") ||
		strings.Contains(message, "upstream stream closed before first payload") ||
		strings.Contains(message, "empty_stream")
}

func responsesWebsocketShouldRetryFullTranscript(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil || errMsg.Error == nil {
		return false
	}
	errText := strings.TrimSpace(errMsg.Error.Error())
	if errText == "" {
		return false
	}
	if gjson.Valid(errText) {
		body := []byte(errText)
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()), "previous_response_not_found") {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.param").String()), "previous_response_id") {
			return true
		}
		errText = strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	}
	if responsesWebsocketPreviousResponseNotFoundText(errText) {
		return true
	}
	// Auth-manager and relay layers can wrap a provider HTTP 400 in a generic
	// 5xx ErrorMessage while retaining the authoritative upstream text. The
	// previous-response classification above is specific enough to recover even
	// when that outer status is lossy; keep the status guard for broader tool-call
	// matching below.
	if errMsg.StatusCode > 0 && errMsg.StatusCode != http.StatusBadRequest {
		return false
	}
	return responsesContainsASCIIFold(errText, "no tool call found") &&
		responsesContainsASCIIFold(errText, "call output")
}

func responsesWebsocketIsCredentialFailoverFailure(err error) bool {
	if err == nil {
		return false
	}
	var failure coreauth.CredentialFailoverFailure
	if errors.As(err, &failure) && failure != nil {
		return failure.IsCredentialFailoverFailure()
	}
	return false
}

func responsesWebsocketPayloadIsError(payload []byte) bool {
	if !gjson.ValidBytes(bytes.TrimSpace(payload)) {
		return false
	}
	return websocketPayloadEventTypeValue(payload) == wsEventTypeError
}

func responsesWebsocketPayloadShouldRetryFullTranscript(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	errorPath := "error"
	switch websocketPayloadEventTypeValue(payload) {
	case wsEventTypeError:
	case "response.failed":
		errorPath = "response.error"
	default:
		return false
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, errorPath+".code").String()), "previous_response_not_found") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, errorPath+".param").String()), "previous_response_id") {
		return true
	}
	errText := strings.TrimSpace(gjson.GetBytes(payload, errorPath+".message").String())
	if errText == "" {
		errText = strings.TrimSpace(gjson.GetBytes(payload, "message").String())
	}
	if responsesWebsocketPreviousResponseNotFoundText(errText) {
		return true
	}
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, errorPath+".status").Int())
	}
	if status > 0 && status != http.StatusBadRequest {
		return false
	}
	return responsesContainsASCIIFold(errText, "no tool call found") &&
		responsesContainsASCIIFold(errText, "call output")
}

func responsesWebsocketPreviousResponseNotFoundText(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || !responsesContainsASCIIFold(text, "not found") {
		return false
	}
	if responsesContainsASCIIFold(text, "previous_response_id") {
		return true
	}
	return responsesContainsASCIIFold(text, "previous response")
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	return websocketJSONPayloadsFromChunkInto(chunk, nil)
}

func websocketJSONPayloadsFromChunkInto(chunk []byte, payloads [][]byte) [][]byte {
	payloads = payloads[:0]
	websocketJSONPayloadsFromChunkEach(chunk, func(payload []byte) bool {
		payloads = append(payloads, payload)
		return true
	})
	return payloads
}

func websocketJSONPayloadsFromChunkEach(chunk []byte, fn func([]byte) bool) bool {
	if fn == nil {
		return true
	}
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 || bytes.Equal(trimmed, wsDoneMarkerBytes) {
		return true
	}
	if bytes.HasPrefix(trimmed, wsDataPrefixBytes) {
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if len(data) > 0 &&
			!bytes.Equal(data, wsDoneMarkerBytes) &&
			!bytes.ContainsAny(data, "\r\n") &&
			json.Valid(data) {
			return fn(data)
		}
	} else if !bytes.ContainsAny(trimmed, "\r\n") && json.Valid(trimmed) {
		return fn(trimmed)
	}

	emitted := false
	remaining := chunk
	for len(remaining) > 0 {
		line := remaining
		if idx := bytes.IndexByte(remaining, '\n'); idx >= 0 {
			line = remaining[:idx]
			remaining = remaining[idx+1:]
		} else {
			remaining = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, wsEventPrefixBytes) {
			continue
		}
		if bytes.HasPrefix(line, wsDataPrefixBytes) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, wsDoneMarkerBytes) {
			continue
		}
		if json.Valid(line) {
			emitted = true
			if !fn(line) {
				return false
			}
		}
	}

	if emitted {
		return true
	}

	if bytes.HasPrefix(trimmed, wsDataPrefixBytes) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, wsDoneMarkerBytes) && json.Valid(trimmed) {
		return fn(trimmed)
	}
	return true
}

func writeResponsesWebsocketError(conn *websocket.Conn, wsTimelineLog *websocketTimelineBuilder, errMsg *interfaces.ErrorMessage) ([]byte, error) {
	status := http.StatusInternalServerError
	errText := http.StatusText(status)
	if errMsg != nil {
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
			errText = http.StatusText(status)
		}
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
	}

	body := handlers.BuildErrorResponseBody(status, errText)
	payload := []byte(`{}`)
	var errSet error
	payload, errSet = sjson.SetBytes(payload, "type", wsEventTypeError)
	if errSet != nil {
		return nil, errSet
	}
	payload, errSet = sjson.SetBytes(payload, "status", status)
	if errSet != nil {
		return nil, errSet
	}

	if errMsg != nil && errMsg.Addon != nil {
		headers := []byte(`{}`)
		hasHeaders := false
		for key, values := range handlers.FilterUpstreamHeaders(errMsg.Addon) {
			if len(values) == 0 {
				continue
			}
			headerPath := strings.ReplaceAll(strings.ReplaceAll(key, `\\`, `\\\\`), ".", `\\.`)
			headers, errSet = sjson.SetBytes(headers, headerPath, values[0])
			if errSet != nil {
				return nil, errSet
			}
			hasHeaders = true
		}
		if hasHeaders {
			payload, errSet = sjson.SetRawBytes(payload, "headers", headers)
			if errSet != nil {
				return nil, errSet
			}
		}
	}

	if len(body) > 0 && json.Valid(body) {
		errorNode := gjson.GetBytes(body, "error")
		if errorNode.Exists() {
			payload, errSet = sjson.SetRawBytes(payload, "error", []byte(errorNode.Raw))
		} else {
			payload, errSet = sjson.SetRawBytes(payload, "error", body)
		}
		if errSet != nil {
			return nil, errSet
		}
	}

	if !gjson.GetBytes(payload, "error").Exists() {
		payload, errSet = sjson.SetBytes(payload, "error.type", "server_error")
		if errSet != nil {
			return nil, errSet
		}
		payload, errSet = sjson.SetBytes(payload, "error.message", errText)
		if errSet != nil {
			return nil, errSet
		}
	}

	return payload, writeResponsesWebsocketPayload(conn, wsTimelineLog, payload, time.Now())
}

func appendWebsocketEvent(builder *websocketTimelineBuilder, eventType string, payload []byte) {
	appendWebsocketEventWithPayloadType(builder, eventType, payload, "")
}

func appendWebsocketEventWithPayloadType(builder *websocketTimelineBuilder, eventType string, payload []byte, payloadType string) {
	if builder == nil {
		return
	}
	if !websocketTimelineShouldRecordWithPayloadType(builder, eventType, payload, payloadType) {
		return
	}
	if websocketTimelineTruncated(builder) {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	trimmedPayload = util.RedactSensitiveLogBytes(trimmedPayload)
	if builder.Len() > 0 {
		appendWebsocketTimelineText(builder, "\n")
	}
	appendWebsocketTimelineText(builder, "websocket.")
	appendWebsocketTimelineText(builder, eventType)
	appendWebsocketTimelineText(builder, "\n")
	appendWebsocketTimelineBytes(builder, trimmedPayload)
	appendWebsocketTimelineText(builder, "\n")
}

func websocketPayloadEventType(payload []byte) string {
	return websocketPayloadEventTypeName(websocketPayloadEventTypeValue(payload))
}

func websocketPayloadEventTypeName(eventType string) string {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return "-"
	}
	return eventType
}

func websocketPayloadEventTypeValue(payload []byte) string {
	if eventType, ok := websocketPayloadTopLevelType(payload); ok {
		return strings.TrimSpace(eventType)
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "type").String())
}

func websocketPayloadTopLevelType(payload []byte) (string, bool) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return "", false
	}
	if eventType, ok := websocketCompactKnownPayloadEventType(payload); ok {
		return eventType, true
	}

	depth := 0
	for i := 0; i < len(payload); {
		switch payload[i] {
		case '{', '[':
			depth++
			i++
		case '}', ']':
			depth--
			if depth < 0 {
				return "", false
			}
			i++
		case '"':
			keyStart := i + 1
			keyEnd, keyEscaped := websocketScanJSONString(payload, keyStart)
			if keyEnd < 0 {
				return "", false
			}
			next := websocketSkipJSONSpaces(payload, keyEnd+1)
			if depth == 1 && next < len(payload) && payload[next] == ':' {
				if !keyEscaped && bytes.Equal(payload[keyStart:keyEnd], wsTypeKeyBytes) {
					valueStart := websocketSkipJSONSpaces(payload, next+1)
					if valueStart >= len(payload) || payload[valueStart] != '"' {
						return "", false
					}
					valueEnd, valueEscaped := websocketScanJSONString(payload, valueStart+1)
					if valueEnd < 0 || valueEscaped {
						return "", false
					}
					value := payload[valueStart+1 : valueEnd]
					if eventType, ok := websocketKnownPayloadEventType(value); ok {
						return eventType, true
					}
					return string(value), true
				}
			}
			i = keyEnd + 1
		default:
			i++
		}
	}
	return "", false
}

func websocketCompactKnownPayloadEventType(payload []byte) (string, bool) {
	if len(payload) <= wsCompactResponseTypeKindOffset || payload[wsCompactResponseTypeKindOffset] != 'o' {
		return "", false
	}
	switch {
	case websocketHasCompactJSONFieldPrefix(payload, wsCompactEventTypeOutputTextDelta):
		return "response.output_text.delta", true
	case websocketHasCompactJSONFieldPrefix(payload, wsCompactEventTypeOutputTextDone):
		return "response.output_text.done", true
	case websocketHasCompactJSONFieldPrefix(payload, wsCompactEventTypeOutputItemAdded):
		return "response.output_item.added", true
	case websocketHasCompactJSONFieldPrefix(payload, wsCompactEventTypeOutputItemDone):
		return "response.output_item.done", true
	default:
		return "", false
	}
}

func websocketHasCompactJSONFieldPrefix(payload []byte, prefix []byte) bool {
	if !bytes.HasPrefix(payload, prefix) || len(payload) == len(prefix) {
		return false
	}
	switch payload[len(prefix)] {
	case ',', '}', ' ', '\n', '\r', '\t':
		return true
	default:
		return false
	}
}

func websocketKnownPayloadEventType(value []byte) (string, bool) {
	if bytes.Equal(value, wsEventTypeOutputTextDelta) {
		return "response.output_text.delta", true
	}
	if bytes.Equal(value, wsEventTypeOutputTextDone) {
		return "response.output_text.done", true
	}
	if bytes.Equal(value, wsEventTypeOutputItemAdded) {
		return "response.output_item.added", true
	}
	if bytes.Equal(value, wsEventTypeOutputItemDone) {
		return "response.output_item.done", true
	}
	if bytes.Equal(value, wsEventTypeContentPartAdded) {
		return "response.content_part.added", true
	}
	if bytes.Equal(value, wsEventTypeContentPartDone) {
		return "response.content_part.done", true
	}
	if bytes.Equal(value, wsEventTypeCompletedBytes) {
		return wsEventTypeCompleted, true
	}
	if bytes.Equal(value, wsEventTypeResponseCreated) {
		return "response.created", true
	}
	if bytes.Equal(value, wsEventTypeResponseInProgress) {
		return "response.in_progress", true
	}
	if bytes.Equal(value, wsRequestTypeProcessedBytes) {
		return wsRequestTypeProcessed, true
	}
	if bytes.Equal(value, wsEventTypeErrorBytes) {
		return wsEventTypeError, true
	}
	return "", false
}

func websocketScanJSONString(payload []byte, start int) (int, bool) {
	escaped := false
	for i := start; i < len(payload); i++ {
		switch payload[i] {
		case '\\':
			escaped = true
			i++
		case '"':
			return i, escaped
		}
	}
	return -1, escaped
}

func websocketSkipJSONSpaces(payload []byte, start int) int {
	for start < len(payload) {
		switch payload[start] {
		case ' ', '\n', '\r', '\t':
			start++
		default:
			return start
		}
	}
	return start
}

func websocketPayloadPreview(payload []byte) string {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return "<empty>"
	}
	previewText := strings.ReplaceAll(string(trimmedPayload), "\n", "\\n")
	previewText = strings.ReplaceAll(previewText, "\r", "\\r")
	return previewText
}

func setWebsocketTimelineBody(c *gin.Context, body string) {
	setWebsocketBody(c, wsTimelineBodyKey, body)
}

func setWebsocketBody(c *gin.Context, key string, body string) {
	if c == nil {
		return
	}
	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		return
	}
	c.Set(key, []byte(trimmedBody))
}

func writeResponsesWebsocketPayload(conn *websocket.Conn, wsTimelineLog *websocketTimelineBuilder, payload []byte, timestamp time.Time) error {
	return writeResponsesWebsocketPayloadWithEventType(conn, wsTimelineLog, payload, timestamp, "")
}

func writeResponsesWebsocketPayloadWithEventType(conn *websocket.Conn, wsTimelineLog *websocketTimelineBuilder, payload []byte, timestamp time.Time, payloadType string) error {
	appendWebsocketTimelineEventWithPayloadType(wsTimelineLog, "response", payload, timestamp, payloadType)
	if conn == nil {
		return fmt.Errorf("responses websocket: downstream websocket conn is nil")
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	_ = conn.SetWriteDeadline(timestamp.Add(responsesWebsocketWriteTimeout))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func appendWebsocketTimelineDisconnect(builder *websocketTimelineBuilder, err error, timestamp time.Time) {
	if err == nil {
		return
	}
	appendWebsocketTimelineEvent(builder, "disconnect", []byte(err.Error()), timestamp)
}

func recordResponsesWebsocketAPIResponseError(h *OpenAIResponsesAPIHandler, c *gin.Context, errMsg *interfaces.ErrorMessage) {
	if h != nil && c != nil && errMsg != nil {
		h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
	}
	markAPIResponseTimestamp(c)
}

func appendWebsocketTimelineEvent(builder *websocketTimelineBuilder, eventType string, payload []byte, timestamp time.Time) {
	appendWebsocketTimelineEventWithPayloadType(builder, eventType, payload, timestamp, "")
}

func appendWebsocketTimelineEventWithPayloadType(builder *websocketTimelineBuilder, eventType string, payload []byte, timestamp time.Time, payloadType string) {
	if builder == nil {
		return
	}
	if !websocketTimelineShouldRecordWithPayloadType(builder, eventType, payload, payloadType) {
		return
	}
	if websocketTimelineTruncated(builder) {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	trimmedPayload = util.RedactSensitiveLogBytes(trimmedPayload)
	var formattedTimestampBuffer [64]byte
	formattedTimestamp := timestamp.AppendFormat(formattedTimestampBuffer[:0], time.RFC3339Nano)
	eventBytes := len("Timestamp: ") + len(formattedTimestamp) + len("\nEvent: websocket.") + len(eventType) + len(trimmedPayload) + 2
	if builder.Len() > 0 {
		eventBytes++
	}
	remaining := builder.maxBytes - builder.Len()
	if eventBytes > remaining {
		eventBytes = remaining + len(responsesWebsocketTimelineTruncatedMarker)
	}
	if eventBytes > 0 {
		builder.Grow(eventBytes)
	}
	if builder.Len() > 0 {
		appendWebsocketTimelineText(builder, "\n")
	}
	appendWebsocketTimelineText(builder, "Timestamp: ")
	appendWebsocketTimelineBytes(builder, formattedTimestamp)
	appendWebsocketTimelineText(builder, "\n")
	appendWebsocketTimelineText(builder, "Event: websocket.")
	appendWebsocketTimelineText(builder, eventType)
	appendWebsocketTimelineText(builder, "\n")
	appendWebsocketTimelineBytes(builder, trimmedPayload)
	appendWebsocketTimelineText(builder, "\n")
}

func appendWebsocketTimelineText(builder *websocketTimelineBuilder, text string) {
	if builder == nil || text == "" || websocketTimelineTruncated(builder) {
		return
	}
	remaining := builder.maxBytes - builder.Len()
	if remaining <= 0 {
		builder.WriteString(responsesWebsocketTimelineTruncatedMarker)
		return
	}
	if len(text) <= remaining {
		builder.WriteString(text)
		return
	}
	builder.WriteString(text[:remaining])
	builder.WriteString(responsesWebsocketTimelineTruncatedMarker)
}

func appendWebsocketTimelineBytes(builder *websocketTimelineBuilder, data []byte) {
	if builder == nil || len(data) == 0 || websocketTimelineTruncated(builder) {
		return
	}
	remaining := builder.maxBytes - builder.Len()
	if remaining <= 0 {
		builder.WriteString(responsesWebsocketTimelineTruncatedMarker)
		return
	}
	if len(data) <= remaining {
		builder.Write(data)
		return
	}
	builder.Write(data[:remaining])
	builder.WriteString(responsesWebsocketTimelineTruncatedMarker)
}

func websocketTimelineTruncated(builder *websocketTimelineBuilder) bool {
	if builder == nil {
		return false
	}
	return builder.Len() > builder.maxBytes
}

func websocketTimelineShouldRecord(builder *websocketTimelineBuilder, eventType string, payload []byte) bool {
	return websocketTimelineShouldRecordWithPayloadType(builder, eventType, payload, "")
}

func websocketTimelineShouldRecordWithPayloadType(builder *websocketTimelineBuilder, eventType string, payload []byte, payloadType string) bool {
	if builder == nil || !builder.errorOnly {
		return true
	}
	switch strings.TrimSpace(eventType) {
	case "disconnect", "error":
		return true
	}
	payloadType = strings.TrimSpace(payloadType)
	if payloadType == "" {
		payloadType = websocketPayloadEventType(payload)
	}
	if payloadType == wsEventTypeError {
		return true
	}
	return strings.Contains(payloadType, "error") || strings.Contains(payloadType, "failed")
}

func markAPIResponseTimestamp(c *gin.Context) {
	markAPIResponseTimestampAt(c, time.Now())
}

func markAPIResponseTimestampAt(c *gin.Context, timestamp time.Time) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	c.Set("API_RESPONSE_TIMESTAMP", timestamp)
}
