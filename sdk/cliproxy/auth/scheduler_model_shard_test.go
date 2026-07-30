package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestProviderSchedulerBoundsModelShardsAndRetainsRecentlyUsedShard(t *testing.T) {
	p := &providerScheduler{
		providerKey: "codex",
		auths:       make(map[string]*scheduledAuthMeta),
		modelShards: make(map[string]*modelScheduler),
	}
	now := time.Now()
	p.ensureModel("sticky-model", now)
	for i := 0; i < schedulerMaxModelShardsPerProvider-1; i++ {
		p.ensureModel(fmt.Sprintf("model-%03d", i), now.Add(time.Duration(i+1)*time.Nanosecond))
	}
	// Refresh the first shard so it is no longer the least recently used entry.
	p.ensureModel("sticky-model", now.Add(time.Second))
	p.ensureModel("overflow-model", now.Add(2*time.Second))

	p.mu.RLock()
	defer p.mu.RUnlock()
	if got := len(p.modelShards); got != schedulerMaxModelShardsPerProvider {
		t.Fatalf("model shard count = %d, want %d", got, schedulerMaxModelShardsPerProvider)
	}
	if p.modelShards["sticky-model"] == nil {
		t.Fatal("recently used shard was evicted")
	}
	if p.modelShards["model-000"] != nil {
		t.Fatal("least recently used shard was not evicted")
	}
}

func TestProviderSchedulerEvictsExpiredModelShards(t *testing.T) {
	now := time.Now()
	expired := &modelScheduler{modelKey: "expired"}
	expired.touch(now.Add(-schedulerModelShardTTL - time.Second))
	fresh := &modelScheduler{modelKey: "fresh"}
	fresh.touch(now)
	p := &providerScheduler{
		modelShards: map[string]*modelScheduler{
			"expired": expired,
			"fresh":   fresh,
		},
	}

	p.mu.Lock()
	p.evictExpiredModelShardsLocked(now)
	p.mu.Unlock()

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.modelShards["expired"] != nil {
		t.Fatal("expired model shard was retained")
	}
	if p.modelShards["fresh"] == nil {
		t.Fatal("fresh model shard was evicted")
	}
}

func TestAuthSchedulerBoundsMixedProviderCursors(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{})
	for i := 0; i <= schedulerMaxMixedCursors; i++ {
		scheduler.ensureSmallMixedCursor(mixedCursorKey{
			modelKey:      fmt.Sprintf("small-model-%d", i),
			providerCount: 1,
			providers:     [4]string{"codex"},
		})
		scheduler.ensureLargeMixedCursor(fmt.Sprintf("large-model-%d", i))
	}

	scheduler.mixedCursorMu.RLock()
	defer scheduler.mixedCursorMu.RUnlock()
	if got := len(scheduler.mixedCursors); got > schedulerMaxMixedCursors {
		t.Fatalf("small mixed cursor count = %d, maximum %d", got, schedulerMaxMixedCursors)
	}
	if got := len(scheduler.largeCursors); got > schedulerMaxMixedCursors {
		t.Fatalf("large mixed cursor count = %d, maximum %d", got, schedulerMaxMixedCursors)
	}
	if scheduler.mixedCursors[mixedCursorKey{modelKey: "small-model-0", providerCount: 1, providers: [4]string{"codex"}}] != nil {
		t.Fatal("oldest small mixed cursor was retained after cache reset")
	}
	if scheduler.largeCursors["large-model-0"] != nil {
		t.Fatal("oldest large mixed cursor was retained after cache reset")
	}
}
