package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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

type refreshReloadStore struct {
	mu      sync.Mutex
	items   []*Auth
	last    *Auth
	saves   int
	listErr error
}

func (s *refreshReloadStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]*Auth, 0, len(s.items))
	for _, auth := range s.items {
		if auth != nil {
			out = append(out, auth.Clone())
		}
	}
	return out, nil
}

func (s *refreshReloadStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if auth == nil {
		return "", nil
	}
	cloned := auth.Clone()
	s.last = cloned
	for i, existing := range s.items {
		if existing != nil && existing.ID == cloned.ID {
			s.items[i] = cloned.Clone()
			return cloned.ID, nil
		}
	}
	s.items = append(s.items, cloned.Clone())
	return cloned.ID, nil
}

func (s *refreshReloadStore) Delete(context.Context, string) error { return nil }

func (s *refreshReloadStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

func TestManager_CoordinatedRefreshReloadsPersistedTokenBeforeRefresh(t *testing.T) {
	t.Parallel()

	current := &Auth{
		ID:       "codex-reload-before-refresh",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":    "old-access-token",
			"account_id": "acct-1",
		},
		Metadata: map[string]any{
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"account_id":    "acct-1",
			"email":         "codex@example.com",
		},
	}
	reloaded := current.Clone()
	reloaded.Attributes["api_key"] = "new-access-token"
	reloaded.Metadata["access_token"] = "new-access-token"
	reloaded.Metadata["refresh_token"] = "new-refresh-token"
	reloaded.Metadata["priority"] = 7

	store := &refreshReloadStore{items: []*Auth{reloaded}}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "codex",
		started:  make(chan string, 1),
		release:  make(chan struct{}),
	}
	close(executor.release)
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(WithSkipPersist(context.Background()), current); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	got, err := manager.coordinatedRefreshForRequest(context.Background(), current)
	if err != nil {
		t.Fatalf("coordinated refresh error = %v", err)
	}
	if got == nil {
		t.Fatal("coordinated refresh returned nil auth")
	}
	if calls := executor.calls.Load(); calls != 0 {
		t.Fatalf("refresh calls = %d, want 0", calls)
	}
	if gotToken := got.Metadata["access_token"]; gotToken != "new-access-token" {
		t.Fatalf("returned access_token = %v, want new-access-token", gotToken)
	}
	if gotRefresh := got.Metadata["refresh_token"]; gotRefresh != "new-refresh-token" {
		t.Fatalf("returned refresh_token = %v, want new-refresh-token", gotRefresh)
	}
	if gotAPIKey := got.Attributes["api_key"]; gotAPIKey != "new-access-token" {
		t.Fatalf("returned api_key = %q, want new-access-token", gotAPIKey)
	}
	if gotPriority := got.Metadata["priority"]; gotPriority != 7 {
		t.Fatalf("returned priority = %v, want 7", gotPriority)
	}
	inMemory, ok := manager.GetByID(current.ID)
	if !ok || inMemory == nil {
		t.Fatal("expected auth to remain in memory")
	}
	if gotToken := inMemory.Metadata["access_token"]; gotToken != "new-access-token" {
		t.Fatalf("in-memory access_token = %v, want new-access-token", gotToken)
	}
	if saves := store.saveCount(); saves != 0 {
		t.Fatalf("store saves = %d, want 0 for persisted-auth reload", saves)
	}
}

func TestManager_CoordinatedRefreshUsesExecutorWhenPersistedTokenUnchanged(t *testing.T) {
	t.Parallel()

	current := &Auth{
		ID:       "codex-reload-token-unchanged",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"account_id":    "acct-1",
		},
	}
	store := &refreshReloadStore{items: []*Auth{current.Clone()}}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "codex",
		started:  make(chan string, 1),
		release:  make(chan struct{}),
		mutate: func(auth *Auth) *Auth {
			updated := auth.Clone()
			updated.Metadata["access_token"] = "refreshed-access-token"
			updated.Metadata["refresh_token"] = "refreshed-refresh-token"
			return updated
		},
	}
	close(executor.release)
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(WithSkipPersist(context.Background()), current); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	got, err := manager.coordinatedRefreshForRequest(context.Background(), current)
	if err != nil {
		t.Fatalf("coordinated refresh error = %v", err)
	}
	if calls := executor.calls.Load(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
	if gotToken := got.Metadata["access_token"]; gotToken != "refreshed-access-token" {
		t.Fatalf("returned access_token = %v, want refreshed-access-token", gotToken)
	}
	if saves := store.saveCount(); saves != 1 {
		t.Fatalf("store saves = %d, want 1 for executor refresh", saves)
	}
}

func TestManager_CoordinatedRefreshIgnoresPersistedTokenForDifferentAccount(t *testing.T) {
	t.Parallel()

	current := &Auth{
		ID:       "codex-reload-account-mismatch",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"account_id":    "acct-1",
			"email":         "codex@example.com",
		},
	}
	reloaded := current.Clone()
	reloaded.Metadata["access_token"] = "other-access-token"
	reloaded.Metadata["refresh_token"] = "other-refresh-token"
	reloaded.Metadata["account_id"] = "acct-2"

	store := &refreshReloadStore{items: []*Auth{reloaded}}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "codex",
		started:  make(chan string, 1),
		release:  make(chan struct{}),
		mutate: func(auth *Auth) *Auth {
			updated := auth.Clone()
			updated.Metadata["access_token"] = "refreshed-access-token"
			updated.Metadata["refresh_token"] = "refreshed-refresh-token"
			return updated
		},
	}
	close(executor.release)
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(WithSkipPersist(context.Background()), current); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	got, err := manager.coordinatedRefreshForRequest(context.Background(), current)
	if err != nil {
		t.Fatalf("coordinated refresh error = %v", err)
	}
	if calls := executor.calls.Load(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
	if gotToken := got.Metadata["access_token"]; gotToken != "refreshed-access-token" {
		t.Fatalf("returned access_token = %v, want refreshed-access-token", gotToken)
	}
	if gotAccount := got.Metadata["account_id"]; gotAccount != "acct-1" {
		t.Fatalf("returned account_id = %v, want acct-1", gotAccount)
	}
}

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
