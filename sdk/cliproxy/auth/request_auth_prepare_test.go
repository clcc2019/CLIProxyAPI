package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type requestPrepareStore struct {
	saveCount atomic.Int32
	mu        sync.Mutex
	last      *Auth
}

func (s *requestPrepareStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *requestPrepareStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.saveCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = auth.Clone()
	return "", nil
}

func (s *requestPrepareStore) Delete(context.Context, string) error { return nil }

func (s *requestPrepareStore) lastAuth() *Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last.Clone()
}

type requestPrepareExecutor struct {
	prepareCalls atomic.Int32
	executeCalls atomic.Int32
}

type requestPrepareContextExecutor struct {
	provider     string
	prepareCalls atomic.Int32
	streamCalls  atomic.Int32
	countCalls   atomic.Int32
}

func (e *requestPrepareExecutor) Identifier() string { return "antigravity" }

func (e *requestPrepareExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth == nil || auth.Metadata == nil || testStringValue(auth.Metadata["project_id"]) == ""
}

func (e *requestPrepareExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.prepareCalls.Add(1)
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["project_id"] = "prepared-project"
	return updated, nil
}

func (e *requestPrepareExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.executeCalls.Add(1)
	if got := testStringValue(auth.Metadata["project_id"]); got != "prepared-project" {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusBadRequest, Message: "missing prepared project"}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *requestPrepareExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "stream not implemented"}
}

func (e *requestPrepareExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *requestPrepareExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "count not implemented"}
}

func (e *requestPrepareExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "http not implemented"}
}

func (e *requestPrepareContextExecutor) Identifier() string { return e.provider }

func (e *requestPrepareContextExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth == nil || auth.Metadata == nil || testStringValue(auth.Metadata["prepared"]) == ""
}

func (e *requestPrepareContextExecutor) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	e.prepareCalls.Add(1)
	if RefreshCoordinatorFrom(ctx) == nil {
		return nil, &Error{HTTPStatus: http.StatusInternalServerError, Message: "missing refresh coordinator"}
	}
	if got := coreusage.RequestedModelAliasFromContext(ctx); got != "client-model" {
		return nil, &Error{HTTPStatus: http.StatusInternalServerError, Message: "missing requested model alias"}
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["prepared"] = "true"
	return updated, nil
}

func (e *requestPrepareContextExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "execute not implemented"}
}

func (e *requestPrepareContextExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.streamCalls.Add(1)
	if auth == nil || testStringValue(auth.Metadata["prepared"]) != "true" {
		return nil, &Error{HTTPStatus: http.StatusInternalServerError, Message: "missing prepared auth"}
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-prepare","output":[]}}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *requestPrepareContextExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *requestPrepareContextExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.countCalls.Add(1)
	return cliproxyexecutor.Response{Payload: []byte("count-ok")}, nil
}

func (e *requestPrepareContextExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "http not implemented"}
}

func TestManagerExecute_PreparesAndPersistsMissingRequestAuthMetadata(t *testing.T) {
	const model = "gemini-3.1-pro"
	store := &requestPrepareStore{}
	executor := &requestPrepareExecutor{}
	manager := NewManager(store, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-request-prepare",
		Provider: "antigravity",
		Metadata: map[string]any{"access_token": "token"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	resp, errExecute := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	if string(resp.Payload) != "ok" {
		t.Fatalf("payload = %q, want ok", string(resp.Payload))
	}
	if got := executor.prepareCalls.Load(); got != 1 {
		t.Fatalf("prepare calls = %d, want 1", got)
	}
	if got := store.saveCount.Load(); got < 1 {
		t.Fatalf("save count = %d, want at least 1", got)
	}
	if got := testStringValue(store.lastAuth().Metadata["project_id"]); got != "prepared-project" {
		t.Fatalf("persisted project_id = %q, want prepared-project", got)
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth in manager")
	}
	if got := testStringValue(current.Metadata["project_id"]); got != "prepared-project" {
		t.Fatalf("manager project_id = %q, want prepared-project", got)
	}

	if _, errExecute = manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("second Execute error: %v", errExecute)
	}
	if got := executor.prepareCalls.Load(); got != 1 {
		t.Fatalf("prepare calls after second execute = %d, want 1", got)
	}
}

func TestManagerExecuteStream_PrepareRequestAuthHasRefreshCoordinator(t *testing.T) {
	const model = "client-model"
	executor := &requestPrepareContextExecutor{provider: "prepare-stream"}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &Auth{ID: "auth-prepare-stream", Provider: executor.Identifier(), Metadata: map[string]any{"access_token": "token"}}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, executor.Identifier(), []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	result, errStream := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatalf("ExecuteStream error: %v", errStream)
	}
	if result == nil || result.Chunks == nil {
		t.Fatal("ExecuteStream returned nil stream")
	}
	chunk, ok := <-result.Chunks
	if !ok {
		t.Fatal("stream closed before first chunk")
	}
	if len(chunk.Payload) == 0 || chunk.Err != nil {
		t.Fatalf("unexpected first chunk: payload=%q err=%v", chunk.Payload, chunk.Err)
	}
	if got := executor.prepareCalls.Load(); got != 1 {
		t.Fatalf("prepare calls = %d, want 1", got)
	}
	if got := executor.streamCalls.Load(); got != 1 {
		t.Fatalf("stream calls = %d, want 1", got)
	}
}

func TestManagerExecuteCount_PrepareRequestAuthHasRefreshCoordinator(t *testing.T) {
	const model = "client-model"
	executor := &requestPrepareContextExecutor{provider: "prepare-count"}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &Auth{ID: "auth-prepare-count", Provider: executor.Identifier(), Metadata: map[string]any{"access_token": "token"}}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, executor.Identifier(), []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	resp, errCount := manager.ExecuteCount(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errCount != nil {
		t.Fatalf("ExecuteCount error: %v", errCount)
	}
	if string(resp.Payload) != "count-ok" {
		t.Fatalf("CountTokens payload = %q, want count-ok", string(resp.Payload))
	}
	if got := executor.prepareCalls.Load(); got != 1 {
		t.Fatalf("prepare calls = %d, want 1", got)
	}
	if got := executor.countCalls.Load(); got != 1 {
		t.Fatalf("count calls = %d, want 1", got)
	}
}

func testStringValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []byte:
		return strings.TrimSpace(string(typed))
	default:
		return ""
	}
}
