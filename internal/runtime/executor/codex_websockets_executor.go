// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/proxy"
)

const (
	codexResponsesWebsocketBetaHeaderValue    = "responses_websockets=2026-02-06"
	codexBetaFeatureResponseProcessed         = "responses_websocket_response_processed"
	codexClientMetadataWSStreamRequestStartMS = "x-codex-ws-stream-request-start-ms"
	codexResponsesWebsocketIdleTimeout        = 5 * time.Minute
	// Rotate before the documented 60 minute upstream connection limit so the
	// next turn opens a fresh socket instead of discovering the cap mid-chain.
	codexResponsesWebsocketMaxLifetime  = 55 * time.Minute
	codexResponsesWebsocketHandshakeTO  = 30 * time.Second
	codexResponsesWebsocketWriteTO      = 30 * time.Second
	codexResponsesWebsocketProbeIdle    = 45 * time.Second
	codexResponsesWebsocketProbeWriteTO = 10 * time.Second
	codexResponsesWebsocketReadBuffer   = 8
	codexResponsesWebsocketReadLimit    = 64 << 20
	codexResponsesWebsocketMaxParked    = 16
)

var codexResponsesWebsocketParkTTL = 10 * time.Second

var codexWebsocketWriteBufferPool sync.Pool

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	store *codexWebsocketSessionStore
}

// codexWebsocketSessionStore is a two-lock session table.
//
// sessionsMu protects the sessions map (keyed by execution sessionID).
// parkedMu protects the parked map (keyed by reuseKey).
//
// Splitting what used to be a single RWMutex lets "get active session" on the
// hot path contend only with other active-session mutations, not with the
// park/unpark bookkeeping that a long-lived TTL timer triggers. When an
// operation must touch both maps it acquires sessionsMu first, then parkedMu,
// to avoid deadlocks.
type codexWebsocketSessionStore struct {
	sessionsMu sync.RWMutex
	sessions   map[string]*codexWebsocketSession

	parkedMu sync.Mutex
	parked   map[string]*codexWebsocketSession
}

var globalCodexWebsocketSessionStore = &codexWebsocketSessionStore{
	sessions: make(map[string]*codexWebsocketSession),
	parked:   make(map[string]*codexWebsocketSession),
}

type codexWebsocketSession struct {
	sessionID string

	reqMu sync.Mutex

	connMu sync.Mutex
	conn   *websocket.Conn
	wsURL  string
	authID string

	writeMu sync.Mutex

	activeMu   sync.Mutex
	activeCh   chan codexWebsocketRead
	activeConn *websocket.Conn
	activeDone <-chan struct{}
	// activeClose closes the current activeDone channel exactly once. It is
	// replaced on every setActive so callers holding a pre-activation copy of
	// activeDone still observe cancellation of the old generation.
	activeClose func()

	readerConn         *websocket.Conn
	forceHTTPFallback  atomic.Bool
	turnState          atomic.Value
	turnStateScope     atomic.Value
	lastRequest        []byte
	lastRequestCmp     []byte
	lastRequestInput   [][]byte
	lastResponseID     string
	lastResponseOutput []byte
	lastResponseItems  [][]byte

	lastActivityUnixNano atomic.Int64
	lastProbeUnixNano    atomic.Int64
	openedUnixNano       atomic.Int64

	reuseKey  string
	parkTimer *time.Timer

	upstreamDisconnectMu sync.Mutex
	upstreamDisconnectCh chan error
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		store:         globalCodexWebsocketSessionStore,
	}
}

type codexWebsocketRead struct {
	conn    *websocket.Conn
	msgType int
	payload []byte
	err     error
}

type codexPreparedWebsocketRequest struct {
	body               []byte
	wsURL              string
	wsHeaders          http.Header
	wsReqBody          []byte
	wsReqLog           helps.UpstreamRequestLog
	authID             string
	executionSessionID string
	sess               *codexWebsocketSession
	sessionLocked      bool
	reuseKey           string
	httpFallback       bool
}

func (r *codexPreparedWebsocketRequest) unlockSession() {
	if r == nil || !r.sessionLocked || r.sess == nil {
		return
	}
	r.sess.reqMu.Unlock()
	r.sessionLocked = false
}

func (s *codexWebsocketSession) httpFallbackActive() bool {
	return s != nil && s.forceHTTPFallback.Load()
}

func (s *codexWebsocketSession) activateHTTPFallback() {
	if s == nil {
		return
	}
	s.forceHTTPFallback.Store(true)
}

func (s *codexWebsocketSession) currentTurnState() string {
	if s == nil {
		return ""
	}
	value := s.turnState.Load()
	state, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(state)
}

func (s *codexWebsocketSession) currentTurnStateScope() string {
	if s == nil {
		return ""
	}
	value := s.turnStateScope.Load()
	scope, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(scope)
}

func (s *codexWebsocketSession) setTurnStateScope(scope string) {
	if s == nil {
		return
	}
	scope = strings.TrimSpace(scope)
	if scope == "" || s.currentTurnStateScope() == scope {
		return
	}
	s.turnStateScope.Store(scope)
	s.turnState.Store("")
}

func (s *codexWebsocketSession) applyTurnStateHeader(headers http.Header) {
	if s == nil || headers == nil {
		return
	}
	if strings.TrimSpace(headers.Get(codexHeaderTurnState)) != "" {
		return
	}
	if state := s.currentTurnState(); state != "" {
		headers.Set(codexHeaderTurnState, state)
	}
}

func (s *codexWebsocketSession) rememberTurnStateHeader(headers http.Header) {
	if s == nil || headers == nil {
		return
	}
	state := strings.TrimSpace(headers.Get(codexHeaderTurnState))
	if state == "" {
		return
	}
	s.turnState.Store(state)
}

func (s *codexWebsocketSession) rememberLogicalRequest(body []byte) {
	if s == nil {
		return
	}
	request := bytes.Clone(bytes.TrimSpace(body))
	s.lastRequest = request
	s.lastResponseID = ""
	s.lastResponseOutput = nil
	s.lastResponseItems = nil
	s.lastRequestCmp, _ = codexComparableRequestWithoutInput(request)
	if inputItems, ok := codexRawArrayItems(gjson.GetBytes(request, "input")); ok {
		s.lastRequestInput = inputItems
	} else {
		s.lastRequestInput = nil
	}
}

func (s *codexWebsocketSession) rememberCompletedResponse(eventData []byte) {
	if s == nil || len(bytes.TrimSpace(eventData)) == 0 {
		return
	}
	s.lastResponseID = strings.TrimSpace(gjson.GetBytes(eventData, "response.id").String())
	s.lastResponseOutput = codexCompletedResponseOutput(eventData)
	if outputItems, ok := codexRawArrayItems(gjson.ParseBytes(s.lastResponseOutput)); ok {
		s.lastResponseItems = outputItems
	} else {
		s.lastResponseItems = nil
	}
}

func (s *codexWebsocketSession) clearIncrementalState() {
	if s == nil {
		return
	}
	s.lastRequest = nil
	s.lastRequestCmp = nil
	s.lastRequestInput = nil
	s.lastResponseID = ""
	s.lastResponseOutput = nil
	s.lastResponseItems = nil
}

