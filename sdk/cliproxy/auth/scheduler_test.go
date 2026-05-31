package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type schedulerTestExecutor struct{}

func (schedulerTestExecutor) Identifier() string { return "test" }

func (schedulerTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (schedulerTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (schedulerTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type schedulerCaptureExecutor struct {
	schedulerTestExecutor
	id     string
	apiKey string
	email  string
}

func (e *schedulerCaptureExecutor) Identifier() string { return e.id }

func (e *schedulerCaptureExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth != nil && auth.Attributes != nil {
		e.apiKey = auth.Attributes["api_key"]
	}
	if auth != nil && auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok {
			e.email = email
		}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

type trackingSelector struct {
	calls      int
	lastAuthID []string
}

func (s *trackingSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	s.calls++
	s.lastAuthID = s.lastAuthID[:0]
	for _, auth := range auths {
		s.lastAuthID = append(s.lastAuthID, auth.ID)
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[len(auths)-1], nil
}

func newSchedulerForTest(selector Selector, auths ...*Auth) *authScheduler {
	scheduler := newAuthScheduler(selector)
	scheduler.rebuild(auths)
	return scheduler
}

func registerSchedulerModels(t *testing.T, provider string, model string, authIDs ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			reg.UnregisterClient(authID)
		}
	})
}

func TestSchedulerPick_RoundRobinHighestPriority(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
		&Auth{ID: "high-b", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "high-a", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
	)

	want := []string{"high-a", "high-b", "high-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_FillFirstSticksToFirstReady(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "b", Provider: "gemini"},
		&Auth{ID: "a", Provider: "gemini"},
		&Auth{ID: "c", Provider: "gemini"},
	)

	for index := 0; index < 3; index++ {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != "a" {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, "a")
		}
	}
}

func TestSchedulerPick_PromotesExpiredCooldownBeforePick(t *testing.T) {
	t.Parallel()

	model := "gemini-2.5-pro"
	registerSchedulerModels(t, "gemini", model, "cooldown-expired")
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{
			ID:       "cooldown-expired",
			Provider: "gemini",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(-1 * time.Second),
				},
			},
		},
	)

	got, errPick := scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickSingle() auth = nil")
	}
	if got.ID != "cooldown-expired" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "cooldown-expired")
	}
}

