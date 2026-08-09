package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestRequestExecutionMetadataUsesIdempotencyHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.Header.Set("Idempotency-Key", "client-key")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	meta := requestExecutionMetadata(ctx)

	if got := meta[idempotencyKeyMetadataKey]; got != "client-key" {
		t.Fatalf("idempotency key = %v, want client-key", got)
	}
}

func TestRequestExecutionMetadataIncludesExecutionHints(t *testing.T) {
	base := context.Background()
	base = WithPinnedAuthID(base, "auth-1")
	base = WithExecutionSessionID(base, "session-1")
	base = WithMaxRetryCredentials(base, 1)

	callbackCalled := false
	base = WithSelectedAuthIDCallback(base, func(authID string) {
		callbackCalled = authID != ""
	})

	meta := requestExecutionMetadata(base)
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("unexpected idempotency key in metadata: %v", meta[idempotencyKeyMetadataKey])
	}
	if got := meta[coreexecutor.PinnedAuthMetadataKey]; got != "auth-1" {
		t.Fatalf("pinned auth = %v, want auth-1", got)
	}
	if got := meta[coreexecutor.ExecutionSessionMetadataKey]; got != "session-1" {
		t.Fatalf("execution session = %v, want session-1", got)
	}
	if got := meta[coreexecutor.MaxRetryCredentialsMetadataKey]; got != 1 {
		t.Fatalf("max retry credentials = %v, want 1", got)
	}
	callback, ok := meta[coreexecutor.SelectedAuthCallbackMetadataKey].(func(string))
	if !ok || callback == nil {
		t.Fatalf("selected auth callback missing")
	}
	callback("auth-1")
	if !callbackCalled {
		t.Fatalf("selected auth callback was not preserved")
	}
}

func TestRequestExecutionMetadataIncludesHashedClientPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Set("apiKey", "client-secret-key")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	meta := requestExecutionMetadata(ctx)

	got, _ := meta[coreexecutor.ClientPrincipalMetadataKey].(string)
	if got == "" {
		t.Fatalf("client principal metadata missing: %#v", meta)
	}
	if got == "client-secret-key" {
		t.Fatalf("client principal metadata should be hashed, got raw key")
	}
	if want := hashClientPrincipal("client-secret-key"); got != want {
		t.Fatalf("client principal metadata = %q, want %q", got, want)
	}
}

func TestRequestExecutionMetadataEmptyReturnsNil(t *testing.T) {
	if meta := requestExecutionMetadata(context.Background()); meta != nil {
		t.Fatalf("requestExecutionMetadata() = %#v, want nil", meta)
	}
}

type headerCaptureExecutor struct {
	selectedAuthIDs []string
	sessionHeaders  []string
	executeRequests []coreexecutor.Request
	executeOptions  []coreexecutor.Options
	countRequests   []coreexecutor.Request
	countOptions    []coreexecutor.Options
}

func (e *headerCaptureExecutor) Identifier() string { return "codex" }

func (e *headerCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	if auth != nil {
		e.selectedAuthIDs = append(e.selectedAuthIDs, auth.ID)
	} else {
		e.selectedAuthIDs = append(e.selectedAuthIDs, "")
	}
	e.sessionHeaders = append(e.sessionHeaders, opts.Headers.Get("Session_id"))
	e.executeRequests = append(e.executeRequests, req)
	e.executeOptions = append(e.executeOptions, opts)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *headerCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *headerCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *headerCaptureExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.countRequests = append(e.countRequests, req)
	e.countOptions = append(e.countOptions, opts)
	return coreexecutor.Response{
		Payload: []byte(`{"total_tokens":0}`),
		Headers: http.Header{
			"X-Upstream-Request-Id": {"count-req-1"},
			"Set-Cookie":            {"secret=value"},
		},
	}, nil
}

func (e *headerCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(http.NoBody)}, nil
}

