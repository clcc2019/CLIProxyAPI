package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type removeCaptureStore struct {
	mu    sync.Mutex
	saved map[string]*Auth
}

type authContinuityResetCaptureExecutor struct {
	resetAuthID string
}

func (e *authContinuityResetCaptureExecutor) Identifier() string { return "codex" }
func (e *authContinuityResetCaptureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *authContinuityResetCaptureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *authContinuityResetCaptureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *authContinuityResetCaptureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *authContinuityResetCaptureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}
func (e *authContinuityResetCaptureExecutor) ResetAuthContinuity(authID string) {
	e.resetAuthID = authID
}

func TestManagerUpdateResetsContinuityWhenAuthFileChangesAccount(t *testing.T) {
	manager := NewManager(nil, NewSessionAffinitySelector(&RoundRobinSelector{}), nil)
	executor := &authContinuityResetCaptureExecutor{}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID: "same-file.json", Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{"auth_kind": "oauth", "account_id": "account-old", "access_token": "token-old"},
		Quota:    QuotaState{Exceeded: true},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.bindPreviousResponseID(context.Background(), "resp_old", auth.ID)

	replacement := auth.Clone()
	replacement.Metadata["account_id"] = "account-new"
	replacement.Metadata["access_token"] = "token-new"
	updated, err := manager.Update(context.Background(), replacement)
	if err != nil {
		t.Fatalf("update auth: %v", err)
	}
	if executor.resetAuthID != auth.ID {
		t.Fatalf("reset auth ID = %q, want %q", executor.resetAuthID, auth.ID)
	}
	if updated.Quota.Exceeded || updated.Unavailable || updated.LastError != nil {
		t.Fatalf("new principal inherited runtime failure state: %#v", updated)
	}
	if _, ok := manager.previousResponseAuths.GetAndRefresh("resp_old"); ok {
		t.Fatal("new principal inherited previous-response affinity")
	}
}

func TestManagerRefreshCannotRestoreReplacedAccount(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	current := &Auth{ID: "same-file.json", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"auth_kind": "oauth", "account_id": "account-new", "access_token": "token-new"}}
	if _, err := manager.Register(context.Background(), current); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	stale := current.Clone()
	stale.Metadata["account_id"] = "account-old"
	stale.Metadata["access_token"] = "token-old"
	updated, err := manager.Update(WithRefreshUpdate(context.Background()), stale)
	if err != nil {
		t.Fatalf("refresh update: %v", err)
	}
	if got := metadataString(updated.Metadata, "account_id"); got != "account-new" {
		t.Fatalf("account ID = %q, want account-new", got)
	}
}

func TestManagerIgnoresInFlightResultFromReplacedAccount(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	oldAuth := &Auth{ID: "same-file.json", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"auth_kind": "oauth", "account_id": "account-old"}}
	if _, err := manager.Register(context.Background(), oldAuth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	requestCtx := withExecutionAuthPrincipal(context.Background(), oldAuth)

	newAuth := oldAuth.Clone()
	newAuth.Metadata["account_id"] = "account-new"
	if _, err := manager.Update(context.Background(), newAuth); err != nil {
		t.Fatalf("replace auth: %v", err)
	}

	manager.MarkResult(requestCtx, Result{
		AuthID: oldAuth.ID,
		Model:  "gpt-5.4",
		Error:  &Error{HTTPStatus: http.StatusUnauthorized, Message: "old token rejected"},
	})
	current, ok := manager.GetByID(oldAuth.ID)
	if !ok || current == nil {
		t.Fatal("replacement auth not found")
	}
	if current.Failed != 0 || current.LastError != nil || current.Unavailable || len(current.ModelStates) != 0 {
		t.Fatalf("replacement auth was polluted by old result: %#v", current)
	}
}

func TestManagerIgnoresInFlightUpdatesFromReplacedAccount(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	oldAuth := &Auth{ID: "same-file.json", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"auth_kind": "oauth", "account_id": "account-old"}}
	if _, err := manager.Register(context.Background(), oldAuth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	requestCtx := withExecutionAuthPrincipal(context.Background(), oldAuth)

	newAuth := oldAuth.Clone()
	newAuth.Metadata["account_id"] = "account-new"
	if _, err := manager.Update(context.Background(), newAuth); err != nil {
		t.Fatalf("replace auth: %v", err)
	}

	staleRefresh := oldAuth.Clone()
	staleRefresh.Metadata["access_token"] = "old-refreshed-token"
	manager.handleExecutionRefreshUpdate(requestCtx, staleRefresh)
	manager.handleExecutionRateLimitUpdate(requestCtx, oldAuth.ID, []RateLimitSnapshot{{
		LimitID: "codex",
		Primary: &RateLimitWindow{UsedPercent: 100},
	}})

	current, ok := manager.GetByID(oldAuth.ID)
	if !ok || current == nil {
		t.Fatal("replacement auth not found")
	}
	if got := metadataString(current.Metadata, "account_id"); got != "account-new" {
		t.Fatalf("account ID = %q, want account-new", got)
	}
	if len(current.RateLimits) != 0 {
		t.Fatalf("replacement auth inherited old rate limits: %#v", current.RateLimits)
	}
}

func (s *removeCaptureStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *removeCaptureStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		s.saved = make(map[string]*Auth)
	}
	s.saved[auth.ID] = auth.Clone()
	return "", nil
}

func (s *removeCaptureStore) Delete(context.Context, string) error { return nil }

func (s *removeCaptureStore) savedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

func (s *removeCaptureStore) reset() {
	s.mu.Lock()
	s.saved = nil
	s.mu.Unlock()
}

func TestManagerRemoveSuppressesInFlightRefreshPersistence(t *testing.T) {
	store := &removeCaptureStore{}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "oauth",
		started:  make(chan string, 1),
		release:  make(chan struct{}),
		mutate: func(auth *Auth) *Auth {
			updated := auth.Clone()
			updated.Metadata["access_token"] = "new-access"
			updated.Metadata["refresh_token"] = "new-refresh"
			updated.NextRefreshAfter = time.Now().Add(time.Minute)
			return updated
		},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "oauth-delete-race.json",
		Provider: "oauth",
		Status:   StatusActive,
		Attributes: map[string]string{
			"path": "/tmp/oauth-delete-race.json",
		},
		Metadata: map[string]any{
			"type":          "oauth",
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	store.reset()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = manager.RefreshAuth(context.Background(), auth)
	}()
	waitForRefreshStart(t, executor.started, auth.ID)

	if removed, err := manager.Remove(context.Background(), auth.ID); err != nil || removed == nil {
		t.Fatalf("Remove() removed=%#v err=%v, want removed auth without error", removed, err)
	}
	close(executor.release)
	<-done
	manager.flushPersistQueue()

	if got := store.savedCount(); got != 0 {
		t.Fatalf("saved auth count after in-flight refresh = %d, want 0", got)
	}
	if current, ok := manager.GetByID(auth.ID); ok || current != nil {
		t.Fatalf("expected auth to stay removed, got auth=%#v ok=%v", current, ok)
	}
}
