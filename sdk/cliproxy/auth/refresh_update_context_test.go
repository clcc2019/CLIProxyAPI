package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type refreshUpdateCaptureStore struct {
	mu   sync.Mutex
	last *Auth
}

func (s *refreshUpdateCaptureStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *refreshUpdateCaptureStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if auth != nil {
		s.last = auth.Clone()
	}
	return "", nil
}

func (s *refreshUpdateCaptureStore) Delete(context.Context, string) error { return nil }

func (s *refreshUpdateCaptureStore) reset() {
	s.mu.Lock()
	s.last = nil
	s.mu.Unlock()
}

func (s *refreshUpdateCaptureStore) snapshot() *Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last.Clone()
}

type refreshUpdateExecutor struct{}

func (e refreshUpdateExecutor) Identifier() string { return "oauth" }

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

type executionAuthUpdateExecutor struct{}

func (e executionAuthUpdateExecutor) Identifier() string { return "codex" }

func (e executionAuthUpdateExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["headers"] = map[string]any{"X-Codex-Beta-Features": "first-feature"}
	PublishAuthUpdate(ctx, updated)
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e executionAuthUpdateExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e executionAuthUpdateExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }

func (e executionAuthUpdateExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e executionAuthUpdateExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerPersistsExecutionRefreshUpdate(t *testing.T) {
	store := &refreshUpdateCaptureStore{}
	manager := NewManager(store, nil, nil)
	manager.RegisterExecutor(refreshUpdateExecutor{})
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "oauth-auth",
		Provider: "oauth",
		Metadata: map[string]any{
			"type":         "oauth",
			"access_token": "old-token",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	store.reset()

	if _, err := manager.Execute(context.Background(), []string{"oauth"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	last := store.snapshot()
	if last == nil {
		t.Fatal("expected refreshed auth to be persisted")
	}
	if got := last.Metadata["access_token"]; got != "new-token" {
		t.Fatalf("persisted access token = %v, want new-token", got)
	}
}

func TestManagerPersistsExecutionAuthUpdate(t *testing.T) {
	store := &refreshUpdateCaptureStore{}
	manager := NewManager(store, nil, nil)
	manager.RegisterExecutor(executionAuthUpdateExecutor{})
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "token",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	store.reset()

	if _, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	last := store.snapshot()
	if last == nil {
		t.Fatal("expected auth update to be persisted")
	}
	headers, ok := last.Metadata["headers"].(map[string]any)
	if !ok {
		t.Fatalf("persisted headers = %T, want map[string]any", last.Metadata["headers"])
	}
	if got := headers["X-Codex-Beta-Features"]; got != "first-feature" {
		t.Fatalf("persisted beta features = %v, want first-feature", got)
	}
}

func TestManagerRefreshUpdatePreservesLatestEditableAuthFileFields(t *testing.T) {
	store := &refreshUpdateCaptureStore{}
	manager := NewManager(store, nil, nil)
	registered, err := manager.Register(context.Background(), &Auth{
		ID:       "oauth-auth",
		Provider: "oauth",
		Metadata: map[string]any{
			"type":         "oauth",
			"access_token": "old-token",
		},
		Attributes: map[string]string{"path": "/tmp/oauth-auth.json"},
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	staleRefresh := registered.Clone()
	staleRefresh.Metadata["access_token"] = "new-token"

	edited := registered.Clone()
	edited.Metadata["priority"] = 9
	edited.Metadata["proxy_url"] = "http://127.0.0.1:7890"
	edited.Metadata["disable_cooling"] = true
	edited.Metadata["headers"] = map[string]any{"X-Test": "1"}
	ApplyAuthFileOptionsFromMetadata(edited)
	ApplyCustomHeadersFromMetadata(edited)
	if _, err := manager.Update(context.Background(), edited); err != nil {
		t.Fatalf("Update(edited) error = %v", err)
	}
	store.reset()

	if _, err := manager.Update(WithRefreshUpdate(context.Background()), staleRefresh); err != nil {
		t.Fatalf("Update(refresh) error = %v", err)
	}
	last := store.snapshot()
	if last == nil {
		t.Fatal("expected refresh update to be persisted")
	}
	if got := last.Metadata["access_token"]; got != "new-token" {
		t.Fatalf("access_token = %v, want new-token", got)
	}
	if got := last.Metadata["priority"]; got != 9 {
		t.Fatalf("priority metadata = %v, want 9", got)
	}
	if got := last.Attributes["priority"]; got != "9" {
		t.Fatalf("priority attribute = %q, want 9", got)
	}
	if got := last.Metadata["proxy_url"]; got != "http://127.0.0.1:7890" {
		t.Fatalf("proxy_url metadata = %v, want proxy", got)
	}
	if got := last.ProxyURL; got != "http://127.0.0.1:7890" {
		t.Fatalf("ProxyURL = %q, want proxy", got)
	}
	if got := last.Metadata["disable_cooling"]; got != true {
		t.Fatalf("disable_cooling metadata = %v, want true", got)
	}
	if got := last.Attributes["header:X-Test"]; got != "1" {
		t.Fatalf("header attribute = %q, want 1", got)
	}
}

func TestManagerRefreshUpdatePreservesLatestCodexPinnedProfile(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registered, err := manager.Register(context.Background(), &Auth{
		ID:       "codex-refresh-profile",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
		},
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	staleRefresh := registered.Clone()
	staleRefresh.Metadata["access_token"] = "new-access"
	staleRefresh.Metadata["refresh_token"] = "new-refresh"

	pinned := registered.Clone()
	pinned.Metadata["codex_client_profile_pinned"] = true
	pinned.Metadata["originator"] = "codex_vscode"
	pinned.Metadata["user_agent"] = "codex_vscode/2.0.0"
	pinned.Metadata["headers"] = map[string]any{
		"Originator": "codex_vscode",
		"User-Agent": "codex_vscode/2.0.0",
		"Version":    "2.0.0",
	}
	ApplyCustomHeadersFromMetadata(pinned)
	if _, err = manager.Update(context.Background(), pinned); err != nil {
		t.Fatalf("Update(pinned) error = %v", err)
	}

	updated, err := manager.Update(WithRefreshUpdate(context.Background()), staleRefresh)
	if err != nil {
		t.Fatalf("Update(refresh) error = %v", err)
	}
	if got := updated.Metadata["access_token"]; got != "new-access" {
		t.Fatalf("access_token = %v, want refreshed token", got)
	}
	if got := updated.Metadata["refresh_token"]; got != "new-refresh" {
		t.Fatalf("refresh_token = %v, want rotated token", got)
	}
	if got := updated.Metadata["codex_client_profile_pinned"]; got != true {
		t.Fatalf("codex_client_profile_pinned = %v, want true", got)
	}
	if got := updated.Metadata["originator"]; got != "codex_vscode" {
		t.Fatalf("originator = %v, want codex_vscode", got)
	}
	if got := updated.Metadata["user_agent"]; got != "codex_vscode/2.0.0" {
		t.Fatalf("user_agent = %v, want pinned profile", got)
	}
}
