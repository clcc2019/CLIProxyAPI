package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
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
	callback, ok := meta[coreexecutor.SelectedAuthCallbackMetadataKey].(func(string))
	if !ok || callback == nil {
		t.Fatalf("selected auth callback missing")
	}
	callback("auth-1")
	if !callbackCalled {
		t.Fatalf("selected auth callback was not preserved")
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
}

func (e *headerCaptureExecutor) Identifier() string { return "codex" }

func (e *headerCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	if auth != nil {
		e.selectedAuthIDs = append(e.selectedAuthIDs, auth.ID)
	} else {
		e.selectedAuthIDs = append(e.selectedAuthIDs, "")
	}
	e.sessionHeaders = append(e.sessionHeaders, opts.Headers.Get("Session_id"))
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *headerCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *headerCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *headerCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":0}`)}, nil
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
