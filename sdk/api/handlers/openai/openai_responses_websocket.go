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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	maxResponsesWebsocketTimelineBytes      = 1 << 20
	maxResponsesWebsocketErrorTimelineBytes = 4 << 10
	maxResponsesWebsocketInboundBytes       = 64 << 20
	responsesWebsocketWriteTimeout          = 30 * time.Second
)

const responsesWebsocketTimelineTruncatedMarker = "\n...[websocket timeline truncated]...\n"

var errResponsesWebsocketNilStreamChannels = errors.New("responses websocket forwarder received nil data and error channels")
var errResponsesWebsocketRetryFullTranscript = errors.New("responses websocket retry with full transcript")

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
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	WriteBufferPool: &responsesWebsocketWriteBufferPool,
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
	originScheme := strings.ToLower(parsedOrigin.Scheme)
	if originScheme != "http" && originScheme != "https" {
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
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http":
		return "80"
	case "https":
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
	var activeDisconnectSessionMu sync.RWMutex
	activeDisconnectSessionID := ""
	setActiveDisconnectSessionID := func(sessionID string) {
		activeDisconnectSessionMu.Lock()
		activeDisconnectSessionID = strings.TrimSpace(sessionID)
		activeDisconnectSessionMu.Unlock()
	}
	getActiveDisconnectSessionID := func() string {
		activeDisconnectSessionMu.RLock()
		defer activeDisconnectSessionMu.RUnlock()
		return activeDisconnectSessionID
	}
	type upstreamDisconnectSubscriber interface {
		UpstreamDisconnectChanIfExists(sessionID string) <-chan error
	}
	type websocketDisconnectSubscription struct {
		done         chan struct{}
		suppressNext atomic.Bool
	}
	var subscribedDisconnectSessions sync.Map
	suppressNextUpstreamDisconnect := func(string) {}
	subscribeUpstreamDisconnect := func(string) {}
	if h != nil && h.AuthManager != nil {
		if exec, ok := h.AuthManager.Executor("codex"); ok && exec != nil {
			if subscriber, ok := exec.(upstreamDisconnectSubscriber); ok && subscriber != nil {
				suppressNextUpstreamDisconnect = func(sessionID string) {
					sessionID = strings.TrimSpace(sessionID)
					if sessionID == "" {
						return
					}
					actual, ok := subscribedDisconnectSessions.Load(sessionID)
					if !ok {
						return
					}
					subscription, _ := actual.(*websocketDisconnectSubscription)
					if subscription == nil {
						return
					}
					subscription.suppressNext.Store(true)
				}
				subscribeUpstreamDisconnect = func(sessionID string) {
					sessionID = strings.TrimSpace(sessionID)
					if sessionID == "" {
						return
					}
					subscription := &websocketDisconnectSubscription{done: make(chan struct{})}
					for {
						actual, loaded := subscribedDisconnectSessions.LoadOrStore(sessionID, subscription)
						if !loaded {
							break
						}
						existing, _ := actual.(*websocketDisconnectSubscription)
						if existing == nil || existing.done == nil {
							return
						}
						select {
						case <-existing.done:
							subscribedDisconnectSessions.Delete(sessionID)
							continue
						default:
							return
						}
					}
					disconnectCh := subscriber.UpstreamDisconnectChanIfExists(sessionID)
					if disconnectCh == nil {
						subscribedDisconnectSessions.Delete(sessionID)
						close(subscription.done)
						return
					}
					go func(subscribedSessionID string, disconnectCh <-chan error) {
						defer close(subscription.done)
						defer subscribedDisconnectSessions.Delete(subscribedSessionID)
						select {
						case <-wsDone:
							return
						case <-disconnectCh:
							if subscription.suppressNext.Swap(false) {
								return
							}
							if getActiveDisconnectSessionID() == subscribedSessionID {
								_ = conn.Close()
							}
						}
					}(sessionID, disconnectCh)
				}
			}
		}
	}
	disconnectSessionID := headerExecutionSessionID
	if disconnectSessionID == "" {
		disconnectSessionID = generatedExecutionSessionID
	}
	setActiveDisconnectSessionID(disconnectSessionID)
	subscribeUpstreamDisconnect(disconnectSessionID)
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
	lastResponseIDIncrementalEligible := false
	pinnedAuthID := ""
	incrementalInputSupportByModel := make(map[string]bool)
	requireFreshFullTranscriptBeforeIncremental := false
	resetIncrementalInputSupportCache := func() {
		incrementalInputSupportByModel = make(map[string]bool)
	}
	clearPinnedAuthIfUnusable := func(sessionID, reason string) bool {
		if pinnedAuthID == "" {
			return false
		}
		if h == nil || h.AuthManager == nil {
			pinnedAuthID = ""
			resetIncrementalInputSupportCache()
			requireFreshFullTranscriptBeforeIncremental = true
			lastResponseIDIncrementalEligible = false
			return true
		}
		if h.responsesWebsocketPinnedAuthReusable(pinnedAuthID) {
			return false
		}
		authID := pinnedAuthID
		pinnedAuthID = ""
		resetIncrementalInputSupportCache()
		requireFreshFullTranscriptBeforeIncremental = true
		lastResponseIDIncrementalEligible = false
		sessionID = strings.TrimSpace(sessionID)
		if sessionID != "" {
			suppressNextUpstreamDisconnect(sessionID)
			setActiveDisconnectSessionID("")
			h.AuthManager.ResetExecutionSession(sessionID)
			setActiveDisconnectSessionID(sessionID)
		}
		log.Infof("responses websocket: unpinned auth id=%s session=%s reason=%s", authID, sessionID, reason)
		return true
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
		pinnedAuthClearedBeforeNormalize := clearPinnedAuthIfUnusable(currentExecutionSessionID, "pre_normalize")
		sessionChanged := activeExecutionSessionID != "" && activeExecutionSessionID != currentExecutionSessionID
		normalizationLastRequest := lastRequest
		normalizationLastResponseOutput := lastResponseOutput
		normalizationLastResponseID := lastResponseID
		if sessionChanged {
			normalizationLastRequest = nil
			normalizationLastResponseOutput = []byte("[]")
			normalizationLastResponseID = ""
			lastResponseIDIncrementalEligible = false
		}

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
			requestModelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
			if requestModelName == "" {
				requestModelName = strings.TrimSpace(gjson.GetBytes(normalizationLastRequest, "model").String())
			}
			allowIncrementalInputWithPreviousResponseID = cachedResponsesWebsocketIncrementalInputSupport(
				incrementalInputSupportByModel,
				requestModelName,
				h.websocketUpstreamSupportsIncrementalInputForModel,
			)
		}

		var requestJSON []byte
		var updatedLastRequest []byte
		var errMsg *interfaces.ErrorMessage
		requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketRequestWithMode(
			payload,
			normalizationLastRequest,
			normalizationLastResponseOutput,
			allowIncrementalInputWithPreviousResponseID,
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
		setActiveDisconnectSessionID(currentExecutionSessionID)
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
		requestStateToCommit := requestJSON

		modelName := gjson.GetBytes(requestJSON, "model").String()
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		cliCtx = handlers.WithExecutionSessionID(cliCtx, currentExecutionSessionID)
		clearPinnedAuthIfUnusable(currentExecutionSessionID, "pre_request")
		requestSelectedIncrementalAuth := false
		if pinnedAuthID != "" {
			requestSelectedIncrementalAuth = h.responsesWebsocketPinnedAuthReusable(pinnedAuthID)
			cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
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
				if websocketUpstreamSupportsIncrementalInput(selectedAuth.Attributes, selectedAuth.Metadata) {
					pinnedAuthID = authID
					if !requireFreshFullTranscriptBeforeIncremental {
						requestSelectedIncrementalAuth = true
					}
				}
			})
		}
		dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")
		subscribeUpstreamDisconnect(currentExecutionSessionID)

		completedOutput, completedResponseID, completedForward, errForward := h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, &wsTimelineLog, connectionID, downstreamSessionKey, toolCallCache)
		if errors.Is(errForward, errResponsesWebsocketRetryFullTranscript) &&
			allowIncrementalInputWithPreviousResponseID &&
			strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != "" {
			if h != nil && h.AuthManager != nil {
				suppressNextUpstreamDisconnect(currentExecutionSessionID)
				setActiveDisconnectSessionID("")
				h.AuthManager.ResetExecutionSession(currentExecutionSessionID)
				setActiveDisconnectSessionID(currentExecutionSessionID)
			}
			retryJSON, _, retryErrMsg := normalizeResponsesWebsocketRequestWithMode(
				payload,
				normalizationLastRequest,
				normalizationLastResponseOutput,
				false,
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
			clearPinnedAuthIfUnusable(currentExecutionSessionID, "pre_retry")
			if pinnedAuthID != "" && h.responsesWebsocketPinnedAuthReusable(pinnedAuthID) {
				requestSelectedIncrementalAuth = true
				cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
			}
			dataChan, _, errChan = h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, retryJSON, "")
			subscribeUpstreamDisconnect(currentExecutionSessionID)
			completedOutput, completedResponseID, completedForward, errForward = h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, &wsTimelineLog, connectionID, downstreamSessionKey, toolCallCache)
		}
		if errForward != nil {
			wsTerminateErr = errForward
			log.Warnf("responses websocket: forward failed id=%s error=%v", connectionID, errForward)
			return
		}
		if clearPinnedAuthIfUnusable(currentExecutionSessionID, "post_forward") {
			requestSelectedIncrementalAuth = false
		}
		if completedForward {
			lastRequest = requestStateToCommit
			lastResponseOutput = completedOutput
			if requestSelectedIncrementalAuth {
				lastResponseID = strings.TrimSpace(completedResponseID)
				lastResponseIDIncrementalEligible = lastResponseID != ""
				requireFreshFullTranscriptBeforeIncremental = false
			} else {
				lastResponseID = ""
				lastResponseIDIncrementalEligible = false
			}
		}
	}
}