func TestExecuteWithAuthManagerPassesHeadersToSessionAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("Session_id", "codex-session-1")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	model := "test-codex-header-affinity-model"

	executor := &headerCaptureExecutor{}
	selector := coreauth.NewSessionAffinitySelector(&coreauth.RoundRobinSelector{})
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	auths := []*coreauth.Auth{
		{ID: "test-codex-header-affinity-a", Provider: "codex"},
		{ID: "test-codex-header-affinity-b", Provider: "codex"},
	}
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	rawJSON := []byte(`{"model":"test-codex-header-affinity-model"}`)

	for i := 0; i < 2; i++ {
		if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai-response", model, rawJSON, ""); errMsg != nil {
			t.Fatalf("ExecuteWithAuthManager(%d) error: %v", i, errMsg.Error)
		}
	}

	if len(executor.selectedAuthIDs) != 2 {
		t.Fatalf("selected auths = %v, want 2 calls", executor.selectedAuthIDs)
	}
	if executor.selectedAuthIDs[0] != executor.selectedAuthIDs[1] {
		t.Fatalf("same Session_id should stay on one auth, got %v", executor.selectedAuthIDs)
	}
	for i, got := range executor.sessionHeaders {
		if got != "codex-session-1" {
			t.Fatalf("call %d Session_id header = %q, want codex-session-1", i, got)
		}
	}
}

func TestExecuteCountWithAuthManagerBuildsSharedExecutionRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	ginCtx.Request.Header.Set("Idempotency-Key", "count-key")
	ginCtx.Request.Header.Set("Session_id", "count-session")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	const (
		model  = "test-count-request-model"
		authID = "test-count-request-auth"
	)
	executor := &headerCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	rawJSON := []byte(`{"model":"test-count-request-model","reasoning":{"effort":"medium"},"service_tier":"priority"}`)
	payload, headers, errMsg := handler.ExecuteCountWithAuthManager(ctx, "openai-response", model, rawJSON, "count-alt")
	if errMsg != nil {
		t.Fatalf("ExecuteCountWithAuthManager() error: %v", errMsg.Error)
	}
	if got := string(payload); got != `{"total_tokens":0}` {
		t.Fatalf("payload = %s", got)
	}
	if got := headers.Get("X-Upstream-Request-Id"); got != "count-req-1" {
		t.Fatalf("X-Upstream-Request-Id = %q", got)
	}
	if got := headers.Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie leaked through filtered headers: %q", got)
	}
	if len(executor.executeRequests) != 0 {
		t.Fatalf("Execute calls = %d, want 0", len(executor.executeRequests))
	}
	if len(executor.countRequests) != 1 || len(executor.countOptions) != 1 {
		t.Fatalf("CountTokens captures = (%d requests, %d options), want 1 each", len(executor.countRequests), len(executor.countOptions))
	}
	req := executor.countRequests[0]
	opts := executor.countOptions[0]
	if req.Model != model || string(req.Payload) != string(rawJSON) {
		t.Fatalf("request = %#v, want model %q and original payload", req, model)
	}
	if opts.Stream {
		t.Fatal("CountTokens options unexpectedly enabled streaming")
	}
	if opts.Alt != "count-alt" || opts.SourceFormat.String() != "openai-response" {
		t.Fatalf("options alt/source = (%q, %q)", opts.Alt, opts.SourceFormat.String())
	}
	if opts.Headers.Get("Session_id") != "count-session" {
		t.Fatalf("Session_id = %q", opts.Headers.Get("Session_id"))
	}
	if string(opts.OriginalRequest) != string(rawJSON) {
		t.Fatalf("OriginalRequest = %s", opts.OriginalRequest)
	}
	meta := opts.Metadata
	if got := meta[coreexecutor.RequestedModelMetadataKey]; got != model {
		t.Fatalf("requested model metadata = %v", got)
	}
	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("reasoning effort metadata = %v", got)
	}
	if got := meta[coreexecutor.ServiceTierMetadataKey]; got != "priority" {
		t.Fatalf("service tier metadata = %v", got)
	}
	if got := meta[coreexecutor.NeedResponseHeadersMetadataKey]; got != true {
		t.Fatalf("response headers metadata = %v", got)
	}
	if got := meta[idempotencyKeyMetadataKey]; got != "count-key" {
		t.Fatalf("idempotency metadata = %v", got)
	}
	if got := meta[coreexecutor.RequestPathMetadataKey]; got != "/v1/responses/input_tokens" {
		t.Fatalf("request path metadata = %v", got)
	}
	if got := meta[coreexecutor.RequestContentTypeMetadataKey]; got != "application/json" {
		t.Fatalf("content type metadata = %v", got)
	}
	requestMeta := opts.RequestMetadata
	if !requestMeta.Parsed || requestMeta.RequestedModel != model || requestMeta.NormalizedModel != model {
		t.Fatalf("structured request metadata models = %#v", requestMeta)
	}
	if requestMeta.ReasoningEffort != "medium" || requestMeta.ServiceTier != "priority" {
		t.Fatalf("structured request metadata usage = %#v", requestMeta)
	}
	if requestMeta.RequestPath != "/v1/responses/input_tokens" || requestMeta.ContentType != "application/json" || requestMeta.IdempotencyKey != "count-key" {
		t.Fatalf("structured request metadata HTTP fields = %#v", requestMeta)
	}
}

