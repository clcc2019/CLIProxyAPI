// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

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
	codexClientMetadataWSStreamRequestStartMS = "x-codex-ws-stream-request-start-ms"
	codexResponsesWebsocketIdleTimeout        = 5 * time.Minute
	// Rotate before the documented 60 minute upstream connection limit so the
	// next turn opens a fresh socket instead of discovering the cap mid-chain.
	codexResponsesWebsocketMaxLifetime    = 55 * time.Minute
	codexResponsesWebsocketHandshakeTO    = 30 * time.Second
	codexResponsesWebsocketWriteTO        = 30 * time.Second
	codexResponsesWebsocketProbeIdle      = 45 * time.Second
	codexResponsesWebsocketReadBuffer     = 8
	codexResponsesWebsocketReadLimit      = 64 << 20
	codexResponsesWebsocketMaxParked      = 16
	codexWebsocketHeaderInitialCapacity   = 12
	codexWebsocketSSEFrameInitialCapacity = 512
	codexDefaultResponsesHTTPURL          = "https://chatgpt.com/backend-api/codex/responses"
	codexDefaultResponsesWebsocketURL     = "wss://chatgpt.com/backend-api/codex/responses"
)

var (
	codexResponsesWebsocketParkTTL      = 10 * time.Second
	codexResponsesWebsocketProbeTimeout = 5 * time.Second
)

var codexWebsocketWriteBufferPool sync.Pool

var codexWebsocketSSEPrefix = []byte("data: ")

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
	// sessionID and reuseKey identify which logical execution session this
	// object is currently bound to. That binding is mutable: unparking rehomes
	// a session (and its live connection) onto a new sessionID under
	// sessionsMu, while resetSessionForReuseKey rebinds reuseKey under reqMu,
	// and the readUpstreamLoop goroutine — which outlives a park — reads both
	// under connMu or while holding no lock at all for logging. Four access
	// paths guarded by three different mutexes provide no mutual exclusion, so
	// these are atomics rather than plain fields; see setIdentity/sessionIDOf.
	sessionIDValue atomic.Value
	reuseKeyValue  atomic.Value

	reqMu sync.Mutex

	connMu sync.Mutex
	conn   *websocket.Conn
	wsURL  string
	authID string
	// proxyPolicy is a credential-safe fingerprint of the effective proxy
	// selection used when conn was established. It is protected by connMu.
	proxyPolicy string

	writeMu sync.Mutex
	probeMu sync.Mutex
	// probePongConn and probePongCh bind pong acknowledgements to the connection
	// being validated. probeSequence makes every probe payload unique so a
	// delayed pong from an earlier probe cannot validate a stale socket.
	probePongConn *websocket.Conn
	probePongCh   chan string
	probeSequence atomic.Uint64

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

	parkTimer *time.Timer

	upstreamDisconnectMu sync.Mutex
	upstreamDisconnectCh chan error
}

// sessionID returns the execution session this object is currently bound to.
func (s *codexWebsocketSession) sessionID() string {
	if s == nil {
		return ""
	}
	value, _ := s.sessionIDValue.Load().(string)
	return value
}

// reuseKey returns the connection reuse key this object is currently bound to.
func (s *codexWebsocketSession) reuseKey() string {
	if s == nil {
		return ""
	}
	value, _ := s.reuseKeyValue.Load().(string)
	return value
}

func (s *codexWebsocketSession) setSessionID(sessionID string) {
	if s != nil {
		s.sessionIDValue.Store(sessionID)
	}
}

func (s *codexWebsocketSession) setReuseKey(reuseKey string) {
	if s != nil {
		s.reuseKeyValue.Store(reuseKey)
	}
}

// newCodexWebsocketSession builds a session bound to the given identity.
func newCodexWebsocketSession(sessionID, reuseKey string) *codexWebsocketSession {
	sess := &codexWebsocketSession{}
	sess.setSessionID(sessionID)
	sess.setReuseKey(reuseKey)
	return sess
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return NewCodexWebsocketsExecutorWithResponseObserver(cfg, nil)
}

// NewCodexWebsocketsExecutorWithResponseObserver creates a WebSocket Codex
// executor whose embedded HTTP fallback observes the same response metadata.
func NewCodexWebsocketsExecutorWithResponseObserver(cfg *config.Config, observer CodexResponseObserver) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutorWithResponseObserver(cfg, observer),
		store:         globalCodexWebsocketSessionStore,
	}
}

type codexWebsocketRead struct {
	conn    *websocket.Conn
	msgType int
	frame   []byte
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

func (s *codexWebsocketSession) rememberTurnStateEvent(payload []byte) {
	if s == nil {
		return
	}
	if state := codexWebsocketTurnStateFromEvent(payload); state != "" {
		s.turnState.Store(state)
	}
}

// codexWebsocketTurnStateFromEvent mirrors codex-rs's response.metadata
// handling. WebSocket turn state may arrive in an event rather than in the
// upgrade response headers and must be replayed for later requests in the
// same turn.
func codexWebsocketTurnStateFromEvent(payload []byte) string {
	if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "type").String()), "response.metadata") {
		return ""
	}
	headers := gjson.GetBytes(payload, "headers")
	if !headers.IsObject() {
		return ""
	}
	state := ""
	headers.ForEach(func(key, value gjson.Result) bool {
		if !strings.EqualFold(strings.TrimSpace(key.String()), codexHeaderTurnState) {
			return true
		}
		switch {
		case value.Type == gjson.String:
			state = strings.TrimSpace(value.String())
		case value.IsArray():
			values := value.Array()
			if len(values) > 0 {
				state = strings.TrimSpace(values[0].String())
			}
		}
		return false
	})
	return state
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
	inputResult := codexGJSONGetImmutableBytes(request, "input")
	s.lastRequestCmp, _ = codexComparableRequestWithoutInputWithInputResult(request, inputResult)
	if inputItems, ok := codexRawArrayItemViews(request, inputResult, 0, 16); ok {
		s.lastRequestInput = inputItems
	} else {
		s.lastRequestInput = nil
	}
}