func (s *codexWebsocketSession) setActive(ch chan codexWebsocketRead, conn *websocket.Conn) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	if s.activeClose != nil {
		s.activeClose()
		s.activeClose = nil
		s.activeDone = nil
	}
	s.activeCh = ch
	s.activeConn = conn
	if ch != nil {
		done := make(chan struct{})
		s.activeDone = done
		var closeOnce sync.Once
		doneSlot := done
		s.activeClose = func() { closeOnce.Do(func() { close(doneSlot) }) }
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) clearActive(ch chan codexWebsocketRead) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	if s.activeCh == ch {
		s.activeCh = nil
		s.activeConn = nil
		if s.activeClose != nil {
			s.activeClose()
		}
		s.activeClose = nil
		s.activeDone = nil
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) activeForConn(conn *websocket.Conn) (chan codexWebsocketRead, <-chan struct{}, bool) {
	if s == nil || conn == nil {
		return nil, nil, false
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeCh == nil || s.activeConn != conn {
		return nil, nil, false
	}
	return s.activeCh, s.activeDone, true
}

func (s *codexWebsocketSession) activeOwnedByAnotherConn(conn *websocket.Conn) bool {
	if s == nil || conn == nil {
		return false
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	return s.activeCh != nil && s.activeConn != nil && s.activeConn != conn
}

func (s *codexWebsocketSession) isCurrentConn(conn *websocket.Conn) bool {
	if s == nil || conn == nil {
		return false
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.conn == conn
}

func (s *codexWebsocketSession) writeMessage(conn *websocket.Conn, msgType int, payload []byte) error {
	if s == nil {
		return fmt.Errorf("codex websockets executor: session is nil")
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now()
	_ = conn.SetWriteDeadline(now.Add(codexResponsesWebsocketWriteTO))
	if err := conn.WriteMessage(msgType, payload); err != nil {
		return err
	}
	s.touchActivityAt(now)
	return nil
}

func (s *codexWebsocketSession) configureConn(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	s.touchActivity()
	conn.SetPingHandler(func(appData string) error {
		s.touchActivity()
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		// Reply pongs from the same write lock to avoid concurrent writes.
		if err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second)); err != nil {
			return err
		}
		s.touchActivity()
		return nil
	})
	conn.SetPongHandler(func(string) error {
		s.touchActivity()
		return nil
	})
}

func (s *codexWebsocketSession) touchActivity() {
	s.touchActivityAt(time.Now())
}

func (s *codexWebsocketSession) touchActivityAt(now time.Time) {
	if s == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.lastActivityUnixNano.Store(now.UnixNano())
}

func (s *codexWebsocketSession) markProbe(now time.Time) {
	if s == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.lastProbeUnixNano.Store(now.UnixNano())
}

func (s *codexWebsocketSession) markOpened(now time.Time) {
	if s == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.openedUnixNano.Store(now.UnixNano())
}

func (s *codexWebsocketSession) shouldRotate(now time.Time) bool {
	if s == nil || codexResponsesWebsocketMaxLifetime <= 0 {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	opened := s.openedUnixNano.Load()
	if opened <= 0 {
		return false
	}
	return now.Sub(time.Unix(0, opened)) >= codexResponsesWebsocketMaxLifetime
}

func (s *codexWebsocketSession) shouldProbe(now time.Time) bool {
	if s == nil {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}

	lastProbe := s.lastProbeUnixNano.Load()
	if lastProbe == 0 {
		return true
	}

	reference := lastProbe
	if lastActivity := s.lastActivityUnixNano.Load(); lastActivity > reference {
		reference = lastActivity
	}
	if reference <= 0 {
		return true
	}
	return now.Sub(time.Unix(0, reference)) >= codexResponsesWebsocketProbeIdle
}

func (s *codexWebsocketSession) probeConn(conn *websocket.Conn) error {
	if s == nil {
		return fmt.Errorf("codex websockets executor: session is nil")
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now()
	if err := conn.WriteControl(websocket.PingMessage, nil, now.Add(codexResponsesWebsocketProbeWriteTO)); err != nil {
		return err
	}
	s.touchActivityAt(now)
	s.markProbe(now)
	return nil
}

func (s *codexWebsocketSession) notifyUpstreamDisconnect(err error) {
	if s == nil {
		return
	}
	s.upstreamDisconnectMu.Lock()
	ch := s.upstreamDisconnectCh
	s.upstreamDisconnectCh = nil
	s.upstreamDisconnectMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
	close(ch)
}

func (s *codexWebsocketSession) upstreamDisconnectChan() <-chan error {
	if s == nil {
		return nil
	}
	s.upstreamDisconnectMu.Lock()
	defer s.upstreamDisconnectMu.Unlock()
	if s.upstreamDisconnectCh == nil {
		s.upstreamDisconnectCh = make(chan error, 1)
	}
	return s.upstreamDisconnectCh
}

func (e *CodexWebsocketsExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = contextWithCodexForcedUpstreamSessionFromOptions(ctx, opts)
	if isCodexOpenAIImageRequest(opts) {
		return e.CodexExecutor.executeOpenAIImage(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return e.CodexExecutor.executeCompact(ctx, auth, req, opts)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	reporter.CaptureModelReasoningEffort(opts.OriginalRequest, req.Payload)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	body, originalTranslated := helps.TranslateRequestWithOriginal(e.cfg, from, to, baseModel, req.Payload, originalPayload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth)
	}

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	prepared, err := e.prepareCodexWebsocketRequest(ctx, auth, req, opts, body, apiKey, httpURL)
	if err != nil {
		return resp, err
	}
	if prepared.httpFallback {
		prepared.unlockSession()
		return e.CodexExecutor.Execute(ctx, auth, req, opts)
	}
	defer prepared.unlockSession()
	body = prepared.body
	wsReqBody := prepared.wsReqBody
	wsReqLog := prepared.wsReqLog
	wsURL := prepared.wsURL
	wsHeaders := prepared.wsHeaders
	authID := prepared.authID
	executionSessionID := prepared.executionSessionID
	sess := prepared.sess
	helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if respHS != nil {
			helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header, bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			e.activateSessionHTTPFallback(sess, nil, "upgrade_required", errDial)
			prepared.unlockSession()
			return e.CodexExecutor.Execute(ctx, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUnauthorized && !codexUnauthorizedRetryAlreadyUsed(ctx) {
			refreshedAuth, retried, refreshErr := e.CodexExecutor.refreshCodexAuthAfterUnauthorized(ctx, auth)
			if refreshErr != nil {
				return resp, refreshErr
			}
			if retried {
				prepared.unlockSession()
				return e.Execute(contextWithCodexUnauthorizedRetryUsed(ctx), refreshedAuth, req, opts)
			}
		}
		if respHS != nil && respHS.StatusCode > 0 {
			return resp, statusErrWithHeaders{
				statusErr: newCodexStatusErr(respHS.StatusCode, bodyErr),
				headers:   respHS.Header.Clone(),
			}
		}
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
		return resp, errDial
	}
	recordAPIWebsocketHandshake(ctx, e.cfg, respHS)
	if sess != nil && respHS != nil {
		sess.rememberTurnStateHeader(respHS.Header)
	}
	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
		defer func() {
			reason := "completed"
			if err != nil {
				reason = "error"
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, reason, err)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, codexResponsesWebsocketReadBuffer)
		sess.setActive(readCh, conn)
		defer sess.clearActive(readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		if sess != nil {
			retryBody := buildCodexWebsocketSendRetryBody(body, wsReqBody, wsHeaders.Get(codexHeaderTurnMetadata), time.Now())
			connRetry, wsReqBodyRetry, errRetry := e.retrySessionWebsocketRequest(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, retryBody, errSend)
			if errRetry != nil {
				return resp, errRetry
			}
			conn = connRetry
			wsReqBody = wsReqBodyRetry
		} else {
			helps.RecordAPIWebsocketError(ctx, e.cfg, "send", errSend)
			return resp, errSend
		}
	}
	streamState := newCodexStreamCompletionState()
	previousResponseRetryUsed := false
	readRetryUsed := false
	for {
		if ctx != nil && ctx.Err() != nil {
			return resp, ctx.Err()
		}
		msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
		if errRead != nil {
			if sess != nil && !readRetryUsed && (ctx == nil || ctx.Err() == nil) {
				readRetryUsed = true
				connRetry, wsReqBodyRetry, errRetry := e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBody, "read_error", errRead)
				if errRetry == nil {
					conn = connRetry
					wsReqBody = wsReqBodyRetry
					streamState = newCodexStreamCompletionState()
					continue
				}
				sess.clearIncrementalState()
				helps.RecordAPIWebsocketError(ctx, e.cfg, "read_retry", errRetry)
				return resp, errRetry
			}
			if sess != nil {
				sess.clearIncrementalState()
				e.invalidateUpstreamConn(sess, conn, "read_error", errRead)
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "read", errRead)
			return resp, errRead
		}
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				err = fmt.Errorf("codex websockets executor: unexpected binary message")
				if sess != nil {
					sess.clearIncrementalState()
					e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
				}
				helps.RecordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", err)
				return resp, err
			}
			continue
		}

		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		helps.AppendAPIWebsocketResponse(ctx, e.cfg, payload)

		if wsErr, ok := parseCodexWebsocketError(payload); ok {
			if sess != nil && codexWebsocketConnectionLimitReached(payload) {
				connRetry, wsReqBodyRetry, errRetry := e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBody, "connection_limit", wsErr)
				if errRetry != nil {
					if sess != nil {
						e.invalidateUpstreamConn(sess, conn, "upstream_error", errRetry)
					}
					helps.RecordAPIWebsocketError(ctx, e.cfg, "connection_limit_retry", errRetry)
					return resp, errRetry
				}
				conn = connRetry
				wsReqBody = wsReqBodyRetry
				streamState = newCodexStreamCompletionState()
				continue
			}
			if !previousResponseRetryUsed && codexShouldRetryWithoutPreviousResponse(body, wsReqBody, payload) {
				previousResponseRetryUsed = true
				helps.LogWithRequestID(ctx).Debugf("codex websockets executor: retrying without previous_response_id after upstream rejected incremental context")
				wsReqBodyRetry := buildCodexWebsocketRetryWithoutPreviousResponse(body, wsHeaders.Get(codexHeaderTurnMetadata), time.Now())
				wsReqLog.Body = wsReqBodyRetry
				helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)
				if errSendRetry := writeCodexWebsocketMessage(sess, conn, wsReqBodyRetry); errSendRetry != nil {
					if sess != nil {
						connRetry, wsReqBodyRetry2, errRetry := e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBodyRetry, "previous_response_not_found", errSendRetry)
						if errRetry == nil {
							conn = connRetry
							wsReqBody = wsReqBodyRetry2
							streamState = newCodexStreamCompletionState()
							continue
						}
						sess.clearIncrementalState()
						e.invalidateUpstreamConn(sess, conn, "upstream_error", errRetry)
						helps.RecordAPIWebsocketError(ctx, e.cfg, "previous_response_not_found_retry", errRetry)
						return resp, errRetry
					}
					helps.RecordAPIWebsocketError(ctx, e.cfg, "previous_response_not_found_retry", errSendRetry)
					return resp, errSendRetry
				}
				wsReqBody = wsReqBodyRetry
				streamState = newCodexStreamCompletionState()
				continue
			}
			if sess != nil {
				sess.clearIncrementalState()
				e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
			return resp, wsErr
		}

		payload = normalizeCodexWebsocketCompletion(payload)
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == "response.incomplete" {
			terminalErr := codexResponseIncompleteEventErr(payload)
			if sess != nil {
				sess.clearIncrementalState()
				e.invalidateUpstreamConn(sess, conn, "upstream_incomplete", terminalErr)
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_incomplete", terminalErr)
			return resp, terminalErr
		}
		if terminalErr, ok := parseCodexStreamTerminalError(eventType, payload); ok {
			if sess != nil {
				sess.clearIncrementalState()
				e.invalidateUpstreamConn(sess, conn, "upstream_terminal", terminalErr)
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_terminal", terminalErr)
			return resp, terminalErr
		}
		if completed, ok := streamState.processEventDataWithType(eventType, payload, true); ok {
			payload = completed.data
			eventType = codexEventCompleted
		}
		if eventType == codexEventCompleted {
			if detail, ok := helps.ParseCodexUsage(payload); ok {
				reporter.Publish(ctx, detail)
			}
			if sess != nil {
				sess.rememberLogicalRequest(body)
				sess.rememberCompletedResponse(payload)
			}
			var param any
			out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, payload, &param)
			resp = cliproxyexecutor.Response{Payload: out}
			if codexWebsocketShouldSendResponseProcessed(wsHeaders, wsReqBody) {
				e.sendCodexWebsocketResponseProcessed(ctx, sess, conn, gjson.GetBytes(payload, "response.id").String())
			}
			return resp, nil
		}
	}
}

func (e *CodexWebsocketsExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	logAuthID := ""
	if auth != nil {
		logAuthID = auth.ID
	}
	log.Debugf("Executing Codex Websockets stream request with auth ID: %s, model: %s", logAuthID, req.Model)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = contextWithCodexForcedUpstreamSessionFromOptions(ctx, opts)
	if isCodexOpenAIImageRequest(opts) {
		return e.CodexExecutor.executeOpenAIImageStream(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	reporter.CaptureModelReasoningEffort(opts.OriginalRequest, req.Payload)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	body, originalTranslated := helps.TranslateRequestWithOriginal(e.cfg, from, to, baseModel, req.Payload, originalPayload, true)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth)
	}

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	prepared, err := e.prepareCodexWebsocketRequest(ctx, auth, req, opts, body, apiKey, httpURL)
	if err != nil {
		return nil, err
	}
	if prepared.httpFallback {
		prepared.unlockSession()
		return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
	}
	body = prepared.body
	wsReqBody := prepared.wsReqBody
	wsReqLog := prepared.wsReqLog
	wsURL := prepared.wsURL
	wsHeaders := prepared.wsHeaders
	authID := prepared.authID
	executionSessionID := prepared.executionSessionID
	sess := prepared.sess
	helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	var upstreamHeaders http.Header
	if respHS != nil {
		upstreamHeaders = respHS.Header.Clone()
	}
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if respHS != nil {
			helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header, bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			e.activateSessionHTTPFallback(sess, nil, "upgrade_required", errDial)
			prepared.unlockSession()
			return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUnauthorized && !codexUnauthorizedRetryAlreadyUsed(ctx) {
			refreshedAuth, retried, refreshErr := e.CodexExecutor.refreshCodexAuthAfterUnauthorized(ctx, auth)
			if refreshErr != nil {
				prepared.unlockSession()
				return nil, refreshErr
			}
			if retried {
				prepared.unlockSession()
				return e.ExecuteStream(contextWithCodexUnauthorizedRetryUsed(ctx), refreshedAuth, req, opts)
			}
		}
		if respHS != nil && respHS.StatusCode > 0 {
			prepared.unlockSession()
			return nil, statusErrWithHeaders{
				statusErr: newCodexStatusErr(respHS.StatusCode, bodyErr),
				headers:   respHS.Header.Clone(),
			}
		}
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
		prepared.unlockSession()
		return nil, errDial
	}
	recordAPIWebsocketHandshake(ctx, e.cfg, respHS)
	if sess != nil && respHS != nil {
		sess.rememberTurnStateHeader(respHS.Header)
	}

	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, codexResponsesWebsocketReadBuffer)
		sess.setActive(readCh, conn)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		helps.RecordAPIWebsocketError(ctx, e.cfg, "send", errSend)
		if sess != nil {
			retryBody := buildCodexWebsocketSendRetryBody(body, wsReqBody, wsHeaders.Get(codexHeaderTurnMetadata), time.Now())
			connRetry, wsReqBodyRetry, errRetry := e.retrySessionWebsocketRequest(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, retryBody, errSend)
			if errRetry != nil {
				sess.clearActive(readCh)
				prepared.unlockSession()
				return nil, errRetry
			}
			conn = connRetry
			wsReqBody = wsReqBodyRetry
		} else {
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, "send_error", errSend)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
			return nil, errSend
		}
	}
	out := make(chan cliproxyexecutor.StreamChunk, helps.StreamChunkBufferSize)
	go func() {
		terminateReason := "completed"
		var terminateErr error

		defer close(out)
		defer func() {
			if sess != nil {
				sess.clearActive(readCh)
				prepared.unlockSession()
				return
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()

		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var param any
		streamState := newCodexStreamCompletionState()
		emittedPayload := false
		previousResponseRetryUsed := false
		readRetryUsed := false
		for {
			if ctx != nil && ctx.Err() != nil {
				terminateReason = "context_done"
				terminateErr = ctx.Err()
				_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
				return
			}
			msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
			if errRead != nil {
				if sess != nil && ctx != nil && ctx.Err() != nil {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
					return
				}
				if sess != nil && !readRetryUsed && !emittedPayload {
					readRetryUsed = true
					connRetry, wsReqBodyRetry, errRetry := e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBody, "read_error", errRead)
					if errRetry == nil {
						conn = connRetry
						wsReqBody = wsReqBodyRetry
						streamState = newCodexStreamCompletionState()
						continue
					}
					terminateReason = "read_retry_error"
					terminateErr = errRetry
					sess.clearIncrementalState()
					helps.RecordAPIWebsocketError(ctx, e.cfg, "read_retry", errRetry)
					reporter.PublishFailureWithError(ctx, errRetry)
					_ = send(cliproxyexecutor.StreamChunk{Err: errRetry})
					return
				}
				terminateReason = "read_error"
				terminateErr = errRead
				if sess != nil {
					sess.clearIncrementalState()
					e.invalidateUpstreamConn(sess, conn, "read_error", errRead)
				}
				helps.RecordAPIWebsocketError(ctx, e.cfg, "read", errRead)
				reporter.PublishFailureWithError(ctx, errRead)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					err = fmt.Errorf("codex websockets executor: unexpected binary message")
					terminateReason = "unexpected_binary"
					terminateErr = err
					helps.RecordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", err)
					reporter.PublishFailureWithError(ctx, err)
					if sess != nil {
						sess.clearIncrementalState()
						e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
					}
					_ = send(cliproxyexecutor.StreamChunk{Err: err})
					return
				}
				continue
			}

			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			helps.AppendAPIWebsocketResponse(ctx, e.cfg, payload)

			if wsErr, ok := parseCodexWebsocketError(payload); ok {
				if sess != nil && !emittedPayload && codexWebsocketConnectionLimitReached(payload) {
					connRetry, wsReqBodyRetry, errRetry := e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBody, "connection_limit", wsErr)
					if errRetry == nil {
						conn = connRetry
						wsReqBody = wsReqBodyRetry
						streamState = newCodexStreamCompletionState()
						continue
					}
					terminateReason = "connection_limit_retry_error"
					terminateErr = errRetry
					helps.RecordAPIWebsocketError(ctx, e.cfg, "connection_limit_retry", errRetry)
					reporter.PublishFailureWithError(ctx, errRetry)
					_ = send(cliproxyexecutor.StreamChunk{Err: errRetry})
					return
				}
				if !previousResponseRetryUsed && !emittedPayload && codexShouldRetryWithoutPreviousResponse(body, wsReqBody, payload) {
					previousResponseRetryUsed = true
					helps.LogWithRequestID(ctx).Debugf("codex websockets executor: retrying without previous_response_id after upstream rejected incremental context")
					wsReqBodyRetry := buildCodexWebsocketRetryWithoutPreviousResponse(body, wsHeaders.Get(codexHeaderTurnMetadata), time.Now())
					wsReqLog.Body = wsReqBodyRetry
					helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)
					if errSendRetry := writeCodexWebsocketMessage(sess, conn, wsReqBodyRetry); errSendRetry != nil {
						if sess != nil {
							connRetry, wsReqBodyRetry2, errRetry := e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBodyRetry, "previous_response_not_found", errSendRetry)
							if errRetry == nil {
								conn = connRetry
								wsReqBody = wsReqBodyRetry2
								streamState = newCodexStreamCompletionState()
								continue
							}
							sess.clearIncrementalState()
							terminateReason = "previous_response_not_found_retry_error"
							terminateErr = errRetry
							helps.RecordAPIWebsocketError(ctx, e.cfg, "previous_response_not_found_retry", errRetry)
							reporter.PublishFailureWithError(ctx, errRetry)
							_ = send(cliproxyexecutor.StreamChunk{Err: errRetry})
							return
						}
						terminateReason = "previous_response_not_found_retry_error"
						terminateErr = errSendRetry
						helps.RecordAPIWebsocketError(ctx, e.cfg, "previous_response_not_found_retry", errSendRetry)
						reporter.PublishFailureWithError(ctx, errSendRetry)
						_ = send(cliproxyexecutor.StreamChunk{Err: errSendRetry})
						return
					}
					wsReqBody = wsReqBodyRetry
					streamState = newCodexStreamCompletionState()
					continue
				}
				terminateReason = "upstream_error"
				terminateErr = wsErr
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
				reporter.PublishFailureWithError(ctx, wsErr)
				if sess != nil {
					sess.clearIncrementalState()
					e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: wsErr})
				return
			}

			payload = normalizeCodexWebsocketCompletion(payload)
			eventType := gjson.GetBytes(payload, "type").String()
			if eventType == "response.incomplete" {
				terminalErr := codexResponseIncompleteEventErr(payload)
				if sess != nil {
					sess.clearIncrementalState()
					e.invalidateUpstreamConn(sess, conn, "upstream_incomplete", terminalErr)
				}
				terminateReason = "upstream_incomplete"
				terminateErr = terminalErr
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_incomplete", terminalErr)
				reporter.PublishFailureWithError(ctx, terminalErr)
				_ = send(cliproxyexecutor.StreamChunk{Err: terminalErr})
				return
			} else if terminalErr, ok := parseCodexStreamTerminalError(eventType, payload); ok {
				if sess != nil {
					sess.clearIncrementalState()
					e.invalidateUpstreamConn(sess, conn, "upstream_terminal", terminalErr)
				}
				terminateReason = "upstream_terminal"
				terminateErr = terminalErr
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_terminal", terminalErr)
				reporter.PublishFailureWithError(ctx, terminalErr)
				_ = send(cliproxyexecutor.StreamChunk{Err: terminalErr})
				return
			}
			if completed, ok := streamState.processEventDataWithType(eventType, payload, true); ok {
				payload = completed.data
				eventType = codexEventCompleted
			}
			if eventType == codexEventCompleted || eventType == "response.done" {
				if detail, ok := helps.ParseCodexUsage(payload); ok {
					reporter.Publish(ctx, detail)
				}
			}

			line := encodeCodexWebsocketAsSSE(payload)
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, body, line, &param)
			for i := range chunks {
				if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					return
				}
				if len(chunks[i]) > 0 {
					emittedPayload = true
				}
			}
			if eventType == codexEventCompleted || eventType == "response.done" {
				if sess != nil {
					sess.rememberLogicalRequest(body)
					sess.rememberCompletedResponse(payload)
				}
				if codexWebsocketShouldSendResponseProcessed(wsHeaders, wsReqBody) {
					e.sendCodexWebsocketResponseProcessed(ctx, sess, conn, gjson.GetBytes(payload, "response.id").String())
				}
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: upstreamHeaders, Chunks: out}, nil
}

