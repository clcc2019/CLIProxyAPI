package auth

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type autoRefreshTestExecutor struct {
	provider    string
	refreshFunc func(*Auth) (*Auth, error)
	mu          sync.Mutex
	ids         []string
}

func (e *autoRefreshTestExecutor) Identifier() string { return e.provider }

func (e *autoRefreshTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *autoRefreshTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *autoRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.ids = append(e.ids, auth.ID)
	e.mu.Unlock()
	if e.refreshFunc != nil {
		return e.refreshFunc(auth)
	}
	return auth, nil
}

func (e *autoRefreshTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *autoRefreshTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *autoRefreshTestExecutor) refreshedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.ids))
	copy(out, e.ids)
	return out
}

func TestManager_StartAutoRefresh_DisabledByDefault(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &autoRefreshTestExecutor{provider: "test"}
	manager.RegisterExecutor(executor)
	manager.SetConfig(&internalconfig.Config{})

	auth := &Auth{
		ID:       "startup-default",
		Provider: "test",
		Metadata: map[string]any{"refresh_interval_seconds": 3600},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := manager.StartAutoRefresh(ctx, time.Hour); started {
		t.Fatal("expected auto refresh to stay disabled by default")
	}
	t.Cleanup(manager.StopAutoRefresh)

	time.Sleep(100 * time.Millisecond)
	if got := len(executor.refreshedIDs()); got != 0 {
		t.Fatalf("startup refresh calls = %d, want 0", got)
	}
}

func TestManager_StartAutoRefresh_DefaultProviderStartsWithoutGlobalConfig(t *testing.T) {
	setDefaultAutoRefreshProvider(t, "default-on", true)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &autoRefreshTestExecutor{provider: "default-on"}
	manager.RegisterExecutor(executor)
	manager.SetConfig(&internalconfig.Config{})

	auth := &Auth{
		ID:       "startup-provider-default",
		Provider: "default-on",
		Metadata: map[string]any{"refresh_interval_seconds": 3600},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := manager.StartAutoRefresh(ctx, time.Hour); !started {
		t.Fatal("expected provider default auto refresh to start without global config")
	}
	t.Cleanup(manager.StopAutoRefresh)

	waitForRefreshCalls(t, executor, 1, time.Second)
}

func TestManager_StartAutoRefresh_DefaultProviderIgnoresGlobalDisabled(t *testing.T) {
	setDefaultAutoRefreshProvider(t, "default-on", true)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &autoRefreshTestExecutor{provider: "default-on"}
	manager.RegisterExecutor(executor)
	disabled := false
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			OAuthRefresh: internalconfig.OAuthRefreshConfig{Enabled: &disabled},
		},
	})

	auth := &Auth{
		ID:       "startup-provider-default-disabled-global",
		Provider: "default-on",
		Metadata: map[string]any{"refresh_interval_seconds": 3600},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := manager.StartAutoRefresh(ctx, time.Hour); !started {
		t.Fatal("expected provider default auto refresh to ignore global disabled switch")
	}
	t.Cleanup(manager.StopAutoRefresh)

	waitForRefreshCalls(t, executor, 1, time.Second)
}