func TestSchedulerPick_GeminiVirtualParentUsesTwoLevelRotation(t *testing.T) {
	t.Parallel()

	registerSchedulerModels(t, "gemini-cli", "gemini-2.5-pro", "cred-a::proj-1", "cred-a::proj-2", "cred-b::proj-1", "cred-b::proj-2")
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "cred-a::proj-1", Provider: "gemini-cli", Attributes: map[string]string{"gemini_virtual_parent": "cred-a"}},
		&Auth{ID: "cred-a::proj-2", Provider: "gemini-cli", Attributes: map[string]string{"gemini_virtual_parent": "cred-a"}},
		&Auth{ID: "cred-b::proj-1", Provider: "gemini-cli", Attributes: map[string]string{"gemini_virtual_parent": "cred-b"}},
		&Auth{ID: "cred-b::proj-2", Provider: "gemini-cli", Attributes: map[string]string{"gemini_virtual_parent": "cred-b"}},
	)

	wantParents := []string{"cred-a", "cred-b", "cred-a", "cred-b"}
	wantIDs := []string{"cred-a::proj-1", "cred-b::proj-1", "cred-a::proj-2", "cred-b::proj-2"}
	for index := range wantIDs {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini-cli", "gemini-2.5-pro", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
		if got.Attributes["gemini_virtual_parent"] != wantParents[index] {
			t.Fatalf("pickSingle() #%d parent = %q, want %q", index, got.Attributes["gemini_virtual_parent"], wantParents[index])
		}
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledSubset(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex"},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_PinnedAuthDiagnosticWhenNoPinnedCandidateAvailable(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-pinned", Provider: "codex"},
	)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "codex-pinned"},
	}
	_, errPick := scheduler.pickSingle(context.Background(), "codex", "", opts, map[string]struct{}{"codex-pinned": {}})
	if errPick == nil {
		t.Fatal("pickSingle() error = nil, want pinned auth diagnostic")
	}
	authErr, ok := errPick.(*Error)
	if !ok || authErr.Code != "auth_not_found" {
		t.Fatalf("pickSingle() error = %#v, want auth_not_found", errPick)
	}
	if !strings.Contains(authErr.Message, "pinned auth codex-pinned") {
		t.Fatalf("pickSingle() message = %q, want pinned auth id", authErr.Message)
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledAcrossPriorities(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_MixedProvidersUsesWeightedProviderRotationOverReadyCandidates(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "gemini-a", Provider: "gemini"},
		&Auth{ID: "gemini-b", Provider: "gemini"},
		&Auth{ID: "claude-a", Provider: "claude"},
	)

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestSchedulerPick_MixedProvidersPrefersHighestPriorityTier(t *testing.T) {
	t.Parallel()

	model := "gpt-default"
	registerSchedulerModels(t, "provider-low", model, "low")
	registerSchedulerModels(t, "provider-high-a", model, "high-a")
	registerSchedulerModels(t, "provider-high-b", model, "high-b")

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "provider-low", Attributes: map[string]string{"priority": "4"}},
		&Auth{ID: "high-a", Provider: "provider-high-a", Attributes: map[string]string{"priority": "7"}},
		&Auth{ID: "high-b", Provider: "provider-high-b", Attributes: map[string]string{"priority": "7"}},
	)

	providers := []string{"provider-low", "provider-high-a", "provider-high-b"}
	wantProviders := []string{"provider-high-a", "provider-high-b", "provider-high-a", "provider-high-b"}
	wantIDs := []string{"high-a", "high-b", "high-a", "high-b"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(context.Background(), providers, model, cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_UsesWeightedProviderRotationBeforeCredentialRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, map[string]struct{}{})
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_DisallowFreeAuthSkipsCodexFreePlan(t *testing.T) {
	t.Parallel()

	model := "gpt-5.4-mini"
	registerSchedulerModels(t, "codex", model, "codex-a-free", "codex-b-plus")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}}); errRegister != nil {
		t.Fatalf("Register(codex-a-free) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "codex-b-plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}); errRegister != nil {
		t.Fatalf("Register(codex-b-plus) error = %v", errRegister)
	}

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.DisallowFreeAuthMetadataKey: true},
	}
	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"codex"}, model, opts, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "codex" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "codex")
	}
	if got.ID != "codex-b-plus" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "codex-b-plus")
	}
}

func TestManagerExecuteSchedulerFastPathUsesFullNonCodexAuth(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &schedulerCaptureExecutor{id: "gemini"}
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "gemini-full",
		Provider: "gemini",
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":  "secret-key",
			"priority": "10",
		},
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}); errRegister != nil {
		t.Fatalf("Register(gemini-full) error = %v", errRegister)
	}

	if _, errExecute := manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if executor.apiKey != "secret-key" {
		t.Fatalf("executor api_key = %q, want %q", executor.apiKey, "secret-key")
	}
	if executor.email != "user@example.com" {
		t.Fatalf("executor email = %q, want %q", executor.email, "user@example.com")
	}
}

func TestManagerPickNextFastPathResyncsStaleDisabledAuth(t *testing.T) {
	for _, provider := range []string{"gemini", "codex"} {
		t.Run(provider, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.executors[provider] = schedulerTestExecutor{}
			authID := provider + "-stale"
			if _, errRegister := manager.Register(context.Background(), &Auth{
				ID:       authID,
				Provider: provider,
				Status:   StatusActive,
			}); errRegister != nil {
				t.Fatalf("Register(%s) error = %v", authID, errRegister)
			}

			manager.mu.Lock()
			manager.auths[authID].Disabled = true
			manager.mu.Unlock()

			got, _, errPick := manager.pickNext(context.Background(), provider, "", cliproxyexecutor.Options{}, map[string]struct{}{})
			if errPick == nil {
				t.Fatalf("pickNext() auth = %#v, want auth_not_found error", got)
			}
			authErr, ok := errPick.(*Error)
			if !ok || authErr.Code != "auth_not_found" {
				t.Fatalf("pickNext() error = %#v, want auth_not_found", errPick)
			}
		})
	}
}