func isResponsesWebsocketControlAck(payload []byte) bool {
	return strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == wsRequestTypeProcessed
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
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		// log.Infof("responses websocket: response.create request")
		if len(lastRequest) == 0 {
			return normalizeResponseCreateRequest(rawJSON)
		}
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID)
	case wsRequestTypeAppend:
		// log.Infof("responses websocket: response.append request")
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID)
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

	// Compaction can cause clients to replace local websocket history with a new
	// compact transcript on the next `response.create`. When the input already
	// contains historical model output items, treating it as an incremental append
	// duplicates stale turn-state and can leave late orphaned function_call items.
	if shouldReplaceWebsocketTranscript(rawJSON, nextInput) {
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
			if !gjson.GetBytes(normalized, "model").Exists() {
				modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
				if modelName != "" {
					normalized, _ = sjson.SetBytes(normalized, "model", modelName)
				}
			}
			if !gjson.GetBytes(normalized, "instructions").Exists() {
				instructions := gjson.GetBytes(lastRequest, "instructions")
				if instructions.Exists() {
					normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
				}
			}
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			normalized = codexcommon.NormalizeResponseInputItems(normalized)
			if websocketIncrementalToolOutputsKnown(gjson.GetBytes(normalized, "input"), gjson.ParseBytes(lastResponseOutput)) {
				return normalized, normalized, nil
			}
		}
	}

	existingInput := gjson.GetBytes(lastRequest, "input")
	mergedInput := mergeJSONArrayRawTrusted(existingInput.Raw, trustedJSONArrayRawString(lastResponseOutput))
	mergedInput = mergeJSONArrayRawTrusted(mergedInput, nextInput.Raw)
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
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	normalized = codexcommon.NormalizeResponseInputItems(normalized)
	return normalized, normalized, nil
}