func (e *CodexWebsocketsExecutor) prepareCodexWebsocketRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	body []byte,
	apiKey string,
	httpURL string,
) (*codexPreparedWebsocketRequest, error) {
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}

	// Cache the inbound gin headers once so the downstream helpers
	// (prompt-cache resolution, client metadata, trace-context propagation)
	// share a single context lookup rather than walking the gin request each
	// time.
	ctx = contextWithCachedCodexGinHeaders(ctx)

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	body = normalizeCodexFinalUpstreamBody(body, baseModel, auth, codexFinalUpstreamBodyOptions{
		requestKind:                codexFinalUpstreamResponses,
		streamMode:                 codexStreamFieldTrue,
		preservePreviousResponseID: true,
		preserveGenerate:           true,
		store:                      codexShouldStoreResponses(auth, httpURL),
		omitServiceTier:            auth == nil || !auth.ServiceTierPassthrough(),
	})

	executionSessionID := executionSessionIDFromOptions(opts)
	body = codexSanitizeForcedUpstreamSessionBody(ctx, body)
	body, wsHeaders := e.applyCodexPromptCacheHeaders(ctx, opts.SourceFormat, executionSessionID, req, body)
	codexApplyForcedUpstreamSessionHeaders(ctx, wsHeaders)
	responsesAPIClientMetadata := codexResponsesAPIClientMetadataFromBody(body)
	explicitTurnMetadata := ""
	if codexForcedUpstreamSessionID(ctx) == "" {
		explicitTurnMetadata = codexExplicitWebsocketTurnMetadata(ctx, body)
	}
	if explicitTurnMetadata != "" && strings.TrimSpace(wsHeaders.Get(codexHeaderTurnMetadata)) == "" {
		wsHeaders.Set(codexHeaderTurnMetadata, explicitTurnMetadata)
	}
	codexEnsureExecutionSessionHeader(wsHeaders, codexGinHeadersFromContext(ctx), executionSessionID)
	wsHeaders = applyCodexWebsocketHeadersForRequestKind(ctx, wsHeaders, auth, apiKey, e.cfg, codexWebsocketTurnMetadataRequestKind(body))
	codexMergeResponsesAPIClientMetadataIntoTurnMetadataHeader(wsHeaders, responsesAPIClientMetadata)
	turnStateScope := strings.TrimSpace(wsHeaders.Get(codexHeaderTurnMetadata))
	if explicitTurnMetadata != "" {
		turnStateScope = explicitTurnMetadata
	}
	body = codexApplyWebsocketClientMetadata(ctx, body, wsHeaders, auth, e.cfg)
	wsHeaders.Del("Traceparent")
	wsHeaders.Del("Tracestate")

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	reuseKey := codexWebsocketReusableKey(opts.SourceFormat, authID, wsURL, body)
	prepared := &codexPreparedWebsocketRequest{
		body:               body,
		wsURL:              wsURL,
		wsHeaders:          wsHeaders,
		authID:             authID,
		executionSessionID: executionSessionID,
		reuseKey:           reuseKey,
	}
	if prepared.executionSessionID != "" {
		prepared.sess = e.getOrCreateSession(prepared.executionSessionID, prepared.reuseKey)
		if prepared.sess != nil {
			prepared.sess.reqMu.Lock()
			prepared.sessionLocked = true
			if prepared.reuseKey != "" && prepared.sess.reuseKey != "" && prepared.sess.reuseKey != prepared.reuseKey {
				e.resetSessionForReuseKey(prepared.sess, prepared.reuseKey, "reuse_key_changed")
			}
			if prepared.reuseKey != "" && prepared.sess.reuseKey == "" {
				prepared.sess.reuseKey = prepared.reuseKey
			}
			prepared.httpFallback = prepared.sess.httpFallbackActive()
			if !prepared.httpFallback {
				prepared.sess.setTurnStateScope(turnStateScope)
				prepared.sess.applyTurnStateHeader(prepared.wsHeaders)
			}
		}
	}

	prepared.wsReqBody = buildCodexWebsocketRequestBody(body, wsHeaders.Get("X-Codex-Turn-Metadata"))
	if !prepared.httpFallback && prepared.sess != nil {
		if incrementalBody, ok := buildCodexIncrementalWebsocketRequestBody(prepared.sess, body, wsHeaders.Get("X-Codex-Turn-Metadata")); ok {
			prepared.wsReqBody = incrementalBody
		}
	}
	prepared.wsReqBody = stampCodexWebsocketStreamRequestStartMS(prepared.wsReqBody, time.Now())
	prepared.wsReqLog = helps.UpstreamRequestLog{
		URL:       wsURL,
		Method:    "WEBSOCKET",
		Headers:   wsHeaders,
		Body:      prepared.wsReqBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
	return prepared, nil
}