func TestManagerCustomSelector_FallsBackToLegacyPath(t *testing.T) {
	t.Parallel()

	selector := &trackingSelector{}
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.auths["auth-a"] = &Auth{ID: "auth-a", Provider: "gemini"}
	manager.auths["auth-b"] = &Auth{ID: "auth-b", Provider: "gemini"}

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if selector.calls != 1 {
		t.Fatalf("selector.calls = %d, want %d", selector.calls, 1)
	}
	if len(selector.lastAuthID) != 2 {
		t.Fatalf("len(selector.lastAuthID) = %d, want %d", len(selector.lastAuthID), 2)
	}
	if got.ID != selector.lastAuthID[len(selector.lastAuthID)-1] {
		t.Fatalf("pickNext() auth.ID = %q, want selector-picked %q", got.ID, selector.lastAuthID[len(selector.lastAuthID)-1])
	}
}

func TestManager_InitializesSchedulerForBuiltInSelector(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if manager.scheduler == nil {
		t.Fatalf("manager.scheduler = nil")
	}
	if manager.scheduler.strategy != schedulerStrategyRoundRobin {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyRoundRobin)
	}

	manager.SetSelector(&FillFirstSelector{})
	if manager.scheduler.strategy != schedulerStrategyFillFirst {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyFillFirst)
	}
}

func TestManager_InitializesSchedulerForSessionAffinityFallback(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	if manager.scheduler == nil {
		t.Fatalf("manager.scheduler = nil")
	}
	if manager.scheduler.strategy != schedulerStrategyRoundRobin {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyRoundRobin)
	}
}

func TestManagerPickNextSessionAffinityUsesSchedulerAndFailsOver(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}

	for _, auth := range []*Auth{
		{ID: "affinity-a", Provider: "gemini", Status: StatusActive},
		{ID: "affinity-b", Provider: "gemini", Status: StatusActive},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"session_id":"session-a"}}`)}
	first, _, errPick := manager.pickNext(context.Background(), "gemini", "", opts, nil)
	if errPick != nil {
		t.Fatalf("first pickNext() error = %v", errPick)
	}
	second, _, errPick := manager.pickNext(context.Background(), "gemini", "", opts, nil)
	if errPick != nil {
		t.Fatalf("second pickNext() error = %v", errPick)
	}
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("same session picks = %v, %v; want same auth", first, second)
	}

	if _, errUpdate := manager.Update(context.Background(), &Auth{ID: first.ID, Provider: "gemini", Status: StatusDisabled, Disabled: true}); errUpdate != nil {
		t.Fatalf("Update(%s disabled) error = %v", first.ID, errUpdate)
	}
	third, _, errPick := manager.pickNext(context.Background(), "gemini", "", opts, nil)
	if errPick != nil {
		t.Fatalf("third pickNext() error = %v", errPick)
	}
	if third == nil || third.ID == first.ID {
		t.Fatalf("after cached auth disabled pick = %v, want a different auth than %s", third, first.ID)
	}
}

func TestManagerPickNextSessionAffinityInvalidatesUnavailableSchedulerCache(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}

	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "affinity-a", Provider: "gemini", Status: StatusActive}); errRegister != nil {
		t.Fatalf("Register(affinity-a) error = %v", errRegister)
	}

	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"session_id":"stale-session"}}`)}
	first, _, errPick := manager.pickNext(context.Background(), "gemini", "", opts, nil)
	if errPick != nil {
		t.Fatalf("first pickNext() error = %v", errPick)
	}
	if first == nil {
		t.Fatalf("first pickNext() auth = nil")
	}
	primaryID, _ := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	cacheKey := sessionAffinityCacheKey("gemini", primaryID, opts.Metadata)
	if cachedAuthID, ok := selector.cache.Get(cacheKey); !ok || cachedAuthID != first.ID {
		t.Fatalf("cached auth = %q/%v, want %s/true", cachedAuthID, ok, first.ID)
	}

	if _, errUpdate := manager.Update(context.Background(), &Auth{ID: first.ID, Provider: "gemini", Status: StatusDisabled, Disabled: true}); errUpdate != nil {
		t.Fatalf("Update(%s disabled) error = %v", first.ID, errUpdate)
	}
	if _, _, errPick = manager.pickNext(context.Background(), "gemini", "", opts, nil); errPick == nil {
		t.Fatalf("second pickNext() error = nil, want no available auth")
	}
	if cachedAuthID, ok := selector.cache.Get(cacheKey); ok {
		t.Fatalf("stale cached auth = %s, want cache entry invalidated", cachedAuthID)
	}
}

