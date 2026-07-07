package executor

import (
	"fmt"
	"testing"
	"time"
)

func TestCodexWindowStateStoreCleanupRotatesAcrossShards(t *testing.T) {
	store := newCodexWindowStateStore()
	staleSeen := time.Now().Add(-codexWindowStateTTL - time.Hour)

	staleKeys := make([]string, 0, codexWindowStateShards)
	for i := range store.shards {
		key := fmt.Sprintf("stale-%d", i)
		shard := &store.shards[i]
		shard.mu.Lock()
		shard.sessions[key] = codexWindowStateEntry{
			generation: 1,
			lastSeen:   staleSeen,
		}
		shard.mu.Unlock()
		staleKeys = append(staleKeys, key)
	}

	for i := 0; i < codexWindowStateCleanupInterval*codexWindowStateShards; i++ {
		store.currentGeneration("active-session")
	}

	for i, key := range staleKeys {
		shard := &store.shards[i]
		shard.mu.Lock()
		_, exists := shard.sessions[key]
		shard.mu.Unlock()
		if exists {
			t.Fatalf("stale session %q remained in shard %d", key, i)
		}
	}
}

func TestCodexWindowStateStoreCurrentWindowIDCachesUntilAdvance(t *testing.T) {
	store := newCodexWindowStateStore()

	first := store.currentWindowID("session-1")
	second := store.currentWindowID("session-1")
	if first != "session-1:0" {
		t.Fatalf("first window id = %q, want session-1:0", first)
	}
	if second != first {
		t.Fatalf("cached window id = %q, want %q", second, first)
	}

	store.advance("session-1")
	advanced := store.currentWindowID("session-1")
	if advanced != "session-1:1" {
		t.Fatalf("advanced window id = %q, want session-1:1", advanced)
	}
}
