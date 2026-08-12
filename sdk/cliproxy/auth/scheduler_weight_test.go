package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func newWeightedSchedulerManager(t *testing.T, selector Selector, auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, selector, nil)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		manager.executors[auth.Provider] = schedulerTestExecutor{}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}
	return manager
}

func TestManagerWeightedSelectorUsesSchedulerFastPath(t *testing.T) {
	t.Parallel()

	manager := newWeightedSchedulerManager(t, &WeightedRoundRobinSelector{},
		&Auth{ID: "weight-a", Provider: "codex", Attributes: map[string]string{AttributeWeight: "5"}},
		&Auth{ID: "weight-b", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1"}},
		&Auth{ID: "weight-zero", Provider: "codex", Attributes: map[string]string{AttributeWeight: "0"}},
	)
	if !manager.useSchedulerFastPath() {
		t.Fatal("weighted selector must use the scheduler fast path")
	}
	if manager.scheduler == nil || manager.scheduler.strategy != schedulerStrategyWeightedRoundRobin {
		t.Fatalf("scheduler strategy = %#v, want weighted round-robin", manager.scheduler)
	}

	counts := make(map[string]int)
	for i := 0; i < 60; i++ {
		picked, _, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNext() #%d error = %v", i, err)
		}
		counts[picked.ID]++
	}
	if counts["weight-a"] != 50 || counts["weight-b"] != 10 || counts["weight-zero"] != 0 {
		t.Fatalf("scheduler weighted picks = %#v, want 50:10:0", counts)
	}
}

func TestManagerWeightedSchedulerHonorsInFlightLoad(t *testing.T) {
	t.Parallel()

	manager := newWeightedSchedulerManager(t, &WeightedRoundRobinSelector{},
		&Auth{ID: "loaded", Provider: "codex", Attributes: map[string]string{AttributeWeight: "100"}},
		&Auth{ID: "idle", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1"}},
	)
	manager.scheduler.authLoad = func(authID string) int64 {
		if authID == "loaded" {
			return 1
		}
		return 0
	}

	picked, _, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if err != nil || picked == nil || picked.ID != "idle" {
		t.Fatalf("pickNext() = %#v, %v; want idle lowest-load auth", picked, err)
	}
}

func TestManagerWeightedSchedulerHonorsCodexQuotaUrgency(t *testing.T) {
	t.Parallel()

	now := time.Now()
	heavy := schedulerCodexQuotaAuth("quota-heavy", now, 80, 5*time.Hour, nil)
	heavy.Attributes = map[string]string{AttributeWeight: "100"}
	light := schedulerCodexQuotaAuth("quota-light", now, 0, 5*time.Hour, nil)
	light.Attributes = map[string]string{AttributeWeight: "1"}
	manager := newWeightedSchedulerManager(t, &WeightedRoundRobinSelector{}, heavy, light)

	for i := 0; i < 4; i++ {
		picked, _, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
		if err != nil || picked == nil || picked.ID != "quota-light" {
			t.Fatalf("pickNext() #%d = %#v, %v; want quota-light", i, picked, err)
		}
	}
}

func TestManagerWeightedSchedulerPriorityPrecedesCodexWebsocketPreference(t *testing.T) {
	t.Parallel()

	manager := newWeightedSchedulerManager(t, &WeightedRoundRobinSelector{},
		&Auth{ID: "http-high", Provider: "codex", Attributes: map[string]string{"priority": "10", AttributeWeight: "100"}},
		&Auth{ID: "ws-low", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true", AttributeWeight: "1"}},
	)
	ctx := cliproxyexecutor.WithPreferUpstreamWebsocket(context.Background())
	for i := 0; i < 4; i++ {
		picked, _, err := manager.pickNext(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if err != nil || picked == nil || picked.ID != "http-high" {
			t.Fatalf("pickNext() #%d = %#v, %v; want http-high", i, picked, err)
		}
	}
}

func TestManagerWeightedSchedulerResetsCreditsAfterWeightChange(t *testing.T) {
	t.Parallel()

	manager := newWeightedSchedulerManager(t, &WeightedRoundRobinSelector{},
		&Auth{ID: "reset-a", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1000000"}},
		&Auth{ID: "reset-b", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1"}},
	)
	for i := 0; i < 20; i++ {
		if _, _, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil); err != nil {
			t.Fatalf("warmup pickNext() #%d error = %v", i, err)
		}
	}
	if _, err := manager.Update(context.Background(), &Auth{ID: "reset-a", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1"}}); err != nil {
		t.Fatalf("Update(reset-a) error = %v", err)
	}

	counts := make(map[string]int)
	for i := 0; i < 12; i++ {
		picked, _, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("updated pickNext() #%d error = %v", i, err)
		}
		counts[picked.ID]++
	}
	if counts["reset-a"] != 6 || counts["reset-b"] != 6 {
		t.Fatalf("updated weighted picks = %#v, want 6:6", counts)
	}
}

func TestManagerWeightedSessionAffinityUsesWeightedBindingAndFailsOver(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&WeightedRoundRobinSelector{})
	defer selector.Stop()
	manager := newWeightedSchedulerManager(t, selector,
		&Auth{ID: "session-a", Provider: "codex", Attributes: map[string]string{AttributeWeight: "5"}},
		&Auth{ID: "session-b", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1"}},
	)
	if !manager.useSchedulerFastPathForProvider("codex") {
		t.Fatal("weighted session-affinity selector must use the scheduler fast path")
	}

	counts := make(map[string]int)
	for i := 0; i < 6; i++ {
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(fmt.Sprintf(`{"metadata":{"session_id":"weighted-session-%d"}}`, i))}
		picked, _, err := manager.pickNext(context.Background(), "codex", "", opts, nil)
		if err != nil {
			t.Fatalf("pickNext() #%d error = %v", i, err)
		}
		counts[picked.ID]++
	}
	if counts["session-a"] != 5 || counts["session-b"] != 1 {
		t.Fatalf("weighted session bindings = %#v, want 5:1", counts)
	}

	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"session_id":"rebind-session"}}`)}
	first, _, err := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if err != nil || first == nil {
		t.Fatalf("initial affinity pick = %#v, %v", first, err)
	}
	if _, err = manager.Update(context.Background(), &Auth{ID: first.ID, Provider: "codex", Attributes: map[string]string{AttributeWeight: "0"}}); err != nil {
		t.Fatalf("Update(%s) error = %v", first.ID, err)
	}
	second, _, err := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if err != nil || second == nil || second.ID == first.ID {
		t.Fatalf("rebound affinity pick = %#v, %v; want a different positive-weight auth", second, err)
	}
}

func TestManagerWeightedSchedulerBalancesMixedProviders(t *testing.T) {
	t.Parallel()

	manager := newWeightedSchedulerManager(t, &WeightedRoundRobinSelector{},
		&Auth{ID: "codex-heavy", Provider: "codex", Attributes: map[string]string{AttributeWeight: "5"}},
		&Auth{ID: "claude-light", Provider: "claude", Attributes: map[string]string{AttributeWeight: "1"}},
	)
	counts := make(map[string]int)
	for i := 0; i < 60; i++ {
		picked, _, _, err := manager.pickNextMixed(context.Background(), []string{"codex", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", i, err)
		}
		counts[picked.ID]++
	}
	if counts["codex-heavy"] != 50 || counts["claude-light"] != 10 {
		t.Fatalf("mixed weighted picks = %#v, want codex:claude = 50:10", counts)
	}
}