func TestManagerPickNextSessionAffinityFallbackCachePropagatesPickError(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)

	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversationState":{"conversationId":"primary-conv","agentContinuationId":"fallback-cont"}}`)}
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" || fallbackID == "" || primaryID == fallbackID {
		t.Fatalf("extractSessionIDs() = %q/%q, want distinct primary and fallback IDs", primaryID, fallbackID)
	}
	fallbackKey := sessionAffinityCacheKey("gemini", fallbackID, opts.Metadata)
	selector.cache.Set(fallbackKey, "affinity-a")

	auth, executor, errPick := manager.pickNext(context.Background(), "gemini", "", opts, nil)
	if errPick == nil {
		t.Fatalf("pickNext() error = nil, want executor_not_found")
	}
	if auth != nil || executor != nil {
		t.Fatalf("pickNext() auth/executor = %v/%v, want nil/nil on pick error", auth, executor)
	}
	authErr, ok := errPick.(*Error)
	if !ok || authErr.Code != "executor_not_found" {
		t.Fatalf("pickNext() error = %#v, want executor_not_found", errPick)
	}
	primaryKey := sessionAffinityCacheKey("gemini", primaryID, opts.Metadata)
	if cachedAuthID, ok := selector.cache.Get(primaryKey); ok {
		t.Fatalf("primary cached auth = %s, want no write after pick error", cachedAuthID)
	}
}

func TestManagerPickNextSessionAffinityStableAcrossRestart(t *testing.T) {
	t.Parallel()

	newManager := func(t *testing.T) *Manager {
		t.Helper()
		selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Minute,
		})
		t.Cleanup(selector.Stop)
		manager := NewManager(nil, selector, nil)
		manager.executors["gemini"] = schedulerTestExecutor{}
		for _, auth := range []*Auth{
			{ID: "affinity-a", Provider: "gemini", Status: StatusActive},
			{ID: "affinity-b", Provider: "gemini", Status: StatusActive},
			{ID: "affinity-c", Provider: "gemini", Status: StatusActive},
		} {
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
			}
		}
		return manager
	}

	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"session_id":"restart-session"}}`)}
	firstManager := newManager(t)
	first, _, errPick := firstManager.pickNext(context.Background(), "gemini", "", opts, nil)
	if errPick != nil {
		t.Fatalf("first manager pickNext() error = %v", errPick)
	}
	secondManager := newManager(t)
	second, _, errPick := secondManager.pickNext(context.Background(), "gemini", "", opts, nil)
	if errPick != nil {
		t.Fatalf("second manager pickNext() error = %v", errPick)
	}
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("restart picks = %v, %v; want same auth", first, second)
	}
}

func TestManagerPickNextSessionAffinityStableDistribution(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	for _, auth := range []*Auth{
		{ID: "affinity-a", Provider: "gemini", Status: StatusActive},
		{ID: "affinity-b", Provider: "gemini", Status: StatusActive},
		{ID: "affinity-c", Provider: "gemini", Status: StatusActive},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	seen := make(map[string]struct{}, 3)
	for index := 0; index < 64; index++ {
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"session_id":"session-` + strconv.Itoa(index) + `"}}`)}
		got, _, errPick := manager.pickNext(context.Background(), "gemini", "", opts, nil)
		if errPick != nil {
			t.Fatalf("pickNext() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNext() #%d auth = nil", index)
		}
		seen[got.ID] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("len(seen) = %d, want at least 2 auths used", len(seen))
	}
}