func TestManager_StartAutoRefresh_DefaultProviderDoesNotRefreshOtherProviders(t *testing.T) {
	setDefaultAutoRefreshProvider(t, "default-on", true)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	defaultExecutor := &autoRefreshTestExecutor{provider: "default-on"}
	otherExecutor := &autoRefreshTestExecutor{provider: "other"}
	manager.RegisterExecutor(defaultExecutor)
	manager.RegisterExecutor(otherExecutor)
	manager.SetConfig(&internalconfig.Config{})

	for _, auth := range []*Auth{
		{ID: "default-auth", Provider: "default-on", Metadata: map[string]any{"refresh_interval_seconds": 3600}},
		{ID: "other-auth", Provider: "other", Metadata: map[string]any{"refresh_interval_seconds": 3600}},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := manager.StartAutoRefresh(ctx, time.Hour); !started {
		t.Fatal("expected provider default auto refresh to start without global config")
	}
	t.Cleanup(manager.StopAutoRefresh)

	waitForRefreshCalls(t, defaultExecutor, 1, time.Second)
	time.Sleep(100 * time.Millisecond)
	if got := len(otherExecutor.refreshedIDs()); got != 0 {
		t.Fatalf("other provider refresh calls = %d, want 0", got)
	}
}

func TestManager_Register_DefaultProviderAppliesMissingRefreshIntervalWithoutDelay(t *testing.T) {
	interval := 7 * time.Minute
	setDefaultAutoRefreshProviderWithInterval(t, "default-on", true, func() time.Duration {
		return interval
	})
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{})

	auth, err := manager.Register(context.Background(), &Auth{
		ID:       "missing-provider-default-interval",
		Provider: "default-on",
		Metadata: map[string]any{"email": "x@example.com"},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	if auth == nil {
		t.Fatal("register auth returned nil")
	}
	gotSeconds, ok := parseIntAny(auth.Metadata["refresh_interval_seconds"])
	if !ok {
		t.Fatalf("refresh_interval_seconds missing or invalid: %#v", auth.Metadata["refresh_interval_seconds"])
	}
	if wantSeconds := int(interval / time.Second); gotSeconds != wantSeconds {
		t.Fatalf("refresh_interval_seconds = %d, want %d", gotSeconds, wantSeconds)
	}
	if _, ok := authLastRefreshTimestamp(auth); ok {
		t.Fatal("last_refresh should not be synthesized for an old auth")
	}
	if !auth.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %s, want zero so due tokens are not delayed", auth.NextRefreshAfter)
	}
	if !manager.shouldRefresh(auth, time.Now()) {
		t.Fatal("expected auth missing last_refresh to be due immediately after default interval is applied")
	}
}

func TestManager_RefreshAuth_PreservesExecutorNextRefreshAfter(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	next := time.Now().UTC().Add(7 * time.Minute).Truncate(time.Second)
	executor := &autoRefreshTestExecutor{
		provider: "test",
		refreshFunc: func(auth *Auth) (*Auth, error) {
			updated := auth.Clone()
			updated.NextRefreshAfter = next
			return updated, nil
		},
	}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "preserve-executor-next-refresh",
		Provider: "test",
		Metadata: map[string]any{
			"last_refresh":             time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
			"refresh_interval_seconds": 1,
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.markRefreshPending(auth.ID, time.Now())

	manager.refreshAuth(context.Background(), auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth not found")
	}
	if !updated.NextRefreshAfter.Equal(next) {
		t.Fatalf("NextRefreshAfter = %s, want executor value %s", updated.NextRefreshAfter, next)
	}
}

func TestManager_RefreshAuthIfNeededUsesCurrentSnapshot(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	now := time.Now().UTC()
	executor := &autoRefreshTestExecutor{
		provider: "test",
		refreshFunc: func(auth *Auth) (*Auth, error) {
			updated := auth.Clone()
			if updated.Metadata == nil {
				updated.Metadata = map[string]any{}
			}
			refreshedAt := time.Now().UTC()
			updated.Metadata["access_token"] = "new-token"
			updated.Metadata["last_refresh"] = refreshedAt.Format(time.RFC3339)
			updated.Metadata["refresh_interval_seconds"] = 60
			updated.NextRefreshAfter = refreshedAt.Add(time.Minute)
			return updated, nil
		},
	}
	manager.RegisterExecutor(executor)
	enabled := true
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			OAuthRefresh: internalconfig.OAuthRefreshConfig{Enabled: &enabled},
		},
	})

	current := &Auth{
		ID:       "refresh-if-needed-current-snapshot",
		Provider: "test",
		Metadata: map[string]any{
			"last_refresh":             now.Add(-time.Hour).Format(time.RFC3339),
			"refresh_interval_seconds": 1,
		},
	}
	if _, err := manager.Register(context.Background(), current); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	staleCallerSnapshot := current.Clone()
	staleCallerSnapshot.Metadata["last_refresh"] = now.Format(time.RFC3339)
	staleCallerSnapshot.NextRefreshAfter = now.Add(time.Hour)

	updated, err := manager.RefreshAuthIfNeeded(context.Background(), staleCallerSnapshot)
	if err != nil {
		t.Fatalf("RefreshAuthIfNeeded() error = %v", err)
	}
	if updated == nil {
		t.Fatal("RefreshAuthIfNeeded() returned nil auth")
	}
	if got := updated.Metadata["access_token"]; got != "new-token" {
		t.Fatalf("access_token = %#v, want new-token", got)
	}
	if got := len(executor.refreshedIDs()); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestManager_StartAutoRefresh_EnabledImmediateByDefault(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &autoRefreshTestExecutor{provider: "test"}
	manager.RegisterExecutor(executor)

	enabled := true
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			OAuthRefresh: internalconfig.OAuthRefreshConfig{
				Enabled: &enabled,
			},
		},
	})

	auth := &Auth{
		ID:       "startup-disabled",
		Provider: "test",
		Metadata: map[string]any{"refresh_interval_seconds": 3600},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := manager.StartAutoRefresh(ctx, time.Hour); !started {
		t.Fatal("expected auto refresh to start when enabled")
	}
	t.Cleanup(manager.StopAutoRefresh)

	waitForRefreshCalls(t, executor, 1, time.Second)
}

