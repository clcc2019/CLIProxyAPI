package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type singleflightRefreshTestExecutor struct {
	provider string
	started  chan string
	release  chan struct{}
	calls    atomic.Int32
	mutate   func(*Auth) *Auth
}

func (e *singleflightRefreshTestExecutor) Identifier() string { return e.provider }

func (e *singleflightRefreshTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *singleflightRefreshTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *singleflightRefreshTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.calls.Add(1)
	select {
	case e.started <- auth.ID:
	default:
	}
	<-e.release
	if e.mutate != nil {
		return e.mutate(auth), nil
	}
	return auth, nil
}

func (e *singleflightRefreshTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *singleflightRefreshTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManager_RefreshAuth_SingleflightByAuthID(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "test",
		started:  make(chan string, 8),
		release:  make(chan struct{}),
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{ID: "singleflight-auth", Provider: "test"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.refreshAuth(context.Background(), auth.ID)
		}()
	}

	waitForRefreshStart(t, executor.started, "singleflight-auth")
	time.Sleep(20 * time.Millisecond)
	close(executor.release)
	wg.Wait()

	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestManager_CoordinatedRefreshJoinsBackgroundResult(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "test",
		started:  make(chan string, 2),
		release:  make(chan struct{}),
		mutate: func(auth *Auth) *Auth {
			updated := auth.Clone()
			if updated.Metadata == nil {
				updated.Metadata = map[string]any{}
			}
			updated.Metadata["access_token"] = "new-token"
			updated.NextRefreshAfter = time.Now().Add(time.Minute)
			return updated
		},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "coordinated-joins-background",
		Provider: "test",
		Metadata: map[string]any{"access_token": "old-token"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		manager.refreshAuth(context.Background(), auth.ID)
	}()
	waitForRefreshStart(t, executor.started, auth.ID)

	type refreshResult struct {
		auth *Auth
		err  error
	}
	resultCh := make(chan refreshResult, 1)
	go func() {
		got, err := manager.coordinatedRefreshForRequest(context.Background(), auth)
		resultCh <- refreshResult{auth: got, err: err}
	}()

	time.Sleep(20 * time.Millisecond)
	close(executor.release)
	bg.Wait()

	var result refreshResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("coordinated refresh did not return")
	}
	if result.err != nil {
		t.Fatalf("coordinated refresh error = %v", result.err)
	}
	if result.auth == nil {
		t.Fatal("coordinated refresh returned nil auth")
	}
	if got := result.auth.Metadata["access_token"]; got != "new-token" {
		t.Fatalf("access_token = %v, want new-token", got)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

type failingRefreshPersistStore struct {
	err  error
	last *Auth
}

func (s *failingRefreshPersistStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *failingRefreshPersistStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth != nil {
		s.last = auth.Clone()
	}
	return "", s.err
}

func (s *failingRefreshPersistStore) Delete(context.Context, string) error { return nil }

func TestManager_CoordinatedRefreshReturnsPersistError(t *testing.T) {
	t.Parallel()

	saveErr := errors.New("save failed")
	store := &failingRefreshPersistStore{err: saveErr}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "kiro",
		started:  make(chan string, 1),
		release:  make(chan struct{}),
		mutate: func(auth *Auth) *Auth {
			updated := auth.Clone()
			if updated.Metadata == nil {
				updated.Metadata = map[string]any{}
			}
			updated.Metadata["access_token"] = "new-token"
			updated.Metadata["refresh_token"] = "new-refresh-token"
			updated.NextRefreshAfter = time.Now().Add(time.Minute)
			return updated
		},
	}
	close(executor.release)
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "kiro-persist-failure",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"access_token":  "old-token",
			"refresh_token": "old-refresh-token",
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	result, err := manager.coordinatedRefreshForRequest(context.Background(), auth)
	if !errors.Is(err, saveErr) {
		t.Fatalf("coordinated refresh error = %v, want %v", err, saveErr)
	}
	if result == nil {
		t.Fatal("coordinated refresh returned nil auth")
	}
	if got := result.Metadata["refresh_token"]; got != "new-refresh-token" {
		t.Fatalf("returned refresh_token = %v, want new-refresh-token", got)
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || current == nil {
		t.Fatal("expected auth to remain in memory")
	}
	if got := current.Metadata["refresh_token"]; got != "new-refresh-token" {
		t.Fatalf("in-memory refresh_token = %v, want new-refresh-token", got)
	}
	if store.last == nil {
		t.Fatal("expected refreshed auth to be written to store")
	}
	if got := store.last.Metadata["refresh_token"]; got != "new-refresh-token" {
		t.Fatalf("persisted refresh_token = %v, want new-refresh-token", got)
	}
}

func TestManager_RefreshAuth_DifferentAuthIDsRemainIndependent(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "test",
		started:  make(chan string, 2),
		release:  make(chan struct{}),
	}
	manager.RegisterExecutor(executor)

	for _, authID := range []string{"auth-a", "auth-b"} {
		if _, err := manager.Register(context.Background(), &Auth{ID: authID, Provider: "test"}); err != nil {
			t.Fatalf("register %s: %v", authID, err)
		}
	}

	var wg sync.WaitGroup
	for _, authID := range []string{"auth-a", "auth-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			manager.refreshAuth(context.Background(), id)
		}(authID)
	}

	waitForRefreshStart(t, executor.started, "")
	waitForRefreshStart(t, executor.started, "")
	close(executor.release)
	wg.Wait()

	if got := executor.calls.Load(); got != 2 {
		t.Fatalf("refresh calls = %d, want 2", got)
	}
}

func waitForRefreshStart(t *testing.T, started <-chan string, want string) {
	t.Helper()

	select {
	case got := <-started:
		if want != "" && got != want {
			t.Fatalf("refresh started for %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh start")
	}
}
