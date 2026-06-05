package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const requestScopedNotFoundMessage = "Item with id 'rs_0b5f3eb6f51f175c0169ca74e4a85881998539920821603a74' not found. Items are not persisted when `store` is set to false. Try again with `store` set to true, or remove this item from your input."

func TestManager_ShouldRetryAfterError_RespectsAuthRequestRetryOverride(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"request_retry": float64(0),
		},
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, _, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for request_retry=0, got true (wait=%v)", wait)
	}

	auth.Metadata["request_retry"] = float64(1)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	wait, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true for request_retry=1, got false")
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}

	_, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 1, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false on attempt=1 for request_retry=1, got true")
	}
}

func TestManager_ShouldRetryAfterError_UsesOAuthModelAliasForCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"kimi": {
			{Name: "deepseek-v3.1", Alias: "pool-model"},
		},
	})

	routeModel := "pool-model"
	upstreamModel := "deepseek-v3.1"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "kimi",
		ModelStates: map[string]*ModelState{
			upstreamModel: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, _, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 429, Message: "quota"}, 0, []string{"kimi"}, routeModel, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true, got false (wait=%v)", wait)
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}
}

type credentialRetryLimitExecutor struct {
	id string

	mu    sync.Mutex
	calls int
}

func (e *credentialRetryLimitExecutor) Identifier() string {
	return e.id
}

func (e *credentialRetryLimitExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordCall()
	return nil, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *credentialRetryLimitExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordCall()
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "boom"}
}

func (e *credentialRetryLimitExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *credentialRetryLimitExecutor) recordCall() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
}

func (e *credentialRetryLimitExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type unauthorizedFailoverSessionExecutor struct {
	failFirstStatus  int
	failFirstMessage string

	mu     sync.Mutex
	calls  int
	forced []string
}

func (e *unauthorizedFailoverSessionExecutor) Identifier() string { return "codex" }

func (e *unauthorizedFailoverSessionExecutor) Execute(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	forced := ""
	if len(opts.Metadata) > 0 {
		forced, _ = opts.Metadata[cliproxyexecutor.ForcedUpstreamSessionMetadataKey].(string)
	}
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.forced = append(e.forced, strings.TrimSpace(forced))
	e.mu.Unlock()
	if call == 1 && e.failFirstStatus > 0 {
		message := strings.TrimSpace(e.failFirstMessage)
		if message == "" {
			message = http.StatusText(e.failFirstStatus)
		}
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: e.failFirstStatus, Message: message}
	}
	if strings.TrimSpace(forced) == "" {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "missing forced upstream session"}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *unauthorizedFailoverSessionExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: 500, Message: "unused"}
}

func (e *unauthorizedFailoverSessionExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *unauthorizedFailoverSessionExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "unused"}
}

func (e *unauthorizedFailoverSessionExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *unauthorizedFailoverSessionExecutor) ForcedSessions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.forced...)
}

type outerRetryForcedSessionExecutor struct {
	mu     sync.Mutex
	calls  int
	forced []string
}

func (e *outerRetryForcedSessionExecutor) Identifier() string { return "codex" }

func (e *outerRetryForcedSessionExecutor) Execute(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	forced := ""
	if len(opts.Metadata) > 0 {
		forced, _ = opts.Metadata[cliproxyexecutor.ForcedUpstreamSessionMetadataKey].(string)
	}
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.forced = append(e.forced, strings.TrimSpace(forced))
	e.mu.Unlock()
	if call == 1 {
		return cliproxyexecutor.Response{}, &retryAfterStatusError{
			status:     http.StatusTooManyRequests,
			message:    "quota exhausted",
			retryAfter: 5 * time.Millisecond,
		}
	}
	if strings.TrimSpace(forced) == "" {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "missing forced upstream session after outer retry"}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *outerRetryForcedSessionExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: 500, Message: "unused"}
}

func (e *outerRetryForcedSessionExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *outerRetryForcedSessionExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "unused"}
}

func (e *outerRetryForcedSessionExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *outerRetryForcedSessionExecutor) ForcedSessions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.forced...)
}

type authFallbackExecutor struct {
	id string

	mu                sync.Mutex
	executeCalls      []string
	streamCalls       []string
	executeErrors     map[string]error
	streamErrors      map[string]error
	streamFirstErrors map[string]error
	streamNilResults  map[string]bool
	streamNilChunks   map[string]bool
}

func (e *authFallbackExecutor) Identifier() string {
	return e.id
}

func (e *authFallbackExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, auth.ID)
	err := e.executeErrors[auth.ID]
	e.mu.Unlock()
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *authFallbackExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, auth.ID)
	errDirect := e.streamErrors[auth.ID]
	err := e.streamFirstErrors[auth.ID]
	nilResult := e.streamNilResults[auth.ID]
	nilChunks := e.streamNilChunks[auth.ID]
	e.mu.Unlock()

	if errDirect != nil {
		return nil, errDirect
	}
	if nilResult {
		return nil, nil
	}
	if nilChunks {
		return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}}, nil
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	if err != nil {
		ch <- cliproxyexecutor.StreamChunk{Err: err}
		close(ch)
		return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: ch}, nil
	}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: ch}, nil
}

func (e *authFallbackExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *authFallbackExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Message: "not implemented"}
}

func (e *authFallbackExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *authFallbackExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeCalls))
	copy(out, e.executeCalls)
	return out
}

func (e *authFallbackExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.streamCalls))
	copy(out, e.streamCalls)
	return out
}