func websocketIncrementalToolOutputsKnown(input gjson.Result, lastResponseOutput gjson.Result) bool {
	if !input.IsArray() {
		return false
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

func shouldReplaceWebsocketTranscript(rawJSON []byte, nextInput gjson.Result) bool {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) != "" {
		return false
	}
	if !nextInput.Exists() || !nextInput.IsArray() {
		return false
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

func normalizeResponseTranscriptReplacement(rawJSON []byte, lastRequest []byte) []byte {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = rawJSON
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
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
	return websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata)
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsIncrementalInputForModel(modelName string) bool {
	if h == nil || h.AuthManager == nil {
		return false
	}

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
	baseModel := strings.TrimSpace(parsed.ModelName)
	providers := util.GetProviderName(baseModel)
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}
	if len(providers) == 0 {
		return false
	}

	modelKey := baseModel
	if modelKey == "" {
		modelKey = strings.TrimSpace(resolvedModelName)
	}
	return h.AuthManager.AnyAvailableAuthForModel(providers, modelKey, func(auth *coreauth.Auth) bool {
		if auth == nil || auth.Disabled || auth.Unavailable || auth.Status == coreauth.StatusDisabled {
			return false
		}
		return websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata)
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

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsTimelineLog *websocketTimelineBuilder,
	sessionID string,
	downstreamSessionKey string,
	toolCallCaches ...*websocketToolOutputCache,
) ([]byte, string, bool, error) {
	var toolCallCache *websocketToolOutputCache
	if len(toolCallCaches) > 0 {
		toolCallCache = toolCallCaches[0]
	}
	completed := false
	completedOutput := []byte("[]")
	completedResponseID := ""
	emittedPayload := false
	noticeFilter := newResponsesNoticeFilter()
	requestCtx := c.Request.Context()
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
	if data == nil && errs == nil {
		return completedOutput, completedResponseID, completed, failNilStreamChannels()
	}

	for {
		select {
		case <-requestCtx.Done():
			cancel(requestCtx.Err())
			return completedOutput, completedResponseID, completed, requestCtx.Err()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				if data == nil {
					return completedOutput, completedResponseID, completed, failNilStreamChannels()
				}
				continue
			}
			if errMsg != nil {
				if !emittedPayload && responsesWebsocketShouldRetryFullTranscript(errMsg) {
					if errMsg.Error != nil {
						cancel(errMsg.Error)
					} else {
						cancel(errResponsesWebsocketRetryFullTranscript)
					}
					return completedOutput, completedResponseID, completed, errResponsesWebsocketRetryFullTranscript
				}
				recordResponsesWebsocketAPIResponseError(h, c, errMsg)
				errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
				log.Infof(
					"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
					sessionID,
					websocket.TextMessage,
					websocketPayloadEventType(errorPayload),
					websocketPayloadPreview(errorPayload),
				)
				if errWrite != nil {
					// log.Warnf(
					// 	"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					// 	sessionID,
					// 	websocketPayloadEventType(errorPayload),
					// 	errWrite,
					// )
					cancel(errMsg.Error)
					return completedOutput, completedResponseID, completed, errWrite
				}
			}
			if errMsg != nil {
				cancel(errMsg.Error)
			} else {
				cancel(nil)
			}
			return completedOutput, completedResponseID, completed, nil
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					errMsg := &interfaces.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					recordResponsesWebsocketAPIResponseError(h, c, errMsg)
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
						cancel(errMsg.Error)
						return completedOutput, completedResponseID, completed, errWrite
					}
					cancel(errMsg.Error)
					return completedOutput, completedResponseID, completed, nil
				}
				cancel(nil)
				return completedOutput, completedResponseID, completed, nil
			}

			payloads := websocketJSONPayloadsFromChunk(chunk)
			for i := range payloads {
				filteredPayload := payloads[i]
				if noticeFilter != nil {
					filteredPayload = noticeFilter.FilterPayload(filteredPayload)
				}
				if len(filteredPayload) == 0 {
					continue
				}
				eventType := strings.TrimSpace(gjson.GetBytes(filteredPayload, "type").String())
				if eventType == wsEventTypeError {
					if !emittedPayload && responsesWebsocketPayloadShouldRetryFullTranscript(filteredPayload) {
						cancel(errResponsesWebsocketRetryFullTranscript)
						return completedOutput, completedResponseID, completed, errResponsesWebsocketRetryFullTranscript
					}
					markAPIResponseTimestamp(c)
					if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, filteredPayload, time.Now()); errWrite != nil {
						log.Warnf(
							"responses websocket: downstream_out write failed id=%s event=%s error=%v",
							sessionID,
							websocketPayloadEventType(filteredPayload),
							errWrite,
						)
						cancel(errWrite)
						return completedOutput, completedResponseID, completed, errWrite
					}
					cancel(nil)
					return completedOutput, completedResponseID, completed, nil
				}
				recordResponsesWebsocketToolCallsFromPayloadWithCacheAndType(toolCallCache, downstreamSessionKey, eventType, filteredPayload)
				if eventType == wsEventTypeCompleted {
					completed = true
					completedOutput = responseCompletedOutputFromPayload(filteredPayload)
					completedResponseID = responseCompletedIDFromPayload(filteredPayload)
				}
				markAPIResponseTimestamp(c)
				// log.Infof(
				// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				// 	sessionID,
				// 	websocket.TextMessage,
				// 	websocketPayloadEventType(payloads[i]),
				// 	websocketPayloadPreview(payloads[i]),
				// )
				if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, filteredPayload, time.Now()); errWrite != nil {
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s error=%v",
						sessionID,
						websocketPayloadEventType(filteredPayload),
						errWrite,
					)
					cancel(errWrite)
					return completedOutput, completedResponseID, completed, errWrite
				}
				emittedPayload = true
			}
		}
	}
}