func TestExecuteImageWithAuthManagerAllowsImageOnlyModel(t *testing.T) {
	const (
		model  = "gpt-image-2"
		authID = "test-image-request-auth"
	)
	executor := &headerCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	rawJSON := []byte(`{"model":"gpt-image-2","prompt":"draw a fox"}`)
	if _, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, rawJSON, ""); errMsg == nil || errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ordinary ExecuteWithAuthManager error = %#v, want image-only rejection", errMsg)
	}
	payload, _, errMsg := handler.ExecuteImageWithAuthManager(context.Background(), "openai", model, rawJSON, "image")
	if errMsg != nil {
		t.Fatalf("ExecuteImageWithAuthManager() error: %v", errMsg.Error)
	}
	if got := string(payload); got != `{"ok":true}` {
		t.Fatalf("payload = %s", got)
	}
	if len(executor.executeRequests) != 1 || len(executor.executeOptions) != 1 {
		t.Fatalf("Execute captures = (%d requests, %d options), want 1 each", len(executor.executeRequests), len(executor.executeOptions))
	}
	if executor.executeRequests[0].Model != model {
		t.Fatalf("image request model = %q", executor.executeRequests[0].Model)
	}
	if executor.executeOptions[0].Stream {
		t.Fatal("image non-stream request unexpectedly enabled streaming")
	}
}

func TestRequestHeadersFromContextReturnsClone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("Session_id", "codex-session-1")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	headers := requestHeadersFromContext(ctx)
	headers.Set("Session_id", "mutated")
	headers.Set("X-Injected", "yes")

	if got := ginCtx.Request.Header.Get("Session_id"); got != "codex-session-1" {
		t.Fatalf("original Session_id = %q, want codex-session-1", got)
	}
	if got := ginCtx.Request.Header.Get("X-Injected"); got != "" {
		t.Fatalf("original X-Injected = %q, want empty", got)
	}
}

func BenchmarkRequestHeadersFromContext(b *testing.B) {
	gin.SetMode(gin.ReleaseMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("Session_id", "codex-session-1")
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	ginCtx.Request.Header.Set("Idempotency-Key", "client-key")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		headers := requestHeadersFromContext(ctx)
		if headers.Get("Session_id") == "" {
			b.Fatal("missing Session_id")
		}
	}
}

func TestSetReasoningEffortMetadataUsesSuffixOverBody(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai", "gpt-5.4(high)", []byte(`{"reasoning_effort":"low"}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "high")
	}
}

func TestSetReasoningEffortMetadataSupportsOpenAIResponses(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai-response", "gpt-5.4", []byte(`{"reasoning":{"effort":"medium"}}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "medium")
	}
}

func TestSetServiceTierMetadataExtractsValue(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"priority"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "priority" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "priority")
	}
}

func TestSetServiceTierMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "default" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "default")
	}
}