type retryAfterStatusError struct {
	status     int
	message    string
	retryAfter time.Duration
	headers    http.Header
}

func (e *retryAfterStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *retryAfterStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *retryAfterStatusError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	d := e.retryAfter
	return &d
}

func (e *retryAfterStatusError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

type credentialFailoverStatusError struct {
	status  int
	message string
}

func TestStreamBootstrapErrorStatusCodeUnwrapsCause(t *testing.T) {
	cause := fmt.Errorf("outer: %w", &retryAfterStatusError{
		status:  http.StatusTooManyRequests,
		message: "quota",
	})
	err := newStreamBootstrapError(cause, nil)

	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("bootstrap error %T does not expose StatusCode()", err)
	}
	if got := statusProvider.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestStreamBootstrapErrorHeadersMergesCauseHeaders(t *testing.T) {
	cause := fmt.Errorf("outer: %w", &retryAfterStatusError{
		status:  http.StatusTooManyRequests,
		message: "quota",
		headers: http.Header{
			"Retry-After": {"30"},
			"X-Cause":     {"kept"},
			"X-Shared":    {"cause"},
		},
	})
	err := newStreamBootstrapError(cause, http.Header{
		"X-Stream": {"kept"},
		"X-Shared": {"stream"},
	})

	headerProvider, ok := err.(interface{ Headers() http.Header })
	if !ok {
		t.Fatalf("bootstrap error %T does not expose Headers()", err)
	}
	headers := headerProvider.Headers()
	if got := headers.Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if got := headers.Get("X-Cause"); got != "kept" {
		t.Fatalf("X-Cause = %q, want kept", got)
	}
	if got := headers.Get("X-Stream"); got != "kept" {
		t.Fatalf("X-Stream = %q, want kept", got)
	}
	if got := headers.Get("X-Shared"); got != "stream" {
		t.Fatalf("X-Shared = %q, want stream", got)
	}
}

func TestCredentialRetryLimitErrorHeadersUnwrapCause(t *testing.T) {
	err := &credentialRetryLimitError{
		cause: fmt.Errorf("outer: %w", &retryAfterStatusError{
			status:  http.StatusTooManyRequests,
			message: "quota",
			headers: http.Header{
				"Retry-After": {"30"},
				"X-Upstream":  {"kept"},
			},
		}),
	}

	headerProvider, ok := error(err).(interface{ Headers() http.Header })
	if !ok {
		t.Fatalf("retry limit error %T does not expose Headers()", err)
	}
	headers := headerProvider.Headers()
	if got := headers.Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if got := headers.Get("X-Upstream"); got != "kept" {
		t.Fatalf("X-Upstream = %q, want kept", got)
	}
}

func TestResultErrorFromErrorUnwrapsStatus(t *testing.T) {
	err := fmt.Errorf("outer: %w", &retryAfterStatusError{
		status:  http.StatusTooManyRequests,
		message: "quota",
	})

	resultErr := resultErrorFromError(err)

	if resultErr == nil {
		t.Fatal("resultErrorFromError returned nil")
	}
	if got := resultErr.HTTPStatus; got != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func (e *credentialFailoverStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *credentialFailoverStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *credentialFailoverStatusError) IsAuthScopedFailure() bool { return true }
func (e *credentialFailoverStatusError) IsCredentialFailoverFailure() bool {
	return true
}

func newCredentialRetryLimitTestManager(t *testing.T, maxRetryCredentials int) (*Manager, *credentialRetryLimitExecutor) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, maxRetryCredentials)

	executor := &credentialRetryLimitExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "claude"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "claude"}

	// Auth selection requires that the global model registry knows each credential supports the model.
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "claude", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	return m, executor
}