func TestManager_StartAutoRefresh_EnabledCanSkipStartupCheck(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &autoRefreshTestExecutor{provider: "test"}
	manager.RegisterExecutor(executor)

	enabled := true
	disabled := false
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			OAuthRefresh: internalconfig.OAuthRefreshConfig{
				Enabled:   &enabled,
				OnStartup: &disabled,
			},
		},
	})

	auth := &Auth{
		ID:       "startup-disabled",
		Provider: "test",
		Metadata: map[string]any{"refresh_interval_seconds": 3600},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := manager.StartAutoRefresh(ctx, time.Hour); !started {
		t.Fatal("expected auto refresh to start when enabled")
	}
	t.Cleanup(manager.StopAutoRefresh)

	time.Sleep(100 * time.Millisecond)
	if got := len(executor.refreshedIDs()); got != 0 {
		t.Fatalf("startup refresh calls = %d, want 0", got)
	}
}

func TestManager_CheckRefreshes_HonorsBatchSize(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &autoRefreshTestExecutor{provider: "test"}
	manager.RegisterExecutor(executor)
	enabled := true
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			OAuthRefresh: internalconfig.OAuthRefreshConfig{
				Enabled:   &enabled,
				BatchSize: 2,
			},
		},
	})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		auth := &Auth{
			ID:       "batch-auth-" + strconv.Itoa(i),
			Provider: "test",
			Metadata: map[string]any{"refresh_interval_seconds": 3600},
		}
		if _, err := manager.Register(ctx, auth); err != nil {
			t.Fatalf("register auth %d: %v", i, err)
		}
	}

	manager.checkRefreshes(ctx)
	waitForRefreshCalls(t, executor, 2, time.Second)

	manager.checkRefreshes(ctx)
	waitForRefreshCalls(t, executor, 4, time.Second)

	manager.checkRefreshes(ctx)
	waitForRefreshCalls(t, executor, 5, time.Second)

	seen := make(map[string]int)
	for _, id := range executor.refreshedIDs() {
		seen[id]++
	}
	if len(seen) != 5 {
		t.Fatalf("refreshed unique auths = %d, want 5; got=%v", len(seen), seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("auth %s refreshed %d times, want 1", id, count)
		}
	}
}

func TestAutoRefreshLoop_HandleDueHonorsBatchSize(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &autoRefreshTestExecutor{provider: "test"}
	manager.RegisterExecutor(executor)
	enabled := true
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			OAuthRefresh: internalconfig.OAuthRefreshConfig{
				Enabled:   &enabled,
				BatchSize: 2,
			},
		},
	})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		auth := &Auth{
			ID:       "loop-batch-auth-" + strconv.Itoa(i),
			Provider: "test",
			Metadata: map[string]any{"refresh_interval_seconds": 3600},
		}
		if _, err := manager.Register(ctx, auth); err != nil {
			t.Fatalf("register auth %d: %v", i, err)
		}
	}

	loop := newAuthAutoRefreshLoop(manager, time.Hour, 1)
	now := time.Now()
	loop.rebuild(now)
	loop.handleDue(ctx, now)

	if got := len(loop.jobs); got != 2 {
		t.Fatalf("queued refresh jobs = %d, want 2", got)
	}
	next, ok := loop.peek()
	if !ok {
		t.Fatal("expected remaining due auths to stay scheduled")
	}
	if !next.After(now) {
		t.Fatalf("remaining due auths were not delayed: next=%s now=%s", next, now)
	}
}

func waitForRefreshCalls(t *testing.T, executor *autoRefreshTestExecutor, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := len(executor.refreshedIDs()); got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d refresh calls; got %d", want, len(executor.refreshedIDs()))
}