func codexEnsureExecutionSessionHeader(headers http.Header, source http.Header, executionSessionID string) {
	executionSessionID = strings.TrimSpace(executionSessionID)
	if headers == nil || executionSessionID == "" {
		return
	}
	if firstNonEmptyHeaderValue(headers, source, codexHeaderSessionID) != "" {
		return
	}
	if firstNonEmptyHeaderValue(headers, source, codexHeaderOfficialSessionID) != "" {
		return
	}
	if firstNonEmptyHeaderValue(headers, source, codexHeaderOfficialThreadID) != "" {
		return
	}
	if firstNonEmptyHeaderValue(headers, source, "Conversation_id") != "" {
		return
	}
	headers.Set(codexHeaderSessionID, executionSessionID)
}

func codexWebsocketReusableKey(_ sdktranslator.Format, authID string, wsURL string, body []byte) string {
	promptCacheID := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	if promptCacheID == "" {
		return ""
	}
	authID = strings.TrimSpace(authID)
	wsURL = strings.TrimSpace(wsURL)
	if authID == "" || wsURL == "" {
		return ""
	}
	windowID := strings.TrimSpace(gjson.GetBytes(body, "client_metadata."+codexClientMetadataWindowID).String())
	if windowID == "" {
		return authID + "|" + wsURL + "|" + promptCacheID
	}
	return authID + "|" + wsURL + "|" + promptCacheID + "|" + windowID
}

func (e *CodexWebsocketsExecutor) retrySessionWebsocketRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	sess *codexWebsocketSession,
	conn *websocket.Conn,
	readCh *chan codexWebsocketRead,
	authID string,
	wsURL string,
	wsHeaders http.Header,
	wsReqLog helps.UpstreamRequestLog,
	wsReqBody []byte,
	sendErr error,
) (*websocket.Conn, []byte, error) {
	return e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBody, "send_error", sendErr)
}

func (e *CodexWebsocketsExecutor) retrySessionWebsocketRequestWithReason(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	sess *codexWebsocketSession,
	conn *websocket.Conn,
	readCh *chan codexWebsocketRead,
	authID string,
	wsURL string,
	wsHeaders http.Header,
	wsReqLog helps.UpstreamRequestLog,
	wsReqBody []byte,
	reason string,
	cause error,
) (*websocket.Conn, []byte, error) {
	if sess == nil {
		return nil, nil, cause
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "retry"
	}

	if readCh != nil && *readCh != nil {
		sess.clearActive(*readCh)
		*readCh = nil
	}
	e.invalidateUpstreamConn(sess, conn, reason, cause)

	// Retry once with a fresh websocket connection. This mainly handles upstream
	// closing the socket between sequential requests within the same execution session.
	connRetry, respHSRetry, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	if errDialRetry != nil || connRetry == nil {
		closeHTTPResponseBody(respHSRetry, "codex websockets executor: close handshake response body error")
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial_retry", errDialRetry)
		return nil, nil, errDialRetry
	}

	wsReqBodyRetry := bytes.Clone(wsReqBody)
	wsReqLog.Body = wsReqBodyRetry
	helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)
	recordAPIWebsocketHandshake(ctx, e.cfg, respHSRetry)
	if respHSRetry != nil {
		sess.rememberTurnStateHeader(respHSRetry.Header)
	}
	if readCh != nil {
		newReadCh := make(chan codexWebsocketRead, codexResponsesWebsocketReadBuffer)
		sess.setActive(newReadCh, connRetry)
		*readCh = newReadCh
	}

	if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry != nil {
		if readCh != nil && *readCh != nil {
			sess.clearActive(*readCh)
			*readCh = nil
		}
		e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
		helps.RecordAPIWebsocketError(ctx, e.cfg, "send_retry", errSendRetry)
		return nil, nil, errSendRetry
	}

	return connRetry, wsReqBodyRetry, nil
}

