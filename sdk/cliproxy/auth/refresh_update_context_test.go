package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type refreshUpdateCaptureStore struct {
	last *Auth
}

func (s *refreshUpdateCaptureStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *refreshUpdateCaptureStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth != nil {
		s.last = auth.Clone()
	}
	return "", nil
}

func (s *refreshUpdateCaptureStore) Delete(context.Context, string) error { return nil }

type refreshUpdateExecutor struct{}

func (e refreshUpdateExecutor) Identifier() string { return "kiro" }

func (e refreshUpdateExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	updated := auth.Clone()
	updated.Metadata["access_token"] = "new-token"
	PublishRefreshUpdate(ctx, updated)
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e refreshUpdateExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e refreshUpdateExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }

func (e refreshUpdateExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e refreshUpdateExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerPersistsExecutionRefreshUpdate(t *testing.T) {
	store := &refreshUpdateCaptureStore{}
	manager := NewManager(store, nil, nil)
	manager.RegisterExecutor(refreshUpdateExecutor{})
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "kiro-auth",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":         "kiro",
			"access_token": "old-token",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	store.last = nil

	if _, err := manager.Execute(context.Background(), []string{"kiro"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if store.last == nil {
		t.Fatal("expected refreshed auth to be persisted")
	}
	if got := store.last.Metadata["access_token"]; got != "new-token" {
		t.Fatalf("persisted access token = %v, want new-token", got)
	}
}