func TestManager_MaxRetryCredentials_LimitsCrossCredentialRetries(t *testing.T) {
	request := cliproxyexecutor.Request{Model: "test-model"}
	testCases := []struct {
		name   string
		invoke func(*Manager) error
	}{
		{
			name: "execute",
			invoke: func(m *Manager) error {
				_, errExecute := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "execute_count",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteCount(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "execute_stream",
			invoke: func(m *Manager) error {
				_, errExecute := m.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
				return errExecute
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			limitedManager, limitedExecutor := newCredentialRetryLimitTestManager(t, 1)
			if errInvoke := tc.invoke(limitedManager); errInvoke == nil {
				t.Fatalf("expected error for limited retry execution")
			} else if !strings.Contains(errInvoke.Error(), "max-retry-credentials=1") {
				t.Fatalf("limited retry error = %v, want max-retry-credentials diagnostic", errInvoke)
			}
			if calls := limitedExecutor.Calls(); calls != 1 {
				t.Fatalf("expected 1 call with max-retry-credentials=1, got %d", calls)
			}

			unlimitedManager, unlimitedExecutor := newCredentialRetryLimitTestManager(t, 0)
			if errInvoke := tc.invoke(unlimitedManager); errInvoke == nil {
				t.Fatalf("expected error for unlimited retry execution")
			}
			if calls := unlimitedExecutor.Calls(); calls != 2 {
				t.Fatalf("expected 2 calls with max-retry-credentials=0, got %d", calls)
			}
		})
	}
}

func TestManager_CredentialFailoverForcesNewUpstreamSession(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	executor := &unauthorizedFailoverSessionExecutor{failFirstStatus: http.StatusTooManyRequests}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "codex"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	_, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	forced := executor.ForcedSessions()
	if len(forced) != 2 {
		t.Fatalf("forced sessions = %#v, want two calls", forced)
	}
	if forced[0] != "" {
		t.Fatalf("first call forced session = %q, want empty", forced[0])
	}
	if strings.TrimSpace(forced[1]) == "" {
		t.Fatalf("second call forced session should be set after credential failover: %#v", forced)
	}
}

func TestManager_BadRequestCredentialFailoverForcesNewUpstreamSession(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	executor := &unauthorizedFailoverSessionExecutor{
		failFirstStatus:  http.StatusBadRequest,
		failFirstMessage: "invalid_request_error: session is polluted for this auth",
	}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "codex"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	_, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	forced := executor.ForcedSessions()
	if len(forced) != 2 {
		t.Fatalf("forced sessions = %#v, want two calls", forced)
	}
	if forced[0] != "" {
		t.Fatalf("first call forced session = %q, want empty", forced[0])
	}
	if strings.TrimSpace(forced[1]) == "" {
		t.Fatalf("second call after 400 credential failover should force a fresh upstream session: %#v", forced)
	}
}

func TestManager_CachedUnavailableAuthReselectForcesNewUpstreamSession(t *testing.T) {
	affinity := NewSessionAffinitySelector(&RoundRobinSelector{})
	m := NewManager(nil, affinity, nil)
	m.SetRetryConfig(0, 0, 0)
	executor := &unauthorizedFailoverSessionExecutor{}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "codex", Disabled: true, Status: StatusDisabled}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
		affinity.Stop()
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	metadata := map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
	}
	affinity.cache.Set("providers:codex::exec:session-1", auth1.ID)
	_, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Metadata: metadata})
	if errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	forced := executor.ForcedSessions()
	if len(forced) != 1 {
		t.Fatalf("forced sessions = %#v, want one call", forced)
	}
	if strings.TrimSpace(forced[0]) == "" {
		t.Fatalf("first call forced session should be set after cached auth was reselected: %#v", forced)
	}
}

func TestManager_AuthWideFailureInvalidatesAffinityAndForcesFreshSession(t *testing.T) {
	affinity := NewSessionAffinitySelector(&RoundRobinSelector{})
	m := NewManager(nil, affinity, nil)
	m.SetRetryConfig(0, 0, 0)
	executor := &unauthorizedFailoverSessionExecutor{}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "codex"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
		affinity.Stop()
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	metadata := map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
	}
	cacheKey := "providers:codex::exec:session-1"
	affinity.cache.Set(cacheKey, auth1.ID)
	m.MarkResult(context.Background(), Result{
		AuthID:   auth1.ID,
		Provider: "codex",
		Model:    "test-model",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
	})
	if cachedAuthID, ok := affinity.cache.Get(cacheKey); ok {
		t.Fatalf("expected auth-wide failure to invalidate affinity cache, got %q", cachedAuthID)
	}

	_, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Metadata: metadata})
	if errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	forced := executor.ForcedSessions()
	if len(forced) != 1 {
		t.Fatalf("forced sessions = %#v, want one call", forced)
	}
	if strings.TrimSpace(forced[0]) == "" {
		t.Fatalf("first call after auth-wide affinity invalidation should force a fresh upstream session: %#v", forced)
	}
	if cachedAuthID, ok := affinity.cache.Get(cacheKey); !ok || cachedAuthID != auth2.ID {
		t.Fatalf("expected session to rebind to auth2, got auth=%q ok=%v", cachedAuthID, ok)
	}
}

func TestManager_AuthWideFailureForceNewSurvivesFailedRebind(t *testing.T) {
	affinity := NewSessionAffinitySelector(&RoundRobinSelector{})
	m := NewManager(nil, affinity, nil)
	m.SetRetryConfig(0, 0, 0)
	executor := &unauthorizedFailoverSessionExecutor{}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "codex"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "codex", Disabled: true, Status: StatusDisabled}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
		affinity.Stop()
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	metadata := map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
	}
	cacheKey := "providers:codex::exec:session-1"
	affinity.cache.Set(cacheKey, auth1.ID)
	m.MarkResult(context.Background(), Result{
		AuthID:   auth1.ID,
		Provider: "codex",
		Model:    "test-model",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
	})

	_, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Metadata: metadata})
	if errExecute == nil {
		t.Fatalf("expected Execute to fail while no replacement auth is available")
	}
	if !affinity.cache.ForceNewPending(cacheKey) {
		t.Fatalf("force-new marker should survive a failed rebind")
	}

	m.mu.Lock()
	if current := m.auths[auth2.ID]; current != nil {
		current.Disabled = false
		current.Status = StatusActive
		current.Unavailable = false
		current.NextRetryAfter = time.Time{}
		current.UpdatedAt = time.Now()
		auth2 = current.Clone()
	}
	m.mu.Unlock()
	if m.scheduler != nil && auth2 != nil {
		m.scheduler.upsertAuth(auth2.CloneForScheduler())
	}

	_, errExecute = m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Metadata: metadata})
	if errExecute != nil {
		t.Fatalf("Execute after replacement auth recovery: %v", errExecute)
	}
	forced := executor.ForcedSessions()
	if len(forced) != 1 {
		t.Fatalf("forced sessions = %#v, want one call", forced)
	}
	if strings.TrimSpace(forced[0]) == "" {
		t.Fatalf("force-new marker should force a fresh upstream session after recovery: %#v", forced)
	}
	if affinity.cache.ForceNewPending(cacheKey) {
		t.Fatalf("force-new marker should be cleared after successful rebind")
	}
}