func responseCompletedIDFromPayload(payload []byte) string {
	return strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
}

func responseCompletedOutputFromPayload(payload []byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() {
		return bytes.Clone([]byte(output.Raw))
	}
	return []byte("[]")
}

func responsesWebsocketShouldRetryFullTranscript(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil || errMsg.Error == nil {
		return false
	}
	if errMsg.StatusCode > 0 && errMsg.StatusCode != http.StatusBadRequest {
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
	lower := strings.ToLower(errText)
	return strings.Contains(lower, "no tool call found") &&
		strings.Contains(lower, "function call output")
}

func responsesWebsocketPayloadIsError(payload []byte) bool {
	if !gjson.ValidBytes(bytes.TrimSpace(payload)) {
		return false
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == wsEventTypeError
}

func responsesWebsocketPayloadShouldRetryFullTranscript(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != wsEventTypeError {
		return false
	}
	if status := int(gjson.GetBytes(payload, "status").Int()); status > 0 && status != http.StatusBadRequest {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()), "previous_response_not_found") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "error.param").String()), "previous_response_id") {
		return true
	}
	errText := strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
	if errText == "" {
		errText = strings.TrimSpace(gjson.GetBytes(payload, "message").String())
	}
	if responsesWebsocketPreviousResponseNotFoundText(errText) {
		return true
	}
	lower := strings.ToLower(errText)
	return strings.Contains(lower, "no tool call found") &&
		strings.Contains(lower, "function call output")
}

