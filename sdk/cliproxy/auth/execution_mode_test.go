package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type responseExecutionModeTestExecutor struct {
	executeCalls atomic.Int32
	countCalls   atomic.Int32
}

func (e *responseExecutionModeTestExecutor) Identifier() string { return "response-mode-test" }

func (e *responseExecutionModeTestExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.executeCalls.Add(1)
	updated := auth.Clone()
	updated.Metadata["mode_marker"] = "execute"
	PublishRefreshUpdate(ctx, updated)
	return cliproxyexecutor.Response{Payload: []byte(`{"id":"resp_execute"}`)}, nil
}

func (e *responseExecutionModeTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "stream not implemented"}
}

func (e *responseExecutionModeTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *responseExecutionModeTestExecutor) CountTokens(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.countCalls.Add(1)
	updated := auth.Clone()
	updated.Metadata["mode_marker"] = "count"
	PublishRefreshUpdate(ctx, updated)
	return cliproxyexecutor.Response{Payload: []byte(`{"id":"resp_count"}`)}, nil
}

func (e *responseExecutionModeTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "http not implemented"}
}

func TestManagerResponseExecutionModesPreserveDistinctBehavior(t *testing.T) {
	const (
		provider = "response-mode-test"
		model    = "response-mode-model"
		authID   = "response-mode-auth"
	)

	executor := &responseExecutionModeTestExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       authID,
		Provider: provider,
		Metadata: map[string]any{
			"access_token": "token",
			"mode_marker":  "initial",
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	resp, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(resp.Payload); got != `{"id":"resp_execute"}` {
		t.Fatalf("Execute() payload = %s", got)
	}
	if got := executor.executeCalls.Load(); got != 1 {
		t.Fatalf("Execute() calls = %d, want 1", got)
	}
	if got := executor.countCalls.Load(); got != 0 {
		t.Fatalf("CountTokens() calls after Execute = %d, want 0", got)
	}
	if selected, ok := manager.previousResponseAuths.GetAndRefresh("resp_execute"); !ok || selected != authID {
		t.Fatalf("Execute() previous-response binding = (%q, %v), want (%q, true)", selected, ok, authID)
	}
	current, ok := manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("expected registered auth after Execute")
	}
	if got := testStringValue(current.Metadata["mode_marker"]); got != "execute" {
		t.Fatalf("Execute() refresh callback marker = %q, want execute", got)
	}

	current.Metadata["mode_marker"] = "initial"
	if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), current); errUpdate != nil {
		t.Fatalf("reset auth marker: %v", errUpdate)
	}
	resp, errCount := manager.ExecuteCount(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errCount != nil {
		t.Fatalf("ExecuteCount() error = %v", errCount)
	}
	if got := string(resp.Payload); got != `{"id":"resp_count"}` {
		t.Fatalf("ExecuteCount() payload = %s", got)
	}
	if got := executor.executeCalls.Load(); got != 1 {
		t.Fatalf("Execute() calls after ExecuteCount = %d, want 1", got)
	}
	if got := executor.countCalls.Load(); got != 1 {
		t.Fatalf("CountTokens() calls = %d, want 1", got)
	}
	if selected, ok := manager.previousResponseAuths.GetAndRefresh("resp_count"); ok || selected != "" {
		t.Fatalf("ExecuteCount() unexpectedly bound previous response to %q", selected)
	}
	current, ok = manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("expected registered auth after ExecuteCount")
	}
	if got := testStringValue(current.Metadata["mode_marker"]); got != "initial" {
		t.Fatalf("ExecuteCount() unexpectedly ran refresh callback, marker = %q", got)
	}
}