func (e *CodexWebsocketsExecutor) dialCodexWebsocket(ctx context.Context, auth *cliproxyauth.Auth, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := newProxyAwareWebsocketDialer(e.cfg, auth)
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = true
	if ctx == nil {
		ctx = context.Background()
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if conn != nil {
		conn.SetReadLimit(codexResponsesWebsocketReadLimit)
		// Avoid gorilla/websocket flate tail validation issues on some upstreams/Go versions.
		// Negotiating permessage-deflate is fine; we just don't compress outbound messages.
		conn.EnableWriteCompression(false)
	}
	return conn, resp, err
}

func writeCodexWebsocketMessage(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	if sess != nil {
		return sess.writeMessage(conn, websocket.TextMessage, payload)
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	_ = conn.SetWriteDeadline(time.Now().Add(codexResponsesWebsocketWriteTO))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func codexWebsocketResponseProcessedEnabled(headers http.Header) bool {
	if headers == nil {
		return false
	}
	for _, raw := range headers.Values("X-Codex-Beta-Features") {
		for _, feature := range strings.Split(raw, ",") {
			if strings.EqualFold(strings.TrimSpace(feature), codexBetaFeatureResponseProcessed) {
				return true
			}
		}
	}
	return false
}

func codexWebsocketShouldSendResponseProcessed(headers http.Header, requestBody []byte) bool {
	if !codexWebsocketResponseProcessedEnabled(headers) {
		return false
	}
	generate := gjson.GetBytes(requestBody, "generate")
	return generate.Type != gjson.False
}

func buildCodexWebsocketResponseProcessedRequest(responseID string) []byte {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil
	}
	payload := make([]byte, 0, len(responseID)+48)
	payload = append(payload, `{"type":"response.processed","response_id":`...)
	payload = strconv.AppendQuote(payload, responseID)
	payload = append(payload, '}')
	return payload
}

func (e *CodexWebsocketsExecutor) sendCodexWebsocketResponseProcessed(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, responseID string) {
	payload := buildCodexWebsocketResponseProcessedRequest(responseID)
	if len(payload) == 0 {
		return
	}
	if err := writeCodexWebsocketMessage(sess, conn, payload); err != nil {
		helps.RecordAPIWebsocketError(ctx, e.cfg, "response_processed", err)
		log.Debugf("codex websockets executor: send response.processed failed: %v", err)
	}
}

func buildCodexWebsocketRequestBody(body []byte, turnMetadataHeader string) []byte {
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	body = codexEnsureResponsesContextField(body, codexFinalUpstreamResponses)

	// Match codex-rs websocket v2 semantics: every request is `response.create`.
	// Incremental follow-up turns continue on the same websocket using
	// `previous_response_id` + incremental `input`, not `response.append`.
	turnMetadataHeader = strings.TrimSpace(turnMetadataHeader)
	typeResult := gjson.GetBytes(body, "type")
	requestType := strings.TrimSpace(typeResult.String())
	turnMetadataMatches := turnMetadataHeader == "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String() == turnMetadataHeader
	if requestType == "response.create" && turnMetadataMatches {
		return body
	}
	if !typeResult.Exists() && turnMetadataMatches {
		if updated, ok := codexAppendTopLevelStringField(body, "type", "response.create"); ok {
			return updated
		}
	}
	if !typeResult.Exists() {
		bodyWithMetadata := body
		if turnMetadataHeader != "" {
			bodyWithMetadata = codexSetClientMetadataString(body, codexClientMetadataTurnMetadata, turnMetadataHeader, true)
		}
		if turnMetadataHeader == "" || gjson.GetBytes(bodyWithMetadata, "client_metadata.x-codex-turn-metadata").String() == turnMetadataHeader {
			if updated, ok := codexAppendTopLevelStringField(bodyWithMetadata, "type", "response.create"); ok {
				return updated
			}
		}
	}

	wsReqBody, errSet := sjson.SetBytes(body, "type", "response.create")
	if errSet == nil && turnMetadataHeader != "" && gjson.GetBytes(wsReqBody, "client_metadata.x-codex-turn-metadata").String() != turnMetadataHeader {
		wsReqBody, errSet = sjson.SetBytes(wsReqBody, "client_metadata.x-codex-turn-metadata", turnMetadataHeader)
	}
	if errSet == nil && len(wsReqBody) > 0 {
		return wsReqBody
	}
	return body
}

func buildCodexIncrementalWebsocketRequestBody(sess *codexWebsocketSession, body []byte, turnMetadataHeader string) ([]byte, bool) {
	if sess == nil || len(sess.lastRequestCmp) == 0 {
		return nil, false
	}
	previousResponseID := strings.TrimSpace(sess.lastResponseID)
	if previousResponseID == "" {
		return nil, false
	}

	currentComparable, ok := codexComparableRequestWithoutInput(body)
	if !ok || !bytes.Equal(sess.lastRequestCmp, currentComparable) {
		return nil, false
	}

	previousInput := sess.lastRequestInput
	if previousInput == nil {
		return nil, false
	}
	currentInput, ok := codexRawArrayItems(gjson.GetBytes(body, "input"))
	if !ok {
		return nil, false
	}
	responseOutput := sess.lastResponseItems
	if responseOutput == nil {
		return nil, false
	}

	baseline := make([][]byte, 0, len(previousInput)+len(responseOutput))
	baseline = append(baseline, previousInput...)
	baseline = append(baseline, responseOutput...)
	if len(currentInput) < len(baseline) {
		return nil, false
	}
	for i := range baseline {
		if !codexJSONRawEqual(currentInput[i], baseline[i]) {
			return nil, false
		}
	}

	delta := currentInput[len(baseline):]
	if !codexWebsocketDeltaToolOutputsAnchorable(baseline, delta) {
		return nil, false
	}
	wsReqBody := buildCodexWebsocketRequestBody(body, turnMetadataHeader)
	updated, err := sjson.SetRawBytes(wsReqBody, "input", codexRawJSONArray(delta))
	if err != nil {
		return nil, false
	}
	updated, err = sjson.SetBytes(updated, "previous_response_id", previousResponseID)
	if err != nil || len(updated) == 0 {
		return nil, false
	}
	return updated, true
}

func codexWebsocketDeltaToolOutputsAnchorable(baseline [][]byte, delta [][]byte) bool {
	knownCalls := make(map[string]string)
	for _, item := range baseline {
		itemType := codexWebsocketRawItemType(item)
		if !codexWebsocketIsToolCallType(itemType) {
			continue
		}
		if callID := codexWebsocketRawItemCallID(item); callID != "" {
			knownCalls[callID] = itemType
		}
	}
	for _, item := range delta {
		itemType := codexWebsocketRawItemType(item)
		callID := codexWebsocketRawItemCallID(item)
		if codexWebsocketIsToolCallType(itemType) {
			if callID != "" {
				knownCalls[callID] = itemType
			}
			continue
		}
		if !codexWebsocketIsToolCallOutputType(itemType) {
			continue
		}
		if itemType == "tool_search_output" && codexWebsocketToolSearchOutputCanStandAlone(item) {
			continue
		}
		if callID == "" {
			return false
		}
		callType := knownCalls[callID]
		if !codexWebsocketToolOutputMatchesCallType(itemType, callType) {
			return false
		}
	}
	return true
}

func codexWebsocketRawItemType(item []byte) string {
	return strings.TrimSpace(gjson.GetBytes(item, "type").String())
}

func codexWebsocketRawItemCallID(item []byte) string {
	return strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
}

func codexWebsocketIsToolCallType(itemType string) bool {
	switch itemType {
	case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call":
		return true
	default:
		return false
	}
}

func codexWebsocketIsToolCallOutputType(itemType string) bool {
	switch itemType {
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		return true
	default:
		return false
	}
}

func codexWebsocketToolOutputMatchesCallType(outputType string, callType string) bool {
	switch callType {
	case "function_call", "local_shell_call":
		return outputType == "function_call_output"
	case "custom_tool_call":
		return outputType == "custom_tool_call_output"
	case "tool_search_call":
		return outputType == "tool_search_output"
	default:
		return false
	}
}

func codexWebsocketToolSearchOutputCanStandAlone(item []byte) bool {
	return codexWebsocketRawItemCallID(item) == "" ||
		strings.EqualFold(strings.TrimSpace(gjson.GetBytes(item, "execution").String()), "server")
}

func codexShouldRetryWithoutPreviousResponse(body []byte, requestBody []byte, errorPayload []byte) bool {
	if strings.TrimSpace(gjson.GetBytes(requestBody, "previous_response_id").String()) == "" {
		return false
	}
	if !codexWebsocketPreviousResponseNotFound(errorPayload) &&
		!codexWebsocketNoToolCallFoundForFunctionOutput(errorPayload) {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) == "" {
		return true
	}
	return codexExplicitPreviousResponseRetryHasStandaloneContext(body)
}

func codexExplicitPreviousResponseRetryHasStandaloneContext(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(body, "prompt").String()) != "" {
		return true
	}
	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() && len(messages.Array()) > 0 {
		return true
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return false
	}
	if input.Type == gjson.String {
		return strings.TrimSpace(input.String()) != ""
	}
	if !input.IsArray() {
		return false
	}
	hasMessage := false
	hasToolOutput := false
	input.ForEach(func(_, item gjson.Result) bool {
		itemType := strings.TrimSpace(item.Get("type").String())
		if codexWebsocketIsToolCallOutputType(itemType) {
			hasToolOutput = true
			return true
		}
		role := strings.TrimSpace(item.Get("role").String())
		if itemType == "message" || (itemType == "" && role != "") {
			hasMessage = true
		}
		return true
	})
	return hasMessage && !hasToolOutput
}

func buildCodexWebsocketRetryWithoutPreviousResponse(body []byte, turnMetadataHeader string, now time.Time) []byte {
	retryBody := buildCodexWebsocketRequestBody(body, turnMetadataHeader)
	if updated, err := sjson.DeleteBytes(retryBody, "previous_response_id"); err == nil {
		retryBody = updated
	}
	return stampCodexWebsocketStreamRequestStartMS(retryBody, now)
}

func buildCodexWebsocketSendRetryBody(body []byte, requestBody []byte, turnMetadataHeader string, now time.Time) []byte {
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) == "" &&
		strings.TrimSpace(gjson.GetBytes(requestBody, "previous_response_id").String()) != "" {
		return buildCodexWebsocketRetryWithoutPreviousResponse(body, turnMetadataHeader, now)
	}
	return bytes.Clone(requestBody)
}