func TestManager_OuterRetryPreservesForcedUpstreamSession(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 100*time.Millisecond, 0)
	executor := &outerRetryForcedSessionExecutor{}
	m.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth1 := &Auth{ID: baseID + "-auth-1", Provider: "codex"}
	auth2 := &Auth{ID: baseID + "-auth-2", Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient(auth2.ID, "codex", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	_, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	forced := executor.ForcedSessions()
	if len(forced) != 2 {
		t.Fatalf("forced sessions = %#v, want two calls", forced)
	}
	if forced[0] != "" {
		t.Fatalf("first call forced session = %q, want empty", forced[0])
	}
	if strings.TrimSpace(forced[1]) == "" {
		t.Fatalf("second outer retry call should preserve forced session: %#v", forced)
	}
}

func TestManager_Execute_CodexUsageLimitFailsOverCredential(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"aa-codex-usage-limit": &credentialFailoverStatusError{
				status:  http.StatusTooManyRequests,
				message: "HTTP 429: The usage limit has been reached",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "test-model-codex-usage-limit-failover"
	limitedAuth := &Auth{ID: "aa-codex-usage-limit", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	backupAuth := &Auth{ID: "bb-codex-backup", Provider: "codex", Metadata: map[string]any{"type": "codex"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(limitedAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(backupAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(limitedAuth.ID)
		reg.UnregisterClient(backupAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), limitedAuth); errRegister != nil {
		t.Fatalf("register limited auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), backupAuth); errRegister != nil {
		t.Fatalf("register backup auth: %v", errRegister)
	}

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want failover success", errExecute)
	}
	if string(resp.Payload) != backupAuth.ID {
		t.Fatalf("payload = %q, want %q", string(resp.Payload), backupAuth.ID)
	}

	got := executor.ExecuteCalls()
	want := []string{limitedAuth.ID, backupAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestManager_Execute_CodexUsageLimitDoesNotLeakWhenCredentialsExhausted(t *testing.T) {
	m := NewManager(nil, nil, nil)
	usageLimitErr := &credentialFailoverStatusError{
		status:  http.StatusTooManyRequests,
		message: "HTTP 429: The usage limit has been reached",
	}
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"aa-codex-usage-limit-exhausted": usageLimitErr,
			"bb-codex-usage-limit-exhausted": usageLimitErr,
		},
	}
	m.RegisterExecutor(executor)

	model := "test-model-codex-usage-limit-exhausted"
	auth1 := &Auth{ID: "aa-codex-usage-limit-exhausted", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	auth2 := &Auth{ID: "bb-codex-usage-limit-exhausted", Provider: "codex", Metadata: map[string]any{"type": "codex"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(auth2.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	_, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatalf("expected exhausted credentials error")
	}
	if strings.Contains(errExecute.Error(), "The usage limit has been reached") {
		t.Fatalf("exhausted credential error leaked upstream usage-limit message: %v", errExecute)
	}
	if calls := executor.ExecuteCalls(); len(calls) != 2 {
		t.Fatalf("execute calls = %v, want both credentials attempted", calls)
	}
}

func TestManager_Execute_CodexUsageLimitDoesNotLeakWhenMaxRetryCredentialsStopsFailover(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 1)
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"aa-codex-usage-limit-max-retry": &credentialFailoverStatusError{
				status:  http.StatusTooManyRequests,
				message: "HTTP 429: The usage limit has been reached",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "test-model-codex-usage-limit-max-retry"
	auth1 := &Auth{ID: "aa-codex-usage-limit-max-retry", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	auth2 := &Auth{ID: "bb-codex-usage-limit-max-retry", Provider: "codex", Metadata: map[string]any{"type": "codex"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth1.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(auth2.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, errRegister := m.Register(context.Background(), auth1); errRegister != nil {
		t.Fatalf("register auth1: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), auth2); errRegister != nil {
		t.Fatalf("register auth2: %v", errRegister)
	}

	_, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatalf("expected max-retry credentials error")
	}
	if strings.Contains(errExecute.Error(), "The usage limit has been reached") {
		t.Fatalf("max-retry credential error leaked upstream usage-limit message: %v", errExecute)
	}
	if !strings.Contains(errExecute.Error(), "max-retry-credentials=1") {
		t.Fatalf("max-retry credential error = %v, want max-retry diagnostic", errExecute)
	}
	if calls := executor.ExecuteCalls(); len(calls) != 1 {
		t.Fatalf("execute calls = %v, want one credential attempted", calls)
	}
}

func TestManager_ExecuteStream_CodexUsageLimitBootstrapFailsOverCredential(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "codex",
		streamFirstErrors: map[string]error{
			"aa-codex-usage-limit-stream": &credentialFailoverStatusError{
				status:  http.StatusTooManyRequests,
				message: "HTTP 429: The usage limit has been reached",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "test-model-codex-usage-limit-stream-failover"
	limitedAuth := &Auth{ID: "aa-codex-usage-limit-stream", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	backupAuth := &Auth{ID: "bb-codex-backup-stream", Provider: "codex", Metadata: map[string]any{"type": "codex"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(limitedAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(backupAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(limitedAuth.ID)
		reg.UnregisterClient(backupAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), limitedAuth); errRegister != nil {
		t.Fatalf("register limited auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), backupAuth); errRegister != nil {
		t.Fatalf("register backup auth: %v", errRegister)
	}

	streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute stream error = %v, want failover success", errExecute)
	}
	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v, want success", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != backupAuth.ID {
		t.Fatalf("payload = %q, want %q", string(payload), backupAuth.ID)
	}

	got := executor.StreamCalls()
	want := []string{limitedAuth.ID, backupAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream call %d auth = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestManagerExecuteStream_InvalidStreamResultReturnsErrorChunk(t *testing.T) {
	testCases := []struct {
		name      string
		configure func(*authFallbackExecutor, string)
	}{
		{
			name: "nil_result",
			configure: func(executor *authFallbackExecutor, authID string) {
				executor.streamNilResults = map[string]bool{authID: true}
			},
		},
		{
			name: "nil_chunks",
			configure: func(executor *authFallbackExecutor, authID string) {
				executor.streamNilChunks = map[string]bool{authID: true}
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetRetryConfig(0, 0, 0)
			executor := &authFallbackExecutor{id: "codex"}
			m.RegisterExecutor(executor)

			model := "gpt-5-invalid-stream-result-" + tc.name
			auth := &Auth{ID: "aa-invalid-stream-" + tc.name, Provider: "codex", Metadata: map[string]any{"type": "codex"}}
			tc.configure(executor, auth.ID)

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

			if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if errExecute != nil {
				t.Fatalf("execute stream error = %v, want error chunk", errExecute)
			}
			if streamResult == nil || streamResult.Chunks == nil {
				t.Fatalf("stream result = %#v, want non-nil result and chunks", streamResult)
			}

			var chunk cliproxyexecutor.StreamChunk
			select {
			case got, ok := <-streamResult.Chunks:
				if !ok {
					t.Fatal("stream closed before error chunk")
				}
				chunk = got
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for invalid stream error chunk")
			}

			authErr, ok := chunk.Err.(*Error)
			if !ok || authErr == nil {
				t.Fatalf("chunk error = %#v, want *Error", chunk.Err)
			}
			if authErr.Code != "invalid_stream_result" || authErr.HTTPStatus != http.StatusBadGateway || !authErr.Retryable {
				t.Fatalf("chunk error = %#v, want retryable invalid_stream_result 502", authErr)
			}

			select {
			case _, ok := <-streamResult.Chunks:
				if ok {
					t.Fatal("expected stream to close after invalid stream error chunk")
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for invalid stream to close")
			}

			updated, ok := m.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("expected auth to remain registered")
			}
			state := updated.ModelStates[model]
			if state == nil || state.LastError == nil {
				t.Fatalf("model state = %#v, want invalid stream error recorded", state)
			}
			if state.LastError.Code != "invalid_stream_result" || state.LastError.HTTPStatus != http.StatusBadGateway {
				t.Fatalf("model last error = %#v, want invalid_stream_result 502", state.LastError)
			}
		})
	}
}

func TestManagerExecuteStream_InvalidStreamResultFailsOverCredential(t *testing.T) {
	testCases := []struct {
		name      string
		configure func(*authFallbackExecutor, string)
	}{
		{
			name: "nil_result",
			configure: func(executor *authFallbackExecutor, authID string) {
				executor.streamNilResults = map[string]bool{authID: true}
			},
		},
		{
			name: "nil_chunks",
			configure: func(executor *authFallbackExecutor, authID string) {
				executor.streamNilChunks = map[string]bool{authID: true}
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetRetryConfig(0, 0, 0)
			executor := &authFallbackExecutor{id: "codex"}
			m.RegisterExecutor(executor)

			model := "gpt-5-invalid-stream-failover-" + tc.name
			badAuth := &Auth{ID: "aa-invalid-stream-failover-" + tc.name, Provider: "codex", Metadata: map[string]any{"type": "codex"}}
			goodAuth := &Auth{ID: "bb-valid-stream-failover-" + tc.name, Provider: "codex", Metadata: map[string]any{"type": "codex"}}
			tc.configure(executor, badAuth.ID)

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(badAuth.ID, badAuth.Provider, []*registry.ModelInfo{{ID: model}})
			reg.RegisterClient(goodAuth.ID, goodAuth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() {
				reg.UnregisterClient(badAuth.ID)
				reg.UnregisterClient(goodAuth.ID)
			})

			if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
				t.Fatalf("register bad auth: %v", errRegister)
			}
			if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
				t.Fatalf("register good auth: %v", errRegister)
			}

			streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if errExecute != nil {
				t.Fatalf("execute stream error = %v, want failover success", errExecute)
			}
			var payload []byte
			for chunk := range streamResult.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error = %v, want success", chunk.Err)
				}
				payload = append(payload, chunk.Payload...)
			}
			if string(payload) != goodAuth.ID {
				t.Fatalf("payload = %q, want %q", string(payload), goodAuth.ID)
			}

			got := executor.StreamCalls()
			want := []string{badAuth.ID, goodAuth.ID}
			if len(got) != len(want) {
				t.Fatalf("stream calls = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("stream call %d auth = %q, want %q", i, got[i], want[i])
				}
			}
		})
	}
}

func TestManager_ModelSupportBadRequest_FallsBackAndSuspendsAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    "invalid_request_error: The requested model is not supported.",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "claude-opus-4-6"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "claude"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "claude"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: model}
	for i := 0; i < 2; i++ {
		resp, errExecute := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute %d error = %v, want success", i, errExecute)
		}
		if string(resp.Payload) != goodAuth.ID {
			t.Fatalf("execute %d payload = %q, want %q", i, string(resp.Payload), goodAuth.ID)
		}
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	state := updatedBad.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state for %q", model)
	}
	if !state.Unavailable {
		t.Fatalf("expected bad auth model state to be unavailable")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected bad auth model state cooldown to be set")
	}
}

func TestManager_AuthWideBadRequestFallsBackAndSuspendsAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    "auth file rejected by upstream account policy",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "claude-opus-4-6"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "claude"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "claude"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: model}
	for i := 0; i < 2; i++ {
		resp, errExecute := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute %d error = %v, want success", i, errExecute)
		}
		if string(resp.Payload) != goodAuth.ID {
			t.Fatalf("execute %d payload = %q, want %q", i, string(resp.Payload), goodAuth.ID)
		}
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	if !updatedBad.Unavailable {
		t.Fatal("expected 400 auth-wide failure to suspend the whole auth file")
	}
	if updatedBad.LastError == nil || updatedBad.LastError.StatusCode() != http.StatusBadRequest {
		t.Fatalf("bad auth LastError = %#v, want 400", updatedBad.LastError)
	}
	if AuthAvailableForModel(updatedBad, "unrelated-model", time.Now()) {
		t.Fatal("expected auth-wide 400 to make unrelated models unavailable on the bad auth")
	}
}

func TestManagerExecuteStream_ModelSupportBadRequestFallsBackAndSuspendsAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		streamFirstErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    "invalid_request_error: The requested model is not supported.",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "claude-opus-4-6"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "claude"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "claude"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: model}
	for i := 0; i < 2; i++ {
		streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute stream %d error = %v, want success", i, errExecute)
		}
		var payload []byte
		for chunk := range streamResult.Chunks {
			if chunk.Err != nil {
				t.Fatalf("execute stream %d chunk error = %v, want success", i, chunk.Err)
			}
			payload = append(payload, chunk.Payload...)
		}
		if string(payload) != goodAuth.ID {
			t.Fatalf("execute stream %d payload = %q, want %q", i, string(payload), goodAuth.ID)
		}
	}

	got := executor.StreamCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	state := updatedBad.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state for %q", model)
	}
	if !state.Unavailable {
		t.Fatalf("expected bad auth model state to be unavailable")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("expected bad auth model state cooldown to be set")
	}
}

func TestManagerExecute_UnauthorizedSuspendsAuthFileAcrossModels(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"aa-invalid-token": &Error{
				HTTPStatus: http.StatusUnauthorized,
				Message:    "Your authentication token has been invalidated. Please try signing in again.",
			},
		},
	}
	m.RegisterExecutor(executor)

	modelA := "gpt-5-invalid-auth-model-a"
	modelB := "gpt-5-invalid-auth-model-b"
	badAuth := &Auth{ID: "aa-invalid-token", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	goodAuth := &Auth{ID: "bb-valid-token", Provider: "codex", Metadata: map[string]any{"type": "codex"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "codex", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	reg.RegisterClient(goodAuth.ID, "codex", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	for _, model := range []string{modelA, modelB} {
		resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute %s error = %v, want failover success", model, errExecute)
		}
		if string(resp.Payload) != goodAuth.ID {
			t.Fatalf("execute %s payload = %q, want %q", model, string(resp.Payload), goodAuth.ID)
		}
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	if !updatedBad.Unavailable {
		t.Fatal("expected 401 to suspend the whole auth file")
	}
	if updatedBad.LastError == nil || updatedBad.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("bad auth LastError = %#v, want 401", updatedBad.LastError)
	}
	if AuthAvailableForModel(updatedBad, modelB, time.Now()) {
		t.Fatalf("expected unauthorized auth to be unavailable for unrelated model %q", modelB)
	}
}

func TestManagerExecute_ForbiddenFailsOverWithinSingleProvider(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"aa-forbidden": &Error{
				HTTPStatus: http.StatusForbidden,
				Message:    "forbidden",
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "gpt-5-forbidden-failover"
	badAuth := &Auth{ID: "aa-forbidden", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	goodAuth := &Auth{ID: "bb-valid-forbidden", Provider: "codex", Metadata: map[string]any{"type": "codex"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want failover success", errExecute)
	}
	if string(resp.Payload) != goodAuth.ID {
		t.Fatalf("payload = %q, want %q", string(resp.Payload), goodAuth.ID)
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestManagerExecute_UnauthorizedIgnoresDisableCooling(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"aa-invalid-token-disable-cooling": &Error{
				HTTPStatus: http.StatusUnauthorized,
				Message:    "token invalid",
			},
		},
	}
	m.RegisterExecutor(executor)

	modelA := "gpt-5-invalid-auth-disable-cooling-a"
	modelB := "gpt-5-invalid-auth-disable-cooling-b"
	badAuth := &Auth{
		ID:       "aa-invalid-token-disable-cooling",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex", "disable_cooling": true},
	}
	goodAuth := &Auth{ID: "bb-valid-token-disable-cooling", Provider: "codex", Metadata: map[string]any{"type": "codex"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "codex", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	reg.RegisterClient(goodAuth.ID, "codex", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	for _, model := range []string{modelA, modelB} {
		resp, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute %s error = %v, want failover success", model, errExecute)
		}
		if string(resp.Payload) != goodAuth.ID {
			t.Fatalf("execute %s payload = %q, want %q", model, string(resp.Payload), goodAuth.ID)
		}
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	if !updatedBad.Unavailable || !updatedBad.NextRetryAfter.After(time.Now()) {
		t.Fatalf("expected 401 auth to stay cooled despite disable_cooling, unavailable=%v next=%v", updatedBad.Unavailable, updatedBad.NextRetryAfter)
	}
	if AuthAvailableForModel(updatedBad, modelB, time.Now()) {
		t.Fatalf("expected unauthorized auth to be unavailable for unrelated model %q", modelB)
	}
}

func TestManagerExecuteStream_UnauthorizedBootstrapSuspendsAuthFileAcrossModels(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "codex",
		streamFirstErrors: map[string]error{
			"aa-invalid-token-stream": &Error{
				HTTPStatus: http.StatusUnauthorized,
				Message:    "Your authentication token has been invalidated. Please try signing in again.",
			},
		},
	}
	m.RegisterExecutor(executor)

	modelA := "gpt-5-invalid-auth-stream-model-a"
	modelB := "gpt-5-invalid-auth-stream-model-b"
	badAuth := &Auth{ID: "aa-invalid-token-stream", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	goodAuth := &Auth{ID: "bb-valid-token-stream", Provider: "codex", Metadata: map[string]any{"type": "codex"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "codex", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	reg.RegisterClient(goodAuth.ID, "codex", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	for _, model := range []string{modelA, modelB} {
		streamResult, errExecute := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("execute stream %s error = %v, want failover success", model, errExecute)
		}
		var payload []byte
		for chunk := range streamResult.Chunks {
			if chunk.Err != nil {
				t.Fatalf("execute stream %s chunk error = %v, want success", model, chunk.Err)
			}
			payload = append(payload, chunk.Payload...)
		}
		if string(payload) != goodAuth.ID {
			t.Fatalf("execute stream %s payload = %q, want %q", model, string(payload), goodAuth.ID)
		}
	}

	got := executor.StreamCalls()
	want := []string{badAuth.ID, goodAuth.ID, goodAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	if !updatedBad.Unavailable {
		t.Fatal("expected 401 to suspend the whole auth file")
	}
	if updatedBad.LastError == nil || updatedBad.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("bad auth LastError = %#v, want 401", updatedBad.LastError)
	}
	if AuthAvailableForModel(updatedBad, modelB, time.Now()) {
		t.Fatalf("expected unauthorized auth to be unavailable for unrelated model %q", modelB)
	}
}

func TestIsRequestInvalidError_BadRequestDoesNotBlockCredentialFailover(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    "oauth API error: ValidationException: Improperly formed request: missing currentMessage.content",
	}
	if isRequestInvalidError(err) {
		t.Fatal("400 errors should not block credential failover and fresh upstream session creation")
	}
}

func TestManagerMarkResult_SessionContextBadRequestDoesNotSuspendAuth(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		message string
	}{
		{
			name:    "previous response missing",
			message: "HTTP 400: Previous response with id 'resp_038d5107ec6cc78c016a1fb143ac088191b14e6ca3097c696e' not found.",
		},
		{
			name:    "tool call missing",
			message: `{"error":{"message":"No tool call found for function call output with call_id call_Rx1FW4RrRF9C1SyH2xxBVtEn.","param":"input","type":"invalid_request_error"}}`,
		},
		{
			name:    "custom tool call missing",
			message: `{"type":"error","status":400,"error":{"message":"No tool call found for custom tool call output with call_id call_jzaeS5GDDushxTKTsXR9CTWL.","param":"input","type":"invalid_request_error"}}`,
		},
		{
			name:    "context too large code",
			code:    "context_too_large",
			message: "Your input exceeds the context window of this model. Please adjust your input and try again.",
		},
		{
			name:    "context length exceeded payload",
			message: `{"type":"error","status":400,"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input","type":"invalid_request_error"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			auth := &Auth{ID: "auth-ws-context", Provider: "codex", Status: StatusActive}
			if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			m.MarkResult(context.Background(), Result{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				Model:    "gpt-5-codex",
				Success:  false,
				Error: &Error{
					HTTPStatus: http.StatusBadRequest,
					Code:       tt.code,
					Message:    tt.message,
				},
			})

			updated, ok := m.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatalf("auth not found")
			}
			if updated.Unavailable {
				t.Fatal("session-context 400 should not suspend the auth file")
			}
			if updated.LastError != nil {
				t.Fatalf("session-context 400 should not persist LastError, got %#v", updated.LastError)
			}
			if !AuthAvailableForModel(updated, "gpt-5-codex", time.Now()) {
				t.Fatal("session-context 400 should not make the model unavailable")
			}
		})
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model"
	m.MarkResult(context.Background(), Result{
		AuthID:   "auth-1",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 500, Message: "boom"},
	})

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride_On403(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-403",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-403"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}

	if count := reg.GetModelCount(model); count <= 0 {
		t.Fatalf("expected model count > 0 when disable_cooling=true, got %d", count)
	}
}

func TestManager_MarkResult_PreservesStructuredAuthError(t *testing.T) {
	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-structured-error",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-structured-error"
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			Code:       "rate_limited",
			Message:    "quota exceeded",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if updated.LastError == nil {
		t.Fatal("expected auth.LastError to be recorded")
	}
	if updated.LastError.Code != "rate_limited" {
		t.Fatalf("auth.LastError.Code = %q, want %q", updated.LastError.Code, "rate_limited")
	}
	if !updated.LastError.Retryable {
		t.Fatal("expected auth.LastError.Retryable to be true")
	}
	state := updated.ModelStates[model]
	if state == nil || state.LastError == nil {
		t.Fatalf("expected model state last error to be recorded, got %#v", state)
	}
	if state.LastError.Code != "rate_limited" {
		t.Fatalf("model LastError.Code = %q, want %q", state.LastError.Code, "rate_limited")
	}
	if state.LastError.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("model LastError.HTTPStatus = %d, want %d", state.LastError.HTTPStatus, http.StatusTooManyRequests)
	}
}

func TestManager_MarkResult_SkipsCleanModelSuccess(t *testing.T) {
	m := NewManager(nil, nil, nil)

	updatedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:        "auth-clean-success",
		Provider:  "claude",
		Status:    StatusActive,
		UpdatedAt: updatedAt,
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registered, ok := m.GetByID(auth.ID)
	if !ok || registered == nil {
		t.Fatalf("expected auth to be present after register")
	}
	updatedAt = registered.UpdatedAt

	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "steady-model",
		Success:  true,
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if !updated.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("UpdatedAt = %v, want unchanged %v", updated.UpdatedAt, updatedAt)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("ModelStates = %#v, want nil/empty for clean success", updated.ModelStates)
	}
}

func TestManager_Execute_DisableCooling_DoesNotBlackoutAfter403(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-403-exec": &Error{
				HTTPStatus: http.StatusForbidden,
				Message:    "forbidden",
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-403-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-403-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute1 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute1 == nil {
		t.Fatal("expected first execute error")
	}
	if statusCodeFromError(errExecute1) != http.StatusForbidden {
		t.Fatalf("first execute status = %d, want %d", statusCodeFromError(errExecute1), http.StatusForbidden)
	}

	_, errExecute2 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute2 == nil {
		t.Fatal("expected second execute error")
	}
	if statusCodeFromError(errExecute2) != http.StatusForbidden {
		t.Fatalf("second execute status = %d, want %d", statusCodeFromError(errExecute2), http.StatusForbidden)
	}
}

func TestManager_Execute_DisableCooling_DoesNotBlackoutAfter429RetryAfter(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-429-exec": &retryAfterStatusError{
				status:     http.StatusTooManyRequests,
				message:    "quota exhausted",
				retryAfter: 2 * time.Minute,
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-429-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-429-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute1 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute1 == nil {
		t.Fatal("expected first execute error")
	}
	if statusCodeFromError(errExecute1) != http.StatusTooManyRequests {
		t.Fatalf("first execute status = %d, want %d", statusCodeFromError(errExecute1), http.StatusTooManyRequests)
	}

	_, errExecute2 := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute2 == nil {
		t.Fatal("expected second execute error")
	}
	if statusCodeFromError(errExecute2) != http.StatusTooManyRequests {
		t.Fatalf("second execute status = %d, want %d", statusCodeFromError(errExecute2), http.StatusTooManyRequests)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 2 {
		t.Fatalf("execute calls = %d, want 2", len(calls))
	}

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestManager_Execute_DisableCooling_RetriesAfter429RetryAfter(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 100*time.Millisecond, 0)

	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"auth-429-retryafter-exec": &retryAfterStatusError{
				status:     http.StatusTooManyRequests,
				message:    "quota exhausted",
				retryAfter: 5 * time.Millisecond,
			},
		},
	}
	m.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-429-retryafter-exec",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model-429-retryafter-exec"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	req := cliproxyexecutor.Request{Model: model}
	_, errExecute := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected execute error")
	}
	if statusCodeFromError(errExecute) != http.StatusTooManyRequests {
		t.Fatalf("execute status = %d, want %d", statusCodeFromError(errExecute), http.StatusTooManyRequests)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 4 {
		t.Fatalf("execute calls = %d, want 4 (initial + 3 retries)", len(calls))
	}
}

func TestManager_MarkResult_RequestScopedNotFoundDoesNotCooldownAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "openai",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "gpt-4.1"
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusNotFound,
			Message:    requestScopedNotFoundMessage,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if updated.Unavailable {
		t.Fatalf("expected request-scoped 404 to keep auth available")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("expected request-scoped 404 to keep auth cooldown unset, got %v", updated.NextRetryAfter)
	}
	if state := updated.ModelStates[model]; state != nil {
		t.Fatalf("expected request-scoped 404 to avoid model cooldown state, got %#v", state)
	}
}

func TestManager_RequestScopedNotFoundStopsRetryWithoutSuspendingAuth(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: "openai",
		executeErrors: map[string]error{
			"aa-bad-auth": &Error{
				HTTPStatus: http.StatusNotFound,
				Message:    requestScopedNotFoundMessage,
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "gpt-4.1"
	badAuth := &Auth{ID: "aa-bad-auth", Provider: "openai"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "openai"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "openai", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "openai", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	_, errExecute := m.Execute(context.Background(), []string{"openai"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected request-scoped not-found error")
	}
	errResult, ok := errExecute.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", errExecute)
	}
	if errResult.HTTPStatus != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", errResult.HTTPStatus, http.StatusNotFound)
	}
	if errResult.Message != requestScopedNotFoundMessage {
		t.Fatalf("message = %q, want %q", errResult.Message, requestScopedNotFoundMessage)
	}

	got := executor.ExecuteCalls()
	want := []string{badAuth.ID}
	if len(got) != len(want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execute call %d auth = %q, want %q", i, got[i], want[i])
		}
	}

	updatedBad, ok := m.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth to remain registered")
	}
	if updatedBad.Unavailable {
		t.Fatalf("expected request-scoped 404 to keep bad auth available")
	}
	if !updatedBad.NextRetryAfter.IsZero() {
		t.Fatalf("expected request-scoped 404 to keep bad auth cooldown unset, got %v", updatedBad.NextRetryAfter)
	}
	if state := updatedBad.ModelStates[model]; state != nil {
		t.Fatalf("expected request-scoped 404 to avoid bad auth model cooldown state, got %#v", state)
	}
}