func (s *codexWebsocketSession) rememberCompletedResponse(eventData []byte) {
	if s == nil || len(bytes.TrimSpace(eventData)) == 0 {
		return
	}
	response := codexGJSONGetImmutableBytes(eventData, "response")
	s.lastResponseID = strings.Clone(strings.TrimSpace(response.Get("id").String()))
	output := response.Get("output")
	if output.Exists() && output.IsArray() {
		s.lastResponseOutput = []byte(output.Raw)
		if outputItems, ok := codexRawArrayItemViews(s.lastResponseOutput, output, output.Index, 4); ok {
			s.lastResponseItems = outputItems
			return
		}
	}
	s.lastResponseOutput = []byte("[]")
	s.lastResponseItems = make([][]byte, 0)
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
	pongCh := make(chan string, 8)
	s.probeMu.Lock()
	s.probePongConn = conn
	s.probePongCh = pongCh
	s.probeMu.Unlock()
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
	conn.SetPongHandler(func(appData string) error {
		s.touchActivity()
		select {
		case pongCh <- appData:
		default:
		}
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

func (s *codexWebsocketSession) probeConn(ctx context.Context, conn *websocket.Conn) error {
	if s == nil {
		return fmt.Errorf("codex websockets executor: session is nil")
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.probePongConn != conn || s.probePongCh == nil {
		return fmt.Errorf("codex websockets executor: websocket pong monitor is unavailable")
	}
	pongCh := s.probePongCh
	for {
		select {
		case <-pongCh:
			continue
		default:
		}
		break
	}

	timeout := codexResponsesWebsocketProbeTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	now := time.Now()
	deadline := now.Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if !deadline.After(now) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	probePayload := "cliproxy:" + strconv.FormatUint(s.probeSequence.Add(1), 36)
	s.writeMu.Lock()
	errWrite := conn.WriteControl(websocket.PingMessage, []byte(probePayload), deadline)
	s.writeMu.Unlock()
	if errWrite != nil {
		return errWrite
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("codex websockets executor: websocket pong timeout after %s", timeout)
		case pongPayload := <-pongCh:
			if pongPayload != probePayload {
				continue
			}
			s.touchActivity()
			s.markProbe(time.Now())
			return nil
		}
	}
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
	body, originalTranslated, _ := codexTranslateRequestWithOriginal(e.cfg, ctx, from, to, baseModel, req.Payload, originalPayload, false, opts.Headers)

	body, err = applyCodexThinking(body, req, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth, opts.Headers)
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
	attempt := e.connectPreparedCodexWebsocket(ctx, auth, prepared)
	if attempt.err != nil {
		if attempt.upgradeRequired() {
			e.activateSessionHTTPFallback(sess, nil, "upgrade_required", attempt.err)
			prepared.unlockSession()
			return e.CodexExecutor.Execute(ctx, auth, req, opts)
		}
		if attempt.unauthorized() && !codexUnauthorizedRetryAlreadyUsed(ctx) {
			refreshedAuth, retried, refreshErr := e.CodexExecutor.recoverCodexAuthAfterUnauthorized(ctx, auth, attempt.statusCode(), attempt.responseBody)
			if refreshErr != nil {
				return resp, refreshErr
			}
			if retried {
				prepared.unlockSession()
				return e.Execute(contextWithCodexUnauthorizedRetryUsed(ctx), refreshedAuth, req, opts)
			}
		}
		return resp, attempt.failure(ctx, e, auth)
	}
	conn := attempt.conn
	reporter.SetCodexResponseMetadata(
		codexResponseHeaderValue(attempt.responseHeaders, codexHeaderOpenAIModel),
		codexResponseHeaderPresent(attempt.responseHeaders, codexHeaderReasoningIncluded),
	)
	if sess == nil {
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
	usageWarningFilter := newCodexUsageWarningStreamFilter()
	previousResponseRetryUsed := false
	readRetryUsed := false
readLoop:
	for {
		if ctx != nil && ctx.Err() != nil {
			return resp, ctx.Err()
		}
		msgType, payload, _, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
		if errRead != nil {
			mappedErr := mapCodexWebsocketReadError(errRead)
			if sess != nil && !readRetryUsed && (ctx == nil || ctx.Err() == nil) && !isCodexWebsocketMessageTooBigError(errRead) {
				readRetryUsed = true
				connRetry, wsReqBodyRetry, errRetry := e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBody, "read_error", mappedErr)
				if errRetry == nil {
					conn = connRetry
					wsReqBody = wsReqBodyRetry
					streamState = newCodexStreamCompletionState()
					continue readLoop
				}
				sess.clearIncrementalState()
				helps.RecordAPIWebsocketError(ctx, e.cfg, "read_retry", errRetry)
				return resp, errRetry
			}
			if sess != nil {
				sess.clearIncrementalState()
				e.invalidateUpstreamConn(sess, conn, "read_error", mappedErr)
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "read", mappedErr)
			return resp, mappedErr
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
		if sess != nil {
			sess.rememberTurnStateEvent(payload)
		}
		if codexConsumesUpstreamControlEvent(ctx, auth, payload) {
			continue
		}

		if wsErr, ok := parseCodexWebsocketError(payload); ok {
			codexPublishRateLimitsFromErrorBody(ctx, auth, payload)
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
				connRetry, wsReqBodyRetry, errRetry := e.retryCodexWebsocketWithoutPreviousResponse(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, &wsReqLog, body)
				if errRetry != nil {
					if sess != nil {
						sess.clearIncrementalState()
						e.invalidateUpstreamConn(sess, conn, "upstream_error", errRetry)
					}
					helps.RecordAPIWebsocketError(ctx, e.cfg, "previous_response_not_found_retry", errRetry)
					return resp, errRetry
				}
				conn = connRetry
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

		payload, eventType := normalizeCodexWebsocketCompletion(payload)
		reporter.SetCodexResponseMetadata(codexServerModelFromResponseData(payload), false)
		events := usageWarningFilter.Filter(eventType, payload)
		if len(events) == 0 {
			continue
		}

		for _, event := range events {
			payload := event.payload
			eventType := event.eventType
			if eventType == "response.incomplete" {
				terminalErr := codexResponseIncompleteEventErr(payload)
				codexPublishRateLimitsFromErrorBody(ctx, auth, payload)
				if sess != nil {
					sess.clearIncrementalState()
					e.invalidateUpstreamConn(sess, conn, "upstream_incomplete", terminalErr)
				}
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_incomplete", terminalErr)
				return resp, terminalErr
			}
			if terminalErr, ok := parseCodexStreamTerminalError(eventType, payload); ok {
				codexPublishRateLimitsFromErrorBody(ctx, auth, payload)
				retryErrorPayload := payload
				if eventType == "response.failed" {
					retryErrorPayload = normalizeCodexResponseFailedErrorBody(payload)
				}
				if !previousResponseRetryUsed && codexShouldRetryWithoutPreviousResponse(body, wsReqBody, retryErrorPayload) {
					previousResponseRetryUsed = true
					helps.LogWithRequestID(ctx).Debugf("codex websockets executor: retrying without previous_response_id after upstream terminal event rejected incremental context")
					connRetry, wsReqBodyRetry, errRetry := e.retryCodexWebsocketWithoutPreviousResponse(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, &wsReqLog, body)
					if errRetry != nil {
						if sess != nil {
							sess.clearIncrementalState()
							e.invalidateUpstreamConn(sess, conn, "upstream_error", errRetry)
						}
						helps.RecordAPIWebsocketError(ctx, e.cfg, "previous_response_not_found_retry", errRetry)
						return resp, errRetry
					}
					conn = connRetry
					wsReqBody = wsReqBodyRetry
					streamState = newCodexStreamCompletionState()
					continue readLoop
				}
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
				reporter.SetCodexResponseMetadata(codexServerModelFromResponseData(payload), false)
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
				return resp, nil
			}
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
	body, originalTranslated, _ := codexTranslateRequestWithOriginal(e.cfg, ctx, from, to, baseModel, req.Payload, originalPayload, true, opts.Headers)

	body, err = applyCodexThinking(body, req, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth, opts.Headers)
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
	attempt := e.connectPreparedCodexWebsocket(ctx, auth, prepared)
	if attempt.err != nil {
		if attempt.upgradeRequired() {
			e.activateSessionHTTPFallback(sess, nil, "upgrade_required", attempt.err)
			prepared.unlockSession()
			return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
		}
		if attempt.unauthorized() && !codexUnauthorizedRetryAlreadyUsed(ctx) {
			refreshedAuth, retried, refreshErr := e.CodexExecutor.recoverCodexAuthAfterUnauthorized(ctx, auth, attempt.statusCode(), attempt.responseBody)
			if refreshErr != nil {
				prepared.unlockSession()
				return nil, refreshErr
			}
			if retried {
				prepared.unlockSession()
				return e.ExecuteStream(contextWithCodexUnauthorizedRetryUsed(ctx), refreshedAuth, req, opts)
			}
		}
		prepared.unlockSession()
		return nil, attempt.failure(ctx, e, auth)
	}
	conn := attempt.conn
	upstreamHeaders := attempt.responseHeaders
	reporter.SetCodexResponseMetadata(
		codexResponseHeaderValue(upstreamHeaders, codexHeaderOpenAIModel),
		codexResponseHeaderPresent(upstreamHeaders, codexHeaderReasoningIncluded),
	)

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
		usageWarningFilter := newCodexUsageWarningStreamFilter()
		emittedPayload := false
		previousResponseRetryUsed := false
		readRetryUsed := false
	streamReadLoop:
		for {
			if ctx != nil && ctx.Err() != nil {
				terminateReason = "context_done"
				terminateErr = ctx.Err()
				_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
				return
			}
			msgType, payload, sseLine, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
			if errRead != nil {
				if sess != nil && ctx != nil && ctx.Err() != nil {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
					return
				}
				mappedErr := mapCodexWebsocketReadError(errRead)
				if sess != nil && !readRetryUsed && !emittedPayload && !isCodexWebsocketMessageTooBigError(errRead) {
					readRetryUsed = true
					connRetry, wsReqBodyRetry, errRetry := e.retrySessionWebsocketRequestWithReason(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, wsReqLog, wsReqBody, "read_error", mappedErr)
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
				terminateErr = mappedErr
				if sess != nil {
					sess.clearIncrementalState()
					e.invalidateUpstreamConn(sess, conn, "read_error", mappedErr)
				}
				helps.RecordAPIWebsocketError(ctx, e.cfg, "read", mappedErr)
				reporter.PublishFailureWithError(ctx, mappedErr)
				_ = send(cliproxyexecutor.StreamChunk{Err: mappedErr})
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

			messagePayload := payload
			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			helps.AppendAPIWebsocketResponse(ctx, e.cfg, payload)
			if sess != nil {
				sess.rememberTurnStateEvent(payload)
			}
			if codexConsumesUpstreamControlEvent(ctx, auth, payload) {
				continue
			}

			if wsErr, ok := parseCodexWebsocketError(payload); ok {
				codexPublishRateLimitsFromErrorBody(ctx, auth, payload)
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
				if !previousResponseRetryUsed && codexShouldRetryWithoutPreviousResponse(body, wsReqBody, payload) {
					previousResponseRetryUsed = true
					helps.LogWithRequestID(ctx).Debugf("codex websockets executor: retrying without previous_response_id after upstream rejected incremental context")
					connRetry, wsReqBodyRetry, errRetry := e.retryCodexWebsocketWithoutPreviousResponse(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, &wsReqLog, body)
					if errRetry != nil {
						if sess != nil {
							sess.clearIncrementalState()
						}
						terminateReason = "previous_response_not_found_retry_error"
						terminateErr = errRetry
						helps.RecordAPIWebsocketError(ctx, e.cfg, "previous_response_not_found_retry", errRetry)
						reporter.PublishFailureWithError(ctx, errRetry)
						_ = send(cliproxyexecutor.StreamChunk{Err: errRetry})
						return
					}
					conn = connRetry
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

			payload, eventType := normalizeCodexWebsocketCompletion(payload)
			reporter.SetCodexResponseMetadata(codexServerModelFromResponseData(payload), false)
			events := usageWarningFilter.Filter(eventType, payload)
			if len(events) == 0 {
				continue
			}

			for _, event := range events {
				payload := event.payload
				eventType := event.eventType
				if eventType == "response.incomplete" {
					terminalErr := codexResponseIncompleteEventErr(payload)
					codexPublishRateLimitsFromErrorBody(ctx, auth, payload)
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
					codexPublishRateLimitsFromErrorBody(ctx, auth, payload)
					retryErrorPayload := payload
					if eventType == "response.failed" {
						retryErrorPayload = normalizeCodexResponseFailedErrorBody(payload)
					}
					if !previousResponseRetryUsed && codexShouldRetryWithoutPreviousResponse(body, wsReqBody, retryErrorPayload) {
						previousResponseRetryUsed = true
						helps.LogWithRequestID(ctx).Debugf("codex websockets executor: retrying without previous_response_id after upstream terminal event rejected incremental context")
						connRetry, wsReqBodyRetry, errRetry := e.retryCodexWebsocketWithoutPreviousResponse(ctx, auth, sess, conn, &readCh, authID, wsURL, wsHeaders, &wsReqLog, body)
						if errRetry != nil {
							if sess != nil {
								sess.clearIncrementalState()
							}
							terminateReason = "previous_response_not_found_retry_error"
							terminateErr = errRetry
							helps.RecordAPIWebsocketError(ctx, e.cfg, "previous_response_not_found_retry", errRetry)
							reporter.PublishFailureWithError(ctx, errRetry)
							_ = send(cliproxyexecutor.StreamChunk{Err: errRetry})
							return
						}
						conn = connRetry
						wsReqBody = wsReqBodyRetry
						streamState = newCodexStreamCompletionState()
						continue streamReadLoop
					}
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
					reporter.SetCodexResponseMetadata(codexServerModelFromResponseData(payload), false)
					if detail, ok := helps.ParseCodexUsage(payload); ok {
						reporter.Publish(ctx, detail)
					}
				}

				line := sseLine
				if len(line) == 0 || !codexSameByteView(payload, messagePayload) {
					line = encodeCodexWebsocketAsSSE(payload)
				}
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
					return
				}
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
	ginHeaders := codexGinHeadersFromContext(ctx)
	body = normalizeCodexFinalUpstreamBody(body, baseModel, auth, codexFinalUpstreamBodyOptions{
		requestKind:                codexFinalUpstreamResponses,
		streamMode:                 codexStreamFieldTrue,
		preservePreviousResponseID: true,
		preserveGenerate:           true,
		preserveNativeFields: codexNativeClientRequest(opts.SourceFormat, opts.Headers, body) ||
			codexNativeClientRequest(opts.SourceFormat, ginHeaders, body),
		store:           codexShouldStoreResponses(auth, httpURL),
		omitServiceTier: auth == nil || !auth.ServiceTierPassthrough(),
	})

	executionSessionID := executionSessionIDFromOptions(opts)
	body = codexSanitizeForcedUpstreamSessionBody(ctx, body)
	body, wsHeaders, promptCacheID := e.applyCodexPromptCacheHeaders(ctx, opts.SourceFormat, executionSessionID, req, body)
	codexApplyForcedUpstreamSessionHeaders(ctx, wsHeaders)
	responsesAPIClientMetadata := codexResponsesAPIClientMetadataFromBody(body)
	explicitTurnMetadata := ""
	if codexForcedUpstreamSessionID(ctx) == "" {
		explicitTurnMetadata = codexExplicitWebsocketTurnMetadata(ctx, body)
	}
	if explicitTurnMetadata != "" && trimHeaderValue(wsHeaders, codexHeaderTurnMetadata) == "" {
		codexSetSingleHeaderValue(wsHeaders, codexHeaderTurnMetadata, explicitTurnMetadata)
	}
	codexEnsureExecutionSessionHeader(wsHeaders, codexGinHeadersFromContext(ctx), executionSessionID)
	authorization, err := e.CodexExecutor.codexAuthorization(ctx, auth, apiKey)
	if err != nil {
		return nil, err
	}
	wsHeaders = applyCodexWebsocketHeadersForRequestKind(ctx, wsHeaders, auth, authorization, e.cfg, codexWebsocketTurnMetadataRequestKind(body))
	// See the HTTP path: normal header preparation accepts caller aliases, so
	// restore a forced session only after that normalization is complete.
	codexApplyForcedUpstreamSessionHeaders(ctx, wsHeaders)
	codexApplyModelHeaderOverrides(wsHeaders, baseModel)
	codexApplyResponsesLiteHeader(wsHeaders, baseModel, auth)
	codexMergeResponsesAPIClientMetadataIntoTurnMetadataHeader(wsHeaders, responsesAPIClientMetadata)
	turnStateScope := trimHeaderValue(wsHeaders, codexHeaderTurnMetadata)
	if explicitTurnMetadata != "" {
		turnStateScope = explicitTurnMetadata
	}
	body = codexApplyWebsocketClientMetadataWithResponseCreateType(ctx, body, wsHeaders, auth, e.cfg, strconv.FormatInt(time.Now().UnixMilli(), 10))
	wsHeaders.Del("Traceparent")
	wsHeaders.Del("Tracestate")

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	prepared := &codexPreparedWebsocketRequest{
		body:               body,
		wsURL:              wsURL,
		wsHeaders:          wsHeaders,
		authID:             authID,
		executionSessionID: executionSessionID,
	}
	if prepared.executionSessionID != "" {
		prepared.reuseKey = codexWebsocketReusableKeyFromParts(
			authID,
			wsURL,
			promptCacheID,
			trimHeaderValue(wsHeaders, codexHeaderWindowID),
			codexWebsocketProxyPolicyFingerprint(e.cfg, auth),
		)
		prepared.sess = e.getOrCreateSession(prepared.executionSessionID, prepared.reuseKey)
		if prepared.sess != nil {
			prepared.sess.reqMu.Lock()
			prepared.sessionLocked = true
			if prepared.reuseKey != "" && prepared.sess.reuseKey() != "" && prepared.sess.reuseKey() != prepared.reuseKey {
				e.resetSessionForReuseKey(prepared.sess, prepared.reuseKey, "reuse_key_changed")
			}
			if prepared.reuseKey != "" && prepared.sess.reuseKey() == "" {
				prepared.sess.setReuseKey(prepared.reuseKey)
			}
			prepared.httpFallback = prepared.sess.httpFallbackActive()
			if !prepared.httpFallback {
				prepared.sess.setTurnStateScope(turnStateScope)
				prepared.sess.applyTurnStateHeader(prepared.wsHeaders)
			}
		}
	}

	if !prepared.httpFallback && prepared.sess != nil {
		if incrementalBody, ok := buildCodexIncrementalWebsocketRequestBody(prepared.sess, body, wsHeaders.Get("X-Codex-Turn-Metadata")); ok {
			prepared.wsReqBody = incrementalBody
		}
	}
	if len(prepared.wsReqBody) == 0 {
		if codexWebsocketRequestBodyReady(body) {
			prepared.wsReqBody = body
		} else {
			prepared.wsReqBody = buildCodexWebsocketRequestBodyWithCurrentTurnMetadata(body)
		}
	}
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
	codexSetSingleHeaderValue(headers, codexHeaderOfficialSessionID, executionSessionID)
}

func codexWebsocketReusableKey(_ sdktranslator.Format, authID string, wsURL string, body []byte) string {
	promptCacheID := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	windowID := strings.TrimSpace(gjson.GetBytes(body, "client_metadata."+codexClientMetadataWindowID).String())
	return codexWebsocketReusableKeyFromParts(authID, wsURL, promptCacheID, windowID)
}

func codexWebsocketReusableKeyFromParts(authID string, wsURL string, promptCacheID string, windowID string, proxyPolicy ...string) string {
	promptCacheID = strings.TrimSpace(promptCacheID)
	if promptCacheID == "" {
		return ""
	}
	authID = strings.TrimSpace(authID)
	wsURL = strings.TrimSpace(wsURL)
	if authID == "" || wsURL == "" {
		return ""
	}
	policySegment := ""
	if len(proxyPolicy) > 0 {
		if fingerprint := strings.TrimSpace(proxyPolicy[0]); fingerprint != "" {
			policySegment = "|proxy=" + fingerprint
		}
	}
	windowID = strings.TrimSpace(windowID)
	if windowID == "" {
		return authID + "|" + wsURL + policySegment + "|" + promptCacheID
	}
	return authID + "|" + wsURL + policySegment + "|" + promptCacheID + "|" + windowID
}

func (e *CodexWebsocketsExecutor) retryCodexWebsocketWithoutPreviousResponse(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	sess *codexWebsocketSession,
	conn *websocket.Conn,
	readCh *chan codexWebsocketRead,
	authID string,
	wsURL string,
	wsHeaders http.Header,
	wsReqLog *helps.UpstreamRequestLog,
	body []byte,
) (*websocket.Conn, []byte, error) {
	retryBody := buildCodexWebsocketRetryWithoutPreviousResponse(body, wsHeaders.Get(codexHeaderTurnMetadata), time.Now())
	if wsReqLog != nil {
		wsReqLog.Body = retryBody
		helps.RecordAPIWebsocketRequest(ctx, e.cfg, *wsReqLog)
	}
	errSend := writeCodexWebsocketMessage(sess, conn, retryBody)
	if errSend == nil {
		return conn, retryBody, nil
	}
	if sess == nil {
		return nil, nil, errSend
	}
	requestLog := helps.UpstreamRequestLog{}
	if wsReqLog != nil {
		requestLog = *wsReqLog
	}
	return e.retrySessionWebsocketRequestWithReason(
		ctx,
		auth,
		sess,
		conn,
		readCh,
		authID,
		wsURL,
		wsHeaders,
		requestLog,
		retryBody,
		"previous_response_not_found",
		errSend,
	)
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
	if respHSRetry != nil {
		codexPublishRateLimitsFromHeaders(ctx, auth, respHSRetry.Header)
	}
	if (errDialRetry != nil || connRetry == nil) && respHSRetry != nil &&
		respHSRetry.StatusCode == http.StatusUnauthorized && codexIsAgentIdentityAuth(auth) {
		responseHeaders := respHSRetry.Header.Clone()
		bodyErr := websocketHandshakeBody(respHSRetry)
		respHSRetry = nil
		helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), http.StatusUnauthorized, responseHeaders, bodyErr)
		codexPublishRateLimitsFromErrorBody(ctx, auth, bodyErr)
		recovered, errRecover := e.CodexExecutor.recoverCodexAgentIdentityTask(ctx, auth, http.StatusUnauthorized, bodyErr)
		if errRecover != nil {
			return nil, nil, fmt.Errorf("codex websockets executor: recover agent identity task during reconnect: %w", errRecover)
		}
		if !recovered {
			retryErr := statusErrWithHeaders{
				statusErr: newCodexStatusErr(http.StatusUnauthorized, bodyErr),
				headers:   responseHeaders,
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "dial_retry", retryErr)
			return nil, nil, retryErr
		}
		connRetry, respHSRetry, errDialRetry = e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
		if respHSRetry != nil {
			codexPublishRateLimitsFromHeaders(ctx, auth, respHSRetry.Header)
		}
	}
	if errDialRetry != nil || connRetry == nil {
		retryErr := errDialRetry
		if respHSRetry != nil && respHSRetry.StatusCode > 0 {
			bodyErr := websocketHandshakeBody(respHSRetry)
			helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHSRetry.StatusCode, respHSRetry.Header, bodyErr)
			codexPublishRateLimitsFromErrorBody(ctx, auth, bodyErr)
			retryErr = statusErrWithHeaders{
				statusErr: newCodexStatusErr(respHSRetry.StatusCode, bodyErr),
				headers:   respHSRetry.Header.Clone(),
			}
		} else {
			closeHTTPResponseBody(respHSRetry, "codex websockets executor: close handshake response body error")
			if retryErr == nil {
				retryErr = fmt.Errorf("codex websockets executor: retry websocket conn is nil")
			}
		}
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial_retry", retryErr)
		return nil, nil, retryErr
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
	if ctx == nil {
		ctx = context.Background()
	}
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	// Agent assertions contain a timestamp and must be regenerated for every
	// handshake. Session reconnects can happen long after the request was first
	// prepared, so reusing the original header may send an expired assertion.
	if codexIsAgentIdentityAuth(auth) {
		authorization, errAuthorization := e.CodexExecutor.codexAuthorization(ctx, auth, "")
		if errAuthorization != nil {
			return nil, nil, errAuthorization
		}
		headers.Set("Authorization", authorization)
	}
	helps.AddChatGPTCloudflareCookies(headers, wsURL)
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if resp != nil {
		helps.StoreChatGPTCloudflareCookies(wsURL, resp.Cookies())
	}
	if conn != nil {
		conn.SetReadLimit(codexResponsesWebsocketReadLimit)
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

func buildCodexWebsocketRequestBody(body []byte, turnMetadataHeader string) []byte {
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	body = codexEnsureResponsesContextField(body, codexFinalUpstreamResponses)
	body = helps.SanitizeCodexInputItemIDs(body)

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

func buildCodexWebsocketRequestBodyWithCurrentTurnMetadata(body []byte) []byte {
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	body = codexEnsureResponsesContextField(body, codexFinalUpstreamResponses)
	body = helps.SanitizeCodexInputItemIDs(body)

	typeResult := gjson.GetBytes(body, "type")
	if strings.TrimSpace(typeResult.String()) == "response.create" {
		return body
	}
	if !typeResult.Exists() {
		if updated, ok := codexAppendTopLevelStringField(body, "type", "response.create"); ok {
			return updated
		}
	}
	if updated, err := sjson.SetBytes(body, "type", "response.create"); err == nil && len(updated) > 0 {
		return updated
	}
	return body
}

func codexWebsocketRequestBodyReady(body []byte) bool {
	return strings.TrimSpace(codexGJSONGetImmutableBytes(body, "type").String()) == "response.create"
}

func buildCodexIncrementalWebsocketRequestBody(sess *codexWebsocketSession, body []byte, turnMetadataHeader string) ([]byte, bool) {
	if sess == nil || len(sess.lastRequestCmp) == 0 {
		return nil, false
	}
	previousResponseID := strings.TrimSpace(sess.lastResponseID)
	if previousResponseID == "" {
		return nil, false
	}

	inputResult := codexGJSONGetImmutableBytes(body, "input")
	currentComparable, ok := codexComparableRequestWithoutInputWithInputResult(body, inputResult)
	if !ok || !codexJSONRawEqual(sess.lastRequestCmp, currentComparable) {
		return nil, false
	}

	previousInput := sess.lastRequestInput
	if previousInput == nil {
		return nil, false
	}
	responseOutput := sess.lastResponseItems
	if responseOutput == nil {
		return nil, false
	}

	delta, ok := codexIncrementalInputDeltaViews(body, inputResult, previousInput, responseOutput)
	if !ok {
		return nil, false
	}
	if !codexWebsocketDeltaToolOutputsAnchorable(previousInput, responseOutput, delta) {
		return nil, false
	}
	if updated, ok := buildCodexIncrementalWebsocketRequestBodyFast(body, inputResult, delta, previousResponseID, turnMetadataHeader); ok {
		return updated, true
	}
	updated, err := sjson.SetRawBytes(body, "input", codexRawJSONArray(delta))
	if err != nil {
		return nil, false
	}
	updated, err = sjson.SetBytes(updated, "previous_response_id", previousResponseID)
	if err != nil || len(updated) == 0 {
		return nil, false
	}
	return buildCodexWebsocketRequestBody(updated, turnMetadataHeader), true
}

func codexIncrementalInputDeltaViews(source []byte, inputResult gjson.Result, previousInput [][]byte, responseOutput [][]byte) ([][]byte, bool) {
	if !inputResult.Exists() || !inputResult.IsArray() {
		return nil, false
	}
	baselineLen := len(previousInput) + len(responseOutput)
	delta := make([][]byte, 0, 1)
	index := 0
	valid := true
	inputResult.ForEach(func(_, item gjson.Result) bool {
		start := item.Index
		end := start + len(item.Raw)
		if start < 0 || end < start || end > len(source) {
			valid = false
			return false
		}
		itemView := source[start:end]
		switch {
		case index < len(previousInput):
			valid = codexJSONRawEqualIgnoringInternalChatMessageMetadata(itemView, previousInput[index])
		case index < baselineLen:
			valid = codexJSONRawEqualIgnoringInternalChatMessageMetadata(itemView, responseOutput[index-len(previousInput)])
		default:
			delta = append(delta, itemView)
		}
		index++
		return valid
	})
	if !valid || index < baselineLen {
		return nil, false
	}
	return delta, true
}

func buildCodexIncrementalWebsocketRequestBodyFast(body []byte, inputResult gjson.Result, delta [][]byte, previousResponseID string, turnMetadataHeader string) ([]byte, bool) {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if previousResponseID == "" || !inputResult.Exists() || !inputResult.IsArray() {
		return nil, false
	}
	if codexTopLevelHasTypeOrPreviousResponseID(body, inputResult) {
		return nil, false
	}
	turnMetadataHeader = strings.TrimSpace(turnMetadataHeader)
	if turnMetadataHeader != "" && gjson.GetBytes(body, "client_metadata."+codexClientMetadataTurnMetadata).String() != turnMetadataHeader {
		return nil, false
	}

	trimmed, suffix, hasFields, ok := codexPrepareTopLevelObjectAppend(body)
	if !ok || !hasFields {
		return nil, false
	}
	start, end, ok := codexJSONResultRawRange(body, inputResult)
	if !ok || end > len(trimmed)-1 {
		return nil, false
	}

	deltaRaw := codexRawJSONArray(delta)
	updated := make([]byte, 0, len(body)-len(inputResult.Raw)+len(deltaRaw)+len(previousResponseID)+len(`,"previous_response_id":"","type":"response.create"`))
	updated = append(updated, body[:start]...)
	updated = append(updated, deltaRaw...)
	updated = append(updated, body[end:len(trimmed)-1]...)
	updated = append(updated, ',')
	updated = strconv.AppendQuote(updated, "previous_response_id")
	updated = append(updated, ':')
	updated = strconv.AppendQuote(updated, previousResponseID)
	updated = append(updated, ',')
	updated = strconv.AppendQuote(updated, "type")
	updated = append(updated, ':')
	updated = strconv.AppendQuote(updated, "response.create")
	updated = append(updated, '}')
	updated = append(updated, suffix...)
	return updated, true
}

func codexTopLevelHasTypeOrPreviousResponseID(data []byte, inputResult gjson.Result) bool {
	i := codexSkipJSONSpaces(data, 0)
	if i >= len(data) || data[i] != '{' {
		return true
	}
	i++
	inputStart, inputEnd, hasInputRange := codexJSONResultRawRange(data, inputResult)
	for {
		i = codexSkipJSONSpaces(data, i)
		if i >= len(data) {
			return true
		}
		if data[i] == '}' {
			return false
		}
		keyStart, keyEnd, keyEscaped, next, ok := codexParseJSONStringRaw(data, i)
		if !ok {
			return true
		}
		if !keyEscaped {
			key := data[keyStart:keyEnd]
			if bytes.Equal(key, codexJSONKeyType) || bytes.Equal(key, codexJSONKeyPreviousID) {
				return true
			}
		}
		i = codexSkipJSONSpaces(data, next)
		if i >= len(data) || data[i] != ':' {
			return true
		}
		valueStart := codexSkipJSONSpaces(data, i+1)
		if valueStart >= len(data) {
			return true
		}
		valueNext := 0
		if !keyEscaped && bytes.Equal(data[keyStart:keyEnd], codexJSONKeyInput) && hasInputRange && inputStart == valueStart {
			valueNext = inputEnd
		} else {
			valueNext, ok = codexSkipJSONValue(data, valueStart)
			if !ok {
				return true
			}
		}
		i = codexSkipJSONSpaces(data, valueNext)
		if i >= len(data) {
			return true
		}
		switch data[i] {
		case ',':
			i++
		case '}':
			return false
		default:
			return true
		}
	}
}

func codexWebsocketDeltaToolOutputsAnchorable(previousInput [][]byte, responseOutput [][]byte, delta [][]byte) bool {
	needsAnchor := false
	for _, item := range delta {
		itemType := codexWebsocketRawItemType(item)
		if !codexWebsocketIsToolCallOutputType(itemType) {
			continue
		}
		if itemType == "tool_search_output" && codexWebsocketToolSearchOutputCanStandAlone(item) {
			continue
		}
		needsAnchor = true
		break
	}
	if !needsAnchor {
		return true
	}

	knownCalls := make(map[string]string)
	rememberCalls := func(items [][]byte) {
		for _, item := range items {
			itemType := codexWebsocketRawItemType(item)
			if !codexWebsocketIsToolCallType(itemType) {
				continue
			}
			if callID := codexWebsocketRawItemCallID(item); callID != "" {
				knownCalls[callID] = itemType
			}
		}
	}
	rememberCalls(previousInput)
	rememberCalls(responseOutput)
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
	return codexComparableRequestWithoutInputWithInputResult(body, gjson.Result{})
}

func codexComparableRequestWithoutInputWithInputResult(body []byte, inputResult gjson.Result) ([]byte, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	if comparable, ok := codexComparableRequestWithoutInputFast(body, inputResult); ok {
		return comparable, true
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
	comparable = bytes.TrimSpace(comparable)
	if len(comparable) == 0 || !gjson.ValidBytes(comparable) {
		return nil, false
	}
	return bytes.Clone(comparable), true
}

func codexComparableRequestWithoutInputFast(data []byte, inputResult gjson.Result) ([]byte, bool) {
	i := codexSkipJSONSpaces(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, false
	}
	i++
	inputStart, inputEnd, hasInputRange := codexJSONResultRawRange(data, inputResult)

	capacity := len(data)
	if capacity > 1024 {
		capacity = 1024
	}
	out := make([]byte, 0, capacity)
	out = append(out, '{')
	wrote := false
	appendField := func(keyRaw []byte, valueRaw []byte) {
		if wrote {
			out = append(out, ',')
		}
		out = append(out, keyRaw...)
		out = append(out, ':')
		out = append(out, bytes.TrimSpace(valueRaw)...)
		wrote = true
	}

	for {
		i = codexSkipJSONSpaces(data, i)
		if i >= len(data) {
			return nil, false
		}
		if data[i] == '}' {
			out = append(out, '}')
			i = codexSkipJSONSpaces(data, i+1)
			if i != len(data) {
				return nil, false
			}
			return out, true
		}

		keyRawStart := i
		keyStart, keyEnd, keyEscaped, next, ok := codexParseJSONStringRaw(data, i)
		if !ok || keyEscaped {
			return nil, false
		}
		keyRaw := data[keyRawStart:next]
		key := data[keyStart:keyEnd]
		i = codexSkipJSONSpaces(data, next)
		if i >= len(data) || data[i] != ':' {
			return nil, false
		}
		valueStart := codexSkipJSONSpaces(data, i+1)
		if valueStart >= len(data) {
			return nil, false
		}
		inputKey := bytes.Equal(key, codexJSONKeyInput)
		valueNext := 0
		if inputKey && hasInputRange && inputStart == valueStart {
			valueNext = inputEnd
		} else {
			valueNext, ok = codexSkipJSONValue(data, valueStart)
			if !ok {
				return nil, false
			}
		}
		valueRaw := data[valueStart:valueNext]

		switch {
		case inputKey, bytes.Equal(key, codexJSONKeyGenerate):
		case bytes.Equal(key, codexJSONKeyMetadata):
			metadataRaw, keep, ok := codexComparableClientMetadataRaw(valueRaw)
			if !ok {
				return nil, false
			}
			if keep {
				appendField(keyRaw, metadataRaw)
			}
		default:
			appendField(keyRaw, valueRaw)
		}

		i = codexSkipJSONSpaces(data, valueNext)
		if i >= len(data) {
			return nil, false
		}
		switch data[i] {
		case ',':
			i++
		case '}':
			out = append(out, '}')
			i = codexSkipJSONSpaces(data, i+1)
			if i != len(data) {
				return nil, false
			}
			return out, true
		default:
			return nil, false
		}
	}
}

func codexComparableClientMetadataRaw(raw []byte) ([]byte, bool, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false, false
	}
	if raw[0] != '{' {
		return raw, true, true
	}
	i := codexSkipJSONSpaces(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, false, false
	}
	i++

	capacity := len(raw)
	if capacity > 256 {
		capacity = 256
	}
	out := make([]byte, 0, capacity)
	out = append(out, '{')
	wrote := false
	appendField := func(keyRaw []byte, valueRaw []byte) {
		if wrote {
			out = append(out, ',')
		}
		out = append(out, keyRaw...)
		out = append(out, ':')
		out = append(out, bytes.TrimSpace(valueRaw)...)
		wrote = true
	}

	for {
		i = codexSkipJSONSpaces(raw, i)
		if i >= len(raw) {
			return nil, false, false
		}
		if raw[i] == '}' {
			i = codexSkipJSONSpaces(raw, i+1)
			if i != len(raw) {
				return nil, false, false
			}
			if !wrote {
				return nil, false, true
			}
			out = append(out, '}')
			return out, true, true
		}

		keyRawStart := i
		keyStart, keyEnd, keyEscaped, next, ok := codexParseJSONStringRaw(raw, i)
		if !ok || keyEscaped {
			return nil, false, false
		}
		keyRaw := raw[keyRawStart:next]
		key := raw[keyStart:keyEnd]
		i = codexSkipJSONSpaces(raw, next)
		if i >= len(raw) || raw[i] != ':' {
			return nil, false, false
		}
		valueStart := codexSkipJSONSpaces(raw, i+1)
		if valueStart >= len(raw) {
			return nil, false, false
		}
		valueNext, ok := codexSkipJSONValue(raw, valueStart)
		if !ok {
			return nil, false, false
		}
		if !codexComparableSkipClientMetadataKey(key) {
			appendField(keyRaw, raw[valueStart:valueNext])
		}
		i = codexSkipJSONSpaces(raw, valueNext)
		if i >= len(raw) {
			return nil, false, false
		}
		switch raw[i] {
		case ',':
			i++
		case '}':
			i = codexSkipJSONSpaces(raw, i+1)
			if i != len(raw) {
				return nil, false, false
			}
			if !wrote {
				return nil, false, true
			}
			out = append(out, '}')
			return out, true, true
		default:
			return nil, false, false
		}
	}
}

func codexComparableSkipClientMetadataKey(key []byte) bool {
	return bytes.Equal(key, codexJSONKeyMetadataSession) ||
		bytes.Equal(key, codexJSONKeyMetadataThread) ||
		bytes.Equal(key, codexJSONKeyMetadataTurnID) ||
		bytes.Equal(key, codexJSONKeyMetadataTurn) ||
		bytes.Equal(key, codexJSONKeyMetadataTrace) ||
		bytes.Equal(key, codexJSONKeyMetadataTraceStat) ||
		bytes.Equal(key, codexJSONKeyMetadataStartMS)
}

func codexRawArrayItems(result gjson.Result) ([][]byte, bool) {
	if !result.Exists() || !result.IsArray() {
		return nil, false
	}
	results := result.Array()
	items := make([][]byte, len(results))
	for i := range results {
		items[i] = []byte(results[i].Raw)
	}
	return items, true
}

func codexRawArrayItemViews(source []byte, result gjson.Result, sourceOffset int, capacityHint int) ([][]byte, bool) {
	if !result.Exists() || !result.IsArray() {
		return nil, false
	}
	if capacityHint < 0 {
		capacityHint = 0
	}
	items := make([][]byte, 0, capacityHint)
	valid := true
	result.ForEach(func(_, item gjson.Result) bool {
		start := item.Index - sourceOffset
		end := start + len(item.Raw)
		if start < 0 || end < start || end > len(source) {
			valid = false
			return false
		}
		items = append(items, source[start:end])
		return true
	})
	if !valid {
		return nil, false
	}
	return items, true
}

// codexGJSONGetImmutableBytes avoids gjson.GetBytes' defensive copy. Callers
// must keep source alive and immutable while using the returned result.
func codexGJSONGetImmutableBytes(source []byte, path string) gjson.Result {
	if len(source) == 0 {
		return gjson.Result{}
	}
	return gjson.Get(unsafe.String(unsafe.SliceData(source), len(source)), path)
}

// codexGJSONParseImmutableBytes parses without gjson.ParseBytes' defensive
// []byte-to-string copy. The result must not outlive or mutate source.
func codexGJSONParseImmutableBytes(source []byte) gjson.Result {
	if len(source) == 0 {
		return gjson.Result{}
	}
	return gjson.Parse(unsafe.String(unsafe.SliceData(source), len(source)))
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

func codexJSONRawEqualIgnoringInternalChatMessageMetadata(left []byte, right []byte) bool {
	if codexJSONRawEqual(left, right) {
		return true
	}
	leftStripped, leftOK := codexStripInternalChatMessageMetadata(left)
	rightStripped, rightOK := codexStripInternalChatMessageMetadata(right)
	if !leftOK || !rightOK {
		return false
	}
	return codexJSONRawEqual(leftStripped, rightStripped)
}

func codexStripInternalChatMessageMetadata(item []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(item)) == 0 {
		return nil, false
	}
	stripped, err := sjson.DeleteBytes(item, "internal_chat_message_metadata_passthrough")
	if err != nil {
		return nil, false
	}
	return stripped, true
}

func readCodexWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (int, []byte, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sess == nil {
		if conn == nil {
			return 0, nil, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		return readCodexWebsocketFrame(conn)
	}
	if conn == nil {
		return 0, nil, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	if readCh == nil {
		return 0, nil, nil, fmt.Errorf("codex websockets executor: session read channel is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return 0, nil, nil, ctx.Err()
		case ev, ok := <-readCh:
			if !ok {
				return 0, nil, nil, fmt.Errorf("codex websockets executor: session read channel closed")
			}
			if ev.conn != conn {
				continue
			}
			if ev.err != nil {
				return 0, nil, nil, ev.err
			}
			if !bytes.HasPrefix(ev.frame, codexWebsocketSSEPrefix) {
				return 0, nil, nil, fmt.Errorf("codex websockets executor: invalid framed websocket payload")
			}
			return ev.msgType, ev.frame[len(codexWebsocketSSEPrefix):], ev.frame, nil
		}
	}
}

func readCodexWebsocketFrame(conn *websocket.Conn) (int, []byte, []byte, error) {
	msgType, reader, err := conn.NextReader()
	if err != nil {
		return msgType, nil, nil, err
	}
	payload, sseLine, err := readCodexWebsocketPayloadWithSSE(reader)
	return msgType, payload, sseLine, err
}

// readCodexWebsocketPayloadWithSSE reserves the downstream SSE prefix in the
// same allocation as the websocket payload. The raw JSON view is used for
// event processing, while sseLine can be forwarded without copying when the
// event remains unchanged.
func readCodexWebsocketPayloadWithSSE(reader io.Reader) ([]byte, []byte, error) {
	line := make([]byte, len(codexWebsocketSSEPrefix), codexWebsocketSSEFrameInitialCapacity)
	copy(line, codexWebsocketSSEPrefix)
	next := 256
	chunks := make([][]byte, 0, 4)
	finalSize := 0
	for {
		n, err := reader.Read(line[len(line):cap(line)])
		line = line[:len(line)+n]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			if len(chunks) == 0 {
				return line[len(codexWebsocketSSEPrefix):], line, err
			}
			finalSize += len(line)
			framed := make([]byte, finalSize)
			offset := 0
			for _, chunk := range chunks {
				offset += copy(framed[offset:], chunk)
			}
			copy(framed[offset:], line)
			return framed[len(codexWebsocketSSEPrefix):], framed, err
		}
		if cap(line)-len(line) < cap(line)/16 {
			chunks = append(chunks, line)
			finalSize += len(line)
			line = make([]byte, 0, next)
			next += next / 2
		}
	}
}

func codexSameByteView(left, right []byte) bool {
	return len(left) == len(right) && (len(left) == 0 || unsafe.SliceData(left) == unsafe.SliceData(right))
}

// codexWebsocketDialerCache memoises constructed websocket.Dialer instances by
// (proxyURL, envCAFingerprint) so that hot paths do not re-parse the proxy URL
// and re-read the CA pool on every dial. The Dialer itself is immutable after
// construction apart from its Proxy/NetDialContext funcs, both of which are
// goroutine-safe to call concurrently.
var codexWebsocketDialerCache sync.Map

func newProxyAwareWebsocketDialer(cfg *config.Config, auth *cliproxyauth.Auth) *websocket.Dialer {
	proxyURL := codexWebsocketProxyURL(cfg, auth)
	// Never retain a proxy URL (and therefore possible credentials) in a
	// process-global cache key. The full digest still distinguishes credential
	// rotations and proxy target changes without exposing either value.
	cacheKey := codexWebsocketDialerCacheKey(cfg, auth)
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

// NewProxyAwareWebsocketDialer returns a WebSocket dialer that honors the
// credential-level and global proxy settings used by upstream executors. It is
// shared by raw WebSocket endpoints whose protocol is not a Responses stream.
func NewProxyAwareWebsocketDialer(cfg *config.Config, auth *cliproxyauth.Auth) *websocket.Dialer {
	return newProxyAwareWebsocketDialer(cfg, auth)
}

func codexWebsocketDialerCacheKey(cfg *config.Config, auth *cliproxyauth.Auth) string {
	return codexWebsocketProxyPolicyFingerprint(cfg, auth) + "\x00" + misc.CustomRootCAsEnvFingerprint()
}

func codexWebsocketProxyURL(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

// codexWebsocketProxyPolicyFingerprint returns a credential-safe identity for
// the effective websocket proxy policy. An empty explicit setting means
// "inherit ProxyFromEnvironment"; the environment itself is intentionally not
// copied into the key because net/http snapshots that process-level policy and
// proxy environment variables may contain credentials.
func codexWebsocketProxyPolicyFingerprint(cfg *config.Config, auth *cliproxyauth.Auth) string {
	proxyURL := codexWebsocketProxyURL(cfg, auth)
	canonical := "inherit"
	if proxyURL != "" {
		setting, errParse := proxyutil.Parse(proxyURL)
		switch {
		case errParse != nil:
			canonical = "invalid\x00" + proxyURL
		case setting.Mode == proxyutil.ModeDirect:
			canonical = "direct"
		case setting.Mode == proxyutil.ModeProxy && setting.URL != nil:
			canonical = "proxy\x00" + setting.URL.String()
		default:
			canonical = "invalid\x00" + proxyURL
		}
	}
	digest := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%x", digest)
}

func blockCodexWebsocketDialer(dialer *websocket.Dialer, err error) *websocket.Dialer {
	if dialer == nil {
		dialer = &websocket.Dialer{}
	}
	if err == nil {
		err = fmt.Errorf("invalid websocket proxy configuration")
	}
	blockedErr := fmt.Errorf("codex websockets executor: proxy policy rejected connection: %w", err)
	dialer.Proxy = func(*http.Request) (*url.URL, error) {
		return nil, blockedErr
	}
	dialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, blockedErr
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
		return blockCodexWebsocketDialer(dialer, errParse)
	}

	switch setting.Mode {
	case proxyutil.ModeDirect:
		dialer.Proxy = nil
		return dialer
	case proxyutil.ModeProxy:
	default:
		return blockCodexWebsocketDialer(dialer, fmt.Errorf("invalid proxy mode"))
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
			return blockCodexWebsocketDialer(dialer, errSOCKS5)
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return proxyutil.DialContext(ctx, socksDialer, network, addr)
		}
	case "http", "https":
		dialer.Proxy = http.ProxyURL(setting.URL)
	default:
		errUnsupported := fmt.Errorf("unsupported proxy scheme: %s", setting.URL.Scheme)
		log.Errorf("codex websockets executor: %v", errUnsupported)
		return blockCodexWebsocketDialer(dialer, errUnsupported)
	}

	return dialer
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	httpURL = strings.TrimSpace(httpURL)
	if httpURL == codexDefaultResponsesHTTPURL {
		return codexDefaultResponsesWebsocketURL, nil
	}
	if rest, ok := strings.CutPrefix(httpURL, "https://"); ok {
		return "wss://" + rest, nil
	}
	if rest, ok := strings.CutPrefix(httpURL, "http://"); ok {
		return "ws://" + rest, nil
	}
	parsed, err := url.Parse(httpURL)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(parsed.Scheme, "http") {
		parsed.Scheme = "ws"
	} else if strings.EqualFold(parsed.Scheme, "https") {
		parsed.Scheme = "wss"
	}
	return parsed.String(), nil
}

func (e *CodexWebsocketsExecutor) applyCodexPromptCacheHeaders(ctx context.Context, from sdktranslator.Format, executionSessionID string, req cliproxyexecutor.Request, rawJSON []byte) ([]byte, http.Header, string) {
	headers := make(http.Header, codexWebsocketHeaderInitialCapacity)
	if len(rawJSON) == 0 {
		return rawJSON, headers, ""
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
			codexSetSingleHeaderValue(headers, codexHeaderOfficialSessionID, sessionHeaderValue)
		}
		if threadHeaderValue := codexPromptCacheThreadHeaderValue(ctx, threadFallbackValue); threadHeaderValue != "" {
			codexSetSingleHeaderValue(headers, codexHeaderOfficialThreadID, threadHeaderValue)
		}
	}

	return rawJSON, headers, resolution.cache.ID
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
		headers = make(http.Header, codexRequestHeaderInitialCapacity)
	}
	authorization := codexAuthorizationHeaderValue(auth, token)
	if authorization != "" {
		codexSetSingleHeaderValue(headers, "Authorization", authorization)
	} else {
		headers.Del("Authorization")
	}

	ginHeaders := codexGinHeadersFromContext(ctx)
	codexPinClientProfileFromFirstRequest(ctx, auth, headers, ginHeaders, cfg)
	codexPreparePinnedClientProfileHeaders(headers, auth)
	profileHeaders := codexClientProfileSourceHeaders(auth, ginHeaders)
	cfgUserAgent, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, profileHeaders, "X-Codex-Beta-Features", cfgBetaFeatures, "")
	codexEnsureHeader(headers, profileHeaders, codexWireHeaderResponsesAPIIncludeTimingMetrics, "")
	if codexIncludeTimingMetrics(cfg) {
		codexSetSingleHeaderValue(headers, codexWireHeaderResponsesAPIIncludeTimingMetrics, "true")
	}
	codexEnsureVersionHeader(headers, profileHeaders)
	codexEnsureHeader(headers, profileHeaders, codexWireHeaderOpenAISubagent, "")
	codexEnsureHeader(headers, profileHeaders, codexWireHeaderOAIAttestation, "")

	codexSetSingleHeaderValue(headers, codexWireHeaderOpenAIBeta, codexResponsesWebsocketBetaHeaderValue)
	identity := codexIdentity(headers, profileHeaders, auth, cfgUserAgent)
	codexSetSingleHeaderValue(headers, "User-Agent", identity.userAgent)
	sessionID := codexEnsureSessionHeaders(headers, ginHeaders, auth, codexSessionHeaderOptions{
		includeRequestID: true,
	})
	codexEnsureResponsesIdentityHeaders(headers, ginHeaders)
	installationID := codexResolvedInstallationID(headers, ginHeaders, auth, cfg)
	codexEnsureTurnMetadataHeader(headers, ginHeaders, codexTurnMetadataDefaults{
		installationID: installationID,
		requestKind:    strings.TrimSpace(requestKind),
		sessionID:      sessionID,
		threadID:       codexThreadIdentityHeaderValue(headers),
		turnID:         uuid.NewString(),
		sandbox:        codexDefaultSandboxTag,
		windowID:       trimHeaderValue(headers, codexHeaderWindowID),
	})
	codexEnsureHeader(headers, ginHeaders, codexHeaderTurnState, "")
	codexSetOriginatorHeader(headers, identity.originator)
	apiKeyAuth := codexIsAPIKeyAuth(auth)
	if accountID := codexAccountID(auth, apiKeyAuth); accountID != "" {
		codexSetHeaderCasePreserved(headers, codexHeaderChatGPTAccountID, accountID)
	}
	codexEnsureFedramp(headers, profileHeaders, auth, apiKeyAuth)

	attrs := codexClientProfileCustomHeaderAttrs(auth)
	if util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs) {
		codexEnsureVersionHeader(headers, nil)
		if cfgUserAgent != "" {
			codexSetSingleHeaderValue(headers, "User-Agent", cfgUserAgent)
		}
	}
	if codexIsAgentIdentityAuth(auth) && authorization != "" {
		codexSetSingleHeaderValue(headers, "Authorization", authorization)
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
	if trimHeaderValue(target, key) != "" {
		return
	}
	if source != nil {
		if val := trimHeaderValue(source, key); val != "" {
			codexSetSingleHeaderValue(target, key, val)
			return
		}
	}
	if val := strings.TrimSpace(configValue); val != "" {
		codexSetSingleHeaderValue(target, key, val)
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		codexSetSingleHeaderValue(target, key, val)
	}
}

func codexEnsureHeader(target http.Header, source http.Header, key, defaultValue string) {
	if target == nil {
		return
	}
	if source != nil {
		if val := trimHeaderValue(source, key); val != "" {
			codexSetSingleHeaderValue(target, key, val)
			return
		}
	}
	if trimHeaderValue(target, key) != "" {
		return
	}
	if val := strings.TrimSpace(defaultValue); val != "" {
		codexSetSingleHeaderValue(target, key, val)
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
		if reuseKey == "" || sess.reuseKey() == reuseKey {
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
			sess.setSessionID(sessionID)
			sess.setReuseKey(reuseKey)
			store.sessions[sessionID] = sess
			store.sessionsMu.Unlock()
			return sess
		}
		store.parkedMu.Unlock()
	}
	sess := newCodexWebsocketSession(sessionID, reuseKey)
	store.sessions[sessionID] = sess
	store.sessionsMu.Unlock()
	return sess
}

func (e *CodexWebsocketsExecutor) resetSessionForReuseKey(sess *codexWebsocketSession, reuseKey string, reason string) {
	if sess == nil {
		return
	}
	reuseKey = strings.TrimSpace(reuseKey)
	if reuseKey == "" || sess.reuseKey() == reuseKey {
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
	sessionID := sess.sessionID()
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sess.authID = ""
	sess.wsURL = ""
	sess.proxyPolicy = ""
	sess.connMu.Unlock()

	sess.setReuseKey(reuseKey)
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

// detachMismatchedWebsocketSessionConn atomically removes a connection that
// was established for a different credential, upstream URL, or proxy policy.
// Closing happens after releasing connMu so the reader goroutine cannot turn a
// controlled policy switch into a downstream disconnect notification.
func detachMismatchedWebsocketSessionConn(sess *codexWebsocketSession, authID string, wsURL string, proxyPolicy string) (*websocket.Conn, string, string, string) {
	if sess == nil {
		return nil, "", "", ""
	}

	authID = strings.TrimSpace(authID)
	wsURL = strings.TrimSpace(wsURL)
	proxyPolicy = strings.TrimSpace(proxyPolicy)

	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	conn := sess.conn
	if conn == nil {
		return nil, "", "", ""
	}

	storedAuthID := strings.TrimSpace(sess.authID)
	storedWSURL := strings.TrimSpace(sess.wsURL)
	storedProxyPolicy := strings.TrimSpace(sess.proxyPolicy)
	targetChanged := storedAuthID != authID || storedWSURL != wsURL
	proxyChanged := storedProxyPolicy != proxyPolicy
	if !targetChanged && !proxyChanged {
		return nil, "", "", ""
	}

	previousAuthID := sess.authID
	previousWSURL := sess.wsURL
	sess.conn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	reason := "proxy_policy_changed"
	if targetChanged {
		reason = "target_changed"
	}
	return conn, previousAuthID, previousWSURL, reason
}

func (e *CodexWebsocketsExecutor) ensureUpstreamConn(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, authID string, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if sess == nil {
		return e.dialCodexWebsocket(ctx, auth, wsURL, headers)
	}
	proxyPolicy := codexWebsocketProxyPolicyFingerprint(e.cfg, auth)

	if staleConn, staleAuthID, staleWSURL, reason := detachMismatchedWebsocketSessionConn(sess, authID, wsURL, proxyPolicy); staleConn != nil {
		logCodexWebsocketDisconnected(sess.sessionID(), staleAuthID, staleWSURL, reason, nil)
		if errClose := staleConn.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close stale websocket error: %v", errClose)
		}
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
			if errProbe := sess.probeConn(ctx, conn); errProbe != nil {
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
	sess.proxyPolicy = proxyPolicy
	sess.readerConn = conn
	sess.connMu.Unlock()

	sess.configureConn(conn)
	sess.markOpened(time.Now())
	sess.markProbe(time.Now())
	go e.readUpstreamLoop(sess, conn)
	logCodexWebsocketConnected(sess.sessionID(), authID, wsURL)
	return conn, resp, nil
}

func (e *CodexWebsocketsExecutor) readUpstreamLoop(sess *codexWebsocketSession, conn *websocket.Conn) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, _, sseLine, errRead := readCodexWebsocketFrame(conn)
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
		case ch <- codexWebsocketRead{conn: conn, msgType: msgType, frame: sseLine}:
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
	sessionID := sess.sessionID()
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
		if sess == nil || strings.TrimSpace(sess.sessionID()) != sessionID {
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
	reuseKey := strings.TrimSpace(sess.reuseKey())
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
	sessionID := sess.sessionID()
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