func stampCodexWebsocketStreamRequestStartMS(body []byte, now time.Time) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	if now.IsZero() {
		now = time.Now()
	}
	return codexSetClientMetadataString(body, codexClientMetadataWSStreamRequestStartMS, strconv.FormatInt(now.UnixMilli(), 10), true)
}

func codexComparableRequestWithoutInput(body []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	comparable, err := sjson.DeleteBytes(body, "input")
	if err != nil {
		return nil, false
	}
	for _, path := range []string{
		"generate",
		"client_metadata." + codexClientMetadataTurnMetadata,
		"client_metadata." + codexWSClientMetadataTraceparent,
		"client_metadata." + codexWSClientMetadataTracestate,
		"client_metadata." + codexClientMetadataWSStreamRequestStartMS,
	} {
		comparable, err = sjson.DeleteBytes(comparable, path)
		if err != nil {
			return nil, false
		}
	}
	metadata := gjson.GetBytes(comparable, "client_metadata")
	if metadata.Exists() && bytes.Equal(bytes.TrimSpace([]byte(metadata.Raw)), []byte("{}")) {
		comparable, err = sjson.DeleteBytes(comparable, "client_metadata")
		if err != nil {
			return nil, false
		}
	}
	canonical, ok := codexCanonicalJSONBytes(comparable)
	if !ok {
		return nil, false
	}
	return canonical, true
}

func codexRawArrayItems(result gjson.Result) ([][]byte, bool) {
	if !result.Exists() || !result.IsArray() {
		return nil, false
	}
	items := make([][]byte, 0, len(result.Array()))
	result.ForEach(func(_, item gjson.Result) bool {
		items = append(items, []byte(item.Raw))
		return true
	})
	return items, true
}

func codexCanonicalJSONBytes(raw []byte) ([]byte, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return canonical, true
}

func codexJSONRawEqual(left []byte, right []byte) bool {
	left = bytes.TrimSpace(left)
	right = bytes.TrimSpace(right)
	if bytes.Equal(left, right) {
		return true
	}
	var leftValue any
	var rightValue any
	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func codexCompletedResponseOutput(eventData []byte) []byte {
	output := gjson.GetBytes(eventData, "response.output")
	if output.Exists() && output.IsArray() {
		return []byte(output.Raw)
	}
	return []byte("[]")
}

func readCodexWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (int, []byte, error) {
	if sess == nil {
		if conn == nil {
			return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		return msgType, payload, errRead
	}
	if conn == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	if readCh == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: session read channel is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case ev, ok := <-readCh:
			if !ok {
				return 0, nil, fmt.Errorf("codex websockets executor: session read channel closed")
			}
			if ev.conn != conn {
				continue
			}
			if ev.err != nil {
				return 0, nil, ev.err
			}
			return ev.msgType, ev.payload, nil
		}
	}
}

// codexWebsocketDialerCache memoises constructed websocket.Dialer instances by
// (proxyURL, envCAFingerprint) so that hot paths do not re-parse the proxy URL
// and re-read the CA pool on every dial. The Dialer itself is immutable after
// construction apart from its Proxy/NetDialContext funcs, both of which are
// goroutine-safe to call concurrently.
var codexWebsocketDialerCache sync.Map

func newProxyAwareWebsocketDialer(cfg *config.Config, auth *cliproxyauth.Auth) *websocket.Dialer {
	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	cacheKey := proxyURL + "\x00" + os.Getenv("CODEX_CA_CERTIFICATE") + "\x00" + os.Getenv("SSL_CERT_FILE")
	if cached, ok := codexWebsocketDialerCache.Load(cacheKey); ok {
		if dialer, okDialer := cached.(*websocket.Dialer); okDialer {
			return dialer
		}
	}

	dialer := buildCodexWebsocketDialer(proxyURL)
	actual, _ := codexWebsocketDialerCache.LoadOrStore(cacheKey, dialer)
	if cached, ok := actual.(*websocket.Dialer); ok {
		return cached
	}
	return dialer
}

func buildCodexWebsocketDialer(proxyURL string) *websocket.Dialer {
	dialer := &websocket.Dialer{
		ReadBufferSize:    1024,
		WriteBufferSize:   1024,
		WriteBufferPool:   &codexWebsocketWriteBufferPool,
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   proxyutil.DefaultDialTimeout,
			KeepAlive: proxyutil.DefaultDialKeepAlive,
		}).DialContext,
	}
	if tlsConfig, err := misc.CustomTLSConfigFromEnv(); err != nil {
		log.Warnf("custom CA disabled for codex websocket dialer: %v", err)
	} else if tlsConfig != nil {
		dialer.TLSClientConfig = tlsConfig
	}

	if proxyURL == "" {
		return dialer
	}

	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("codex websockets executor: %v", errParse)
		return dialer
	}

	switch setting.Mode {
	case proxyutil.ModeDirect:
		dialer.Proxy = nil
		return dialer
	case proxyutil.ModeProxy:
	default:
		return dialer
	}

	switch setting.URL.Scheme {
	case "socks5", "socks5h":
		var proxyAuth *proxy.Auth
		if setting.URL.User != nil {
			username := setting.URL.User.Username()
			password, _ := setting.URL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		socksDialer, errSOCKS5 := proxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, &net.Dialer{
			Timeout:   proxyutil.DefaultDialTimeout,
			KeepAlive: proxyutil.DefaultDialKeepAlive,
		})
		if errSOCKS5 != nil {
			log.Errorf("codex websockets executor: create SOCKS5 dialer failed: %v", errSOCKS5)
			return dialer
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return socksDialer.Dial(network, addr)
		}
	case "http", "https":
		dialer.Proxy = http.ProxyURL(setting.URL)
	default:
		log.Errorf("codex websockets executor: unsupported proxy scheme: %s", setting.URL.Scheme)
	}

	return dialer
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	return parsed.String(), nil
}

func (e *CodexWebsocketsExecutor) applyCodexPromptCacheHeaders(ctx context.Context, from sdktranslator.Format, executionSessionID string, req cliproxyexecutor.Request, rawJSON []byte) ([]byte, http.Header) {
	headers := http.Header{}
	if len(rawJSON) == 0 {
		return rawJSON, headers
	}

	var resolution codexPromptCacheResolution
	if e != nil && e.CodexExecutor != nil {
		resolution = e.resolvePromptCacheResolution(ctx, from, executionSessionID, req)
	}

	if resolution.cache.ID != "" {
		rawJSON = codexSetPromptCacheKey(rawJSON, resolution.cache.ID)
		fallbackHeaderValue := resolution.cache.ID
		if resolution.headerEligibleID != "" {
			fallbackHeaderValue = resolution.headerEligibleID
		}
		sessionFallbackValue := fallbackHeaderValue
		if resolution.sessionHeaderID != "" {
			sessionFallbackValue = resolution.sessionHeaderID
		}
		threadFallbackValue := fallbackHeaderValue
		if resolution.threadHeaderID != "" {
			threadFallbackValue = resolution.threadHeaderID
		}
		if sessionHeaderValue := codexPromptCacheSessionHeaderValue(ctx, sessionFallbackValue); sessionHeaderValue != "" {
			headers.Set(codexHeaderSessionID, sessionHeaderValue)
		}
		if threadHeaderValue := codexPromptCacheThreadHeaderValue(ctx, threadFallbackValue); threadHeaderValue != "" {
			headers.Set(codexHeaderThreadID, threadHeaderValue)
		}
		if conversationID := strings.TrimSpace(fallbackHeaderValue); conversationID != "" {
			headers.Set("Conversation_id", conversationID)
		}
	}

	return rawJSON, headers
}

func codexExplicitWebsocketTurnMetadata(ctx context.Context, body []byte) string {
	if value := strings.TrimSpace(gjson.GetBytes(body, "client_metadata."+codexClientMetadataTurnMetadata).String()); value != "" {
		return value
	}
	if ctx != nil {
		return strings.TrimSpace(codexGinHeadersFromContext(ctx).Get(codexHeaderTurnMetadata))
	}
	return ""
}

func codexWebsocketTurnMetadataRequestKind(body []byte) string {
	if gjson.GetBytes(body, "generate").Type == gjson.False {
		return codexPrewarmRequestKind
	}
	return codexTurnRequestKind
}

func applyCodexWebsocketHeaders(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, token string, cfg *config.Config) http.Header {
	return applyCodexWebsocketHeadersForRequestKind(ctx, headers, auth, token, cfg, codexTurnRequestKind)
}

func applyCodexWebsocketHeadersForRequestKind(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, token string, cfg *config.Config, requestKind string) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}

	ginHeaders := codexGinHeadersFromContext(ctx)
	codexPinClientProfileFromFirstRequest(ctx, auth, headers, ginHeaders, cfg)
	codexPreparePinnedClientProfileHeaders(headers, auth)
	profileHeaders := codexClientProfileSourceHeaders(auth, ginHeaders)
	cfgUserAgent, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, profileHeaders, "x-codex-beta-features", cfgBetaFeatures, "")
	misc.EnsureHeader(headers, profileHeaders, "x-responsesapi-include-timing-metrics", "")
	if codexIncludeTimingMetrics(cfg) {
		headers.Set("x-responsesapi-include-timing-metrics", "true")
	}
	codexEnsureVersionHeader(headers, profileHeaders)
	misc.EnsureHeader(headers, profileHeaders, "x-openai-subagent", "")
	misc.EnsureHeader(headers, profileHeaders, codexHeaderOAIAttestation, "")

	headers.Set("OpenAI-Beta", codexResponsesWebsocketBetaHeaderValue)
	identity := codexResolvedIdentity(headers, profileHeaders, auth, cfg)
	headers.Set("User-Agent", identity.userAgent)
	sessionID := codexEnsureSessionHeaders(headers, ginHeaders, auth, codexSessionHeaderOptions{
		includeRequestID: true,
	})
	codexEnsureResponsesIdentityHeaders(headers, ginHeaders)
	codexEnsureTurnMetadataHeader(headers, ginHeaders, codexTurnMetadataDefaults{
		requestKind: strings.TrimSpace(requestKind),
		sessionID:   sessionID,
		threadID:    strings.TrimSpace(headers.Get(codexHeaderThreadID)),
		turnID:      uuid.NewString(),
		sandbox:     codexDefaultSandboxTag,
		windowID:    strings.TrimSpace(headers.Get(codexHeaderWindowID)),
	})
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-state", "")
	headers.Set("Originator", identity.originator)
	if !codexIsAPIKeyAuth(auth) {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				if trimmed := strings.TrimSpace(accountID); trimmed != "" {
					codexSetHeaderCasePreserved(headers, codexHeaderChatGPTAccountID, trimmed)
				}
			}
		}
	}
	codexEnsureFedrampHeader(headers, profileHeaders, auth)

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)
	codexEnsureVersionHeader(headers, nil)
	if cfgUserAgent != "" {
		headers.Set("User-Agent", cfgUserAgent)
	}

	return headers
}