func responsesWebsocketPreviousResponseNotFoundText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || !strings.Contains(lower, "not found") {
		return false
	}
	if strings.Contains(lower, "previous_response_id") {
		return true
	}
	return strings.Contains(lower, "previous response")
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	payloads := make([][]byte, 0, 2)
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
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte(wsDoneMarker)) {
			continue
		}
		if json.Valid(line) {
			payloads = append(payloads, line)
		}
	}

	if len(payloads) > 0 {
		return payloads
	}

	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte(wsDoneMarker)) && json.Valid(trimmed) {
		payloads = append(payloads, trimmed)
	}
	return payloads
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
	if builder == nil {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	if !websocketTimelineShouldRecord(builder, eventType, trimmedPayload) {
		return
	}
	if websocketTimelineTruncated(builder) {
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
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType == "" {
		return "-"
	}
	return eventType
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
	appendWebsocketTimelineEvent(wsTimelineLog, "response", payload, timestamp)
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
	if builder == nil {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	if !websocketTimelineShouldRecord(builder, eventType, trimmedPayload) {
		return
	}
	if websocketTimelineTruncated(builder) {
		return
	}
	trimmedPayload = util.RedactSensitiveLogBytes(trimmedPayload)
	if builder.Len() > 0 {
		appendWebsocketTimelineText(builder, "\n")
	}
	appendWebsocketTimelineText(builder, "Timestamp: ")
	appendWebsocketTimelineText(builder, timestamp.Format(time.RFC3339Nano))
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
	if builder == nil || !builder.errorOnly {
		return true
	}
	switch strings.TrimSpace(eventType) {
	case "disconnect", "error":
		return true
	}
	payloadType := websocketPayloadEventType(payload)
	if payloadType == wsEventTypeError {
		return true
	}
	return strings.Contains(payloadType, "error") || strings.Contains(payloadType, "failed")
}

func markAPIResponseTimestamp(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	c.Set("API_RESPONSE_TIMESTAMP", time.Now())
}