func TestManagerPickNextMixedSessionAffinityStableAcrossRestart(t *testing.T) {
	t.Parallel()

	newManager := func(t *testing.T) *Manager {
		t.Helper()
		selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
			Fallback: &RoundRobinSelector{},
			TTL:      time.Minute,
		})
		t.Cleanup(selector.Stop)
		manager := NewManager(nil, selector, nil)
		manager.executors["gemini"] = schedulerTestExecutor{}
		manager.executors["claude"] = schedulerTestExecutor{}
		for _, auth := range []*Auth{
			{ID: "gemini-a", Provider: "gemini", Status: StatusActive},
			{ID: "gemini-b", Provider: "gemini", Status: StatusActive},
			{ID: "claude-a", Provider: "claude", Status: StatusActive},
		} {
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
			}
		}
		return manager
	}

	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ClientPrincipalMetadataKey: "client-hash"}}
	firstManager := newManager(t)
	first, _, firstProvider, errPick := firstManager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", opts, nil)
	if errPick != nil {
		t.Fatalf("first manager pickNextMixed() error = %v", errPick)
	}
	secondManager := newManager(t)
	second, _, secondProvider, errPick := secondManager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", opts, nil)
	if errPick != nil {
		t.Fatalf("second manager pickNextMixed() error = %v", errPick)
	}
	if first == nil || second == nil || first.ID != second.ID || firstProvider != secondProvider {
		t.Fatalf("restart mixed picks = %v/%s, %v/%s; want same auth and provider", first, firstProvider, second, secondProvider)
	}
}

func TestManager_SchedulerTracksRegisterAndUpdate(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "auth-a" {
		t.Fatalf("scheduler.pickSingle() auth = %v, want auth-a", got)
	}

	if _, errUpdate := manager.Update(context.Background(), &Auth{ID: "auth-a", Provider: "gemini", Disabled: true}); errUpdate != nil {
		t.Fatalf("Update(auth-a) error = %v", errUpdate)
	}

	got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() after update error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("scheduler.pickSingle() after update auth = %v, want auth-b", got)
	}
}

func TestManager_SingleLegacySelectionRequired_ClearsCachedSafeResultOnRegister(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	routeModel := "team/gpt-5"
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "plain", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(plain) error = %v", errRegister)
	}

	if manager.singleLegacySelectionRequired("gemini", routeModel, nil) {
		t.Fatalf("singleLegacySelectionRequired() = true, want false before prefixed auth is added")
	}

	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "prefixed", Provider: "gemini", Prefix: "team"}); errRegister != nil {
		t.Fatalf("Register(prefixed) error = %v", errRegister)
	}

	if !manager.singleLegacySelectionRequired("gemini", routeModel, nil) {
		t.Fatalf("singleLegacySelectionRequired() = false, want true after prefixed auth is added")
	}
}

func TestManager_SingleLegacySelectionRequired_ClearsCachedSafeResultOnOAuthAliasChange(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{})
	routeModel := "claude-sonnet-4-5"
	auth := &Auth{ID: "claude-oauth", Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register(claude-oauth) error = %v", errRegister)
	}

	if manager.singleLegacySelectionRequired("claude", routeModel, nil) {
		t.Fatalf("singleLegacySelectionRequired() = true, want false before alias is configured")
	}

	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: "claude-sonnet-4-5-20250514", Alias: "claude-sonnet-4-5"}},
	})

	if !manager.singleLegacySelectionRequired("claude", routeModel, nil) {
		t.Fatalf("singleLegacySelectionRequired() = false, want true after alias is configured")
	}
}

func TestManager_PickNextMixed_UsesSchedulerRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_SkipsProvidersWithoutExecutors(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "claude")
	}
	if got.ID != "claude-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "claude-a")
	}
}

func TestManager_SchedulerTracksMarkResultCooldownAndRecovery(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient("auth-b", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient("auth-a")
		reg.UnregisterClient("auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "gemini",
		Model:    "test-model",
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
	})

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "test-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() after cooldown error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("scheduler.pickSingle() after cooldown auth = %v, want auth-b", got)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "gemini",
		Model:    "test-model",
		Success:  true,
	})

	seen := make(map[string]struct{}, 2)
	for index := 0; index < 2; index++ {
		got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", "test-model", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("scheduler.pickSingle() after recovery #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("scheduler.pickSingle() after recovery #%d auth = nil", index)
		}
		seen[got.ID] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("len(seen) = %d, want %d", len(seen), 2)
	}
}