func codexHeaderDefaults(cfg *config.Config, auth *cliproxyauth.Auth) (string, string) {
	if cfg == nil {
		return "", ""
	}
	userAgent := strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent)
	return userAgent, strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

func codexIncludeTimingMetrics(cfg *config.Config) bool {
	return cfg != nil && cfg.CodexHeaderDefaults.IncludeTimingMetrics != nil && *cfg.CodexHeaderDefaults.IncludeTimingMetrics
}

func ensureHeaderWithPriority(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

func ensureHeaderWithConfigPrecedence(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

func websocketUpgradeRequestLog(info helps.UpstreamRequestLog) helps.UpstreamRequestLog {
	upgradeInfo := info
	upgradeInfo.URL = helps.WebsocketUpgradeRequestURL(info.URL)
	upgradeInfo.Method = http.MethodGet
	upgradeInfo.Body = nil
	upgradeInfo.Headers = info.Headers.Clone()
	if upgradeInfo.Headers == nil {
		upgradeInfo.Headers = make(http.Header)
	}
	if strings.TrimSpace(upgradeInfo.Headers.Get("Connection")) == "" {
		upgradeInfo.Headers.Set("Connection", "Upgrade")
	}
	if strings.TrimSpace(upgradeInfo.Headers.Get("Upgrade")) == "" {
		upgradeInfo.Headers.Set("Upgrade", "websocket")
	}
	return upgradeInfo
}

func recordAPIWebsocketHandshake(ctx context.Context, cfg *config.Config, resp *http.Response) {
	if resp == nil {
		return
	}
	helps.RecordAPIWebsocketHandshake(ctx, cfg, resp.StatusCode, resp.Header)
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
}

func websocketHandshakeBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	body, _ := helps.ReadErrorResponseBody(resp.Body)
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
	if len(body) == 0 {
		return nil
	}
	return body
}

func closeHTTPResponseBody(resp *http.Response, logPrefix string) {
	if resp == nil || resp.Body == nil {
		return
	}
	if errClose := resp.Body.Close(); errClose != nil {
		log.Errorf("%s: %v", logPrefix, errClose)
	}
}

func executionSessionIDFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func (e *CodexWebsocketsExecutor) getOrCreateSession(sessionID string, reuseKey string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	if e == nil {
		return nil
	}
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	reuseKey = strings.TrimSpace(reuseKey)

	// Fast path: if the session already exists with a compatible reuseKey, a
	// read lock on sessionsMu is enough. This keeps concurrent request preparation
	// on distinct sessions from serializing behind other store mutations, and is
	// orthogonal to park/unpark traffic which now has its own lock.
	store.sessionsMu.RLock()
	if sess, ok := store.sessions[sessionID]; ok && sess != nil {
		if reuseKey == "" || sess.reuseKey == reuseKey {
			store.sessionsMu.RUnlock()
			return sess
		}
	}
	store.sessionsMu.RUnlock()

	store.sessionsMu.Lock()
	if store.sessions == nil {
		store.sessions = make(map[string]*codexWebsocketSession)
	}
	if sess, ok := store.sessions[sessionID]; ok && sess != nil {
		store.sessionsMu.Unlock()
		return sess
	}
	// Lock ordering: sessionsMu before parkedMu. Holding sessionsMu keeps other
	// acquirers from inserting the same sessionID while we rehome a parked
	// session.
	if reuseKey != "" {
		store.parkedMu.Lock()
		if store.parked == nil {
			store.parked = make(map[string]*codexWebsocketSession)
		}
		if sess, ok := store.parked[reuseKey]; ok && sess != nil {
			delete(store.parked, reuseKey)
			if sess.parkTimer != nil {
				sess.parkTimer.Stop()
				sess.parkTimer = nil
			}
			store.parkedMu.Unlock()
			sess.sessionID = sessionID
			sess.reuseKey = reuseKey
			store.sessions[sessionID] = sess
			store.sessionsMu.Unlock()
			return sess
		}
		store.parkedMu.Unlock()
	}
	sess := &codexWebsocketSession{sessionID: sessionID, reuseKey: reuseKey}
	store.sessions[sessionID] = sess
	store.sessionsMu.Unlock()
	return sess
}

func (e *CodexWebsocketsExecutor) resetSessionForReuseKey(sess *codexWebsocketSession, reuseKey string, reason string) {
	if sess == nil {
		return
	}
	reuseKey = strings.TrimSpace(reuseKey)
	if reuseKey == "" || sess.reuseKey == reuseKey {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "reuse_key_changed"
	}

	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sessionID := sess.sessionID
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sess.authID = ""
	sess.wsURL = ""
	sess.connMu.Unlock()

	sess.reuseKey = reuseKey
	sess.forceHTTPFallback.Store(false)
	sess.turnState.Store("")
	sess.turnStateScope.Store("")
	sess.clearIncrementalState()

	if conn == nil {
		return
	}
	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, nil)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func (e *CodexWebsocketsExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	sess := e.getOrCreateSession(sessionID, "")
	if sess == nil {
		return nil
	}
	return sess.upstreamDisconnectChan()
}

func (e *CodexWebsocketsExecutor) UpstreamDisconnectChanIfExists(sessionID string) <-chan error {
	sess := e.existingSession(sessionID)
	if sess == nil {
		return nil
	}
	return sess.upstreamDisconnectChan()
}

func (e *CodexWebsocketsExecutor) existingSession(sessionID string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil || sessionID == "" {
		return nil
	}
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	if store == nil {
		return nil
	}
	store.sessionsMu.RLock()
	sess := store.sessions[sessionID]
	store.sessionsMu.RUnlock()
	return sess
}

func (e *CodexWebsocketsExecutor) ensureUpstreamConn(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, authID string, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if sess == nil {
		return e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	}

	sess.connMu.Lock()
	conn := sess.conn
	readerConn := sess.readerConn
	sess.connMu.Unlock()
	if conn != nil {
		if sess.shouldRotate(time.Now()) {
			e.invalidateUpstreamConn(sess, conn, "connection_lifetime", nil)
			conn = nil
			readerConn = nil
		}
	}
	if conn != nil {
		// Validate reused session connections on first reuse and after sustained idleness.
		// Under steady traffic, per-request pings add measurable overhead without improving
		// liveness because recent reads/writes already prove the socket is healthy.
		if sess.shouldProbe(time.Now()) {
			if errProbe := sess.probeConn(conn); errProbe != nil {
				e.invalidateUpstreamConn(sess, conn, "probe_failed", errProbe)
				conn = nil
				readerConn = nil
			}
		}
	}
	if conn != nil {
		if readerConn != conn {
			sess.connMu.Lock()
			sess.readerConn = conn
			sess.connMu.Unlock()
			sess.configureConn(conn)
			go e.readUpstreamLoop(sess, conn)
		}
		return conn, nil, nil
	}

	conn, resp, errDial := e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	if errDial != nil {
		return nil, resp, errDial
	}

	sess.connMu.Lock()
	if sess.conn != nil {
		previous := sess.conn
		sess.connMu.Unlock()
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close websocket error: %v", errClose)
		}
		return previous, nil, nil
	}
	sess.conn = conn
	sess.wsURL = wsURL
	sess.authID = authID
	sess.readerConn = conn
	sess.connMu.Unlock()

	sess.configureConn(conn)
	sess.markOpened(time.Now())
	sess.markProbe(time.Now())
	go e.readUpstreamLoop(sess, conn)
	logCodexWebsocketConnected(sess.sessionID, authID, wsURL)
	return conn, resp, nil
}

func (e *CodexWebsocketsExecutor) readUpstreamLoop(sess *codexWebsocketSession, conn *websocket.Conn) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			codexMetrics.wsUpstreamError.Add(1)
			ch, done, active := sess.activeForConn(conn)
			if active {
				// Terminal error must reach the consumer; do NOT fall through a
				// `default` that silently drops it. The buffer is 32, and the
				// consumer either drains it or abandons the read by closing
				// `done`. Either way we stay wait-free for this goroutine.
				select {
				case ch <- codexWebsocketRead{conn: conn, err: errRead}:
				case <-done:
				}
				sess.clearActive(ch)
				close(ch)
			} else if sess.isCurrentConn(conn) {
				sess.notifyUpstreamDisconnect(errRead)
			}
			e.invalidateUpstreamConn(sess, conn, "upstream_disconnected", errRead)
			return
		}

		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				codexMetrics.wsUpstreamBinary.Add(1)
				errBinary := fmt.Errorf("codex websockets executor: unexpected binary message")
				ch, done, active := sess.activeForConn(conn)
				if active {
					// Same reasoning as the upstream-disconnect path above:
					// surfacing the terminal error to the consumer is the
					// whole point, silently dropping it is a correctness bug.
					select {
					case ch <- codexWebsocketRead{conn: conn, err: errBinary}:
					case <-done:
					}
					sess.clearActive(ch)
					close(ch)
				} else if sess.isCurrentConn(conn) {
					sess.notifyUpstreamDisconnect(errBinary)
				}
				e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
				return
			}
			continue
		}
		sess.touchActivity()

		ch, done, active := sess.activeForConn(conn)
		if !active {
			if sess.activeOwnedByAnotherConn(conn) {
				return
			}
			codexMetrics.wsActiveChMissing.Add(1)
			continue
		}
		select {
		case ch <- codexWebsocketRead{conn: conn, msgType: msgType, payload: payload}:
		case <-done:
		}
	}
}

func (e *CodexWebsocketsExecutor) activateSessionHTTPFallback(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error) {
	if sess == nil {
		return
	}
	sess.activateHTTPFallback()
	if conn != nil {
		e.invalidateUpstreamConn(sess, conn, reason, err)
	}
}

func (e *CodexWebsocketsExecutor) invalidateUpstreamConn(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error) {
	if sess == nil || conn == nil {
		return
	}

	sess.connMu.Lock()
	current := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sessionID := sess.sessionID
	if current == nil || current != conn {
		sess.connMu.Unlock()
		return
	}
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sess.connMu.Unlock()

	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, err)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func (e *CodexWebsocketsExecutor) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil {
		return
	}
	if sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		// Executor replacement can happen during hot reload (config/credential changes).
		// Do not force-close upstream websocket sessions here, otherwise in-flight
		// downstream websocket requests get interrupted.
		return
	}

	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.sessionsMu.Lock()
	sess := store.sessions[sessionID]
	delete(store.sessions, sessionID)
	store.sessionsMu.Unlock()

	if !e.parkExecutionSession(sess) {
		e.closeExecutionSession(sess, "session_closed")
	}
}

func (e *CodexWebsocketsExecutor) ResetExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil || sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		e.closeAllExecutionSessions("session_reset")
		return
	}

	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}

	toClose := make([]*codexWebsocketSession, 0, 2)
	store.sessionsMu.Lock()
	if sess := store.sessions[sessionID]; sess != nil {
		delete(store.sessions, sessionID)
		toClose = append(toClose, sess)
	}
	store.sessionsMu.Unlock()

	store.parkedMu.Lock()
	for reuseKey, sess := range store.parked {
		if sess == nil || strings.TrimSpace(sess.sessionID) != sessionID {
			continue
		}
		delete(store.parked, reuseKey)
		if sess.parkTimer != nil {
			sess.parkTimer.Stop()
			sess.parkTimer = nil
		}
		alreadyQueued := false
		for i := range toClose {
			if toClose[i] == sess {
				alreadyQueued = true
				break
			}
		}
		if !alreadyQueued {
			toClose = append(toClose, sess)
		}
	}
	store.parkedMu.Unlock()

	for i := range toClose {
		e.closeExecutionSession(toClose[i], "session_reset")
	}
}

func (e *CodexWebsocketsExecutor) closeAllExecutionSessions(reason string) {
	if e == nil {
		return
	}

	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.sessionsMu.Lock()
	sessions := make([]*codexWebsocketSession, 0, len(store.sessions))
	for sessionID, sess := range store.sessions {
		delete(store.sessions, sessionID)
		if sess != nil {
			sessions = append(sessions, sess)
		}
	}
	store.sessionsMu.Unlock()

	store.parkedMu.Lock()
	for reuseKey, sess := range store.parked {
		delete(store.parked, reuseKey)
		if sess != nil {
			if sess.parkTimer != nil {
				sess.parkTimer.Stop()
				sess.parkTimer = nil
			}
			sessions = append(sessions, sess)
		}
	}
	store.parkedMu.Unlock()

	for i := range sessions {
		e.closeExecutionSession(sessions[i], reason)
	}
}

func (e *CodexWebsocketsExecutor) closeExecutionSession(sess *codexWebsocketSession, reason string) {
	closeCodexWebsocketSession(sess, reason)
}

func (e *CodexWebsocketsExecutor) parkExecutionSession(sess *codexWebsocketSession) bool {
	if sess == nil {
		return false
	}
	if sess.httpFallbackActive() {
		return false
	}
	reuseKey := strings.TrimSpace(sess.reuseKey)
	if reuseKey == "" {
		return false
	}
	if sess.shouldRotate(time.Now().Add(codexResponsesWebsocketParkTTL)) {
		return false
	}

	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	if store == nil {
		return false
	}

	store.parkedMu.Lock()
	defer store.parkedMu.Unlock()
	if store.parked == nil {
		store.parked = make(map[string]*codexWebsocketSession)
	}
	if existing, ok := store.parked[reuseKey]; ok && existing != nil && existing != sess {
		if existing.parkTimer != nil {
			existing.parkTimer.Stop()
			existing.parkTimer = nil
		}
		go closeCodexWebsocketSession(existing, "parked_replaced")
	}
	if _, exists := store.parked[reuseKey]; !exists && len(store.parked) >= codexResponsesWebsocketMaxParked {
		evicted := evictOldestParkedCodexWebsocketSessionLocked(store)
		if evicted != nil {
			go closeCodexWebsocketSession(evicted, "parked_capacity")
		}
	}
	if sess.parkTimer != nil {
		sess.parkTimer.Stop()
	}
	store.parked[reuseKey] = sess
	sess.parkTimer = time.AfterFunc(codexResponsesWebsocketParkTTL, func() {
		store.parkedMu.Lock()
		current := store.parked[reuseKey]
		if current == sess {
			delete(store.parked, reuseKey)
			sess.parkTimer = nil
		}
		store.parkedMu.Unlock()
		if current == sess {
			closeCodexWebsocketSession(sess, "parked_ttl_expired")
		}
	})
	return true
}

func evictOldestParkedCodexWebsocketSessionLocked(store *codexWebsocketSessionStore) *codexWebsocketSession {
	if store == nil || len(store.parked) == 0 {
		return nil
	}
	var oldestKey string
	var oldest *codexWebsocketSession
	var oldestActivity int64
	for key, sess := range store.parked {
		if sess == nil {
			delete(store.parked, key)
			continue
		}
		activity := sess.lastActivityUnixNano.Load()
		if oldest == nil || activity < oldestActivity {
			oldestKey = key
			oldest = sess
			oldestActivity = activity
		}
	}
	if oldest == nil {
		return nil
	}
	delete(store.parked, oldestKey)
	if oldest.parkTimer != nil {
		oldest.parkTimer.Stop()
		oldest.parkTimer = nil
	}
	return oldest
}

func closeCodexWebsocketSession(sess *codexWebsocketSession, reason string) {
	if sess == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "session_closed"
	}
	if sess.parkTimer != nil {
		sess.parkTimer.Stop()
		sess.parkTimer = nil
	}

	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sessionID := sess.sessionID
	sess.connMu.Unlock()

	if conn == nil {
		return
	}
	logCodexWebsocketDisconnected(sessionID, authID, wsURL, reason, nil)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("codex websockets executor: close websocket error: %v", errClose)
	}
}

func logCodexWebsocketConnected(sessionID string, authID string, wsURL string) {
	log.Debugf("codex websockets: upstream connected session=%s auth=%s url=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL))
}

func logCodexWebsocketDisconnected(sessionID string, authID string, wsURL string, reason string, err error) {
	if err != nil {
		// Errors remain at Info since they surface actionable failures.
		log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s err=%v", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason), err)
		return
	}
	log.Debugf("codex websockets: upstream disconnected session=%s auth=%s url=%s reason=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason))
}

// CloseCodexWebsocketSessionsForAuthID closes all active Codex upstream websocket sessions
// associated with the supplied auth ID.
func CloseCodexWebsocketSessionsForAuthID(authID string, reason string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "auth_removed"
	}

	store := globalCodexWebsocketSessionStore
	if store == nil {
		return
	}

	type sessionItem struct {
		sessionID string
		sess      *codexWebsocketSession
	}

	store.sessionsMu.RLock()
	items := make([]sessionItem, 0, len(store.sessions))
	for sessionID, sess := range store.sessions {
		items = append(items, sessionItem{sessionID: sessionID, sess: sess})
	}
	store.sessionsMu.RUnlock()

	store.parkedMu.Lock()
	for sessionID, sess := range store.parked {
		items = append(items, sessionItem{sessionID: sessionID, sess: sess})
	}
	store.parkedMu.Unlock()

	matches := make([]sessionItem, 0)
	for i := range items {
		sess := items[i].sess
		if sess == nil {
			continue
		}
		sess.connMu.Lock()
		sessAuthID := strings.TrimSpace(sess.authID)
		sess.connMu.Unlock()
		if sessAuthID == authID {
			matches = append(matches, items[i])
		}
	}
	if len(matches) == 0 {
		return
	}

	toClose := make([]*codexWebsocketSession, 0, len(matches))
	store.sessionsMu.Lock()
	for i := range matches {
		current, ok := store.sessions[matches[i].sessionID]
		if ok && current != nil && current == matches[i].sess {
			delete(store.sessions, matches[i].sessionID)
			toClose = append(toClose, current)
		}
	}
	store.sessionsMu.Unlock()

	store.parkedMu.Lock()
	for i := range matches {
		// The sessionsMu pass above already handled matches living in the
		// active map, so those will no longer be in store.parked. Skip quickly
		// when the lookup there finds a different session or none at all.
		current, ok := store.parked[matches[i].sessionID]
		if !ok || current == nil || current != matches[i].sess {
			continue
		}
		delete(store.parked, matches[i].sessionID)
		if current.parkTimer != nil {
			current.parkTimer.Stop()
			current.parkTimer = nil
		}
		toClose = append(toClose, current)
	}
	store.parkedMu.Unlock()

	for i := range toClose {
		closeCodexWebsocketSession(toClose[i], reason)
	}
}
