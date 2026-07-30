package executor

import (
	"testing"
	"time"
)

func TestCodexHTTPTurnStateStoreBoundsEntriesUsingLRU(t *testing.T) {
	store := newCodexHTTPTurnStateStoreWithLimit(2)
	now := time.Now()
	store.put("first", "scope", "state-first", now)
	store.put("second", "scope", "state-second", now.Add(time.Second))
	if got := store.get("first", "scope", now.Add(2*time.Second)); got != "state-first" {
		t.Fatalf("first state = %q, want state-first", got)
	}
	store.put("third", "scope", "state-third", now.Add(3*time.Second))

	if got := store.get("second", "scope", now.Add(4*time.Second)); got != "" {
		t.Fatalf("second state = %q, want evicted", got)
	}
	if got := store.get("first", "scope", now.Add(4*time.Second)); got != "state-first" {
		t.Fatalf("first state = %q, want state-first", got)
	}
	if got := store.get("third", "scope", now.Add(4*time.Second)); got != "state-third" {
		t.Fatalf("third state = %q, want state-third", got)
	}
	if got := len(store.entries); got != 2 {
		t.Fatalf("entry count = %d, want 2", got)
	}
}

func TestCodexHTTPTurnStateStoreRemovesExpiredEntriesFromLRUEnd(t *testing.T) {
	store := newCodexHTTPTurnStateStoreWithLimit(2)
	now := time.Now()
	store.put("expired", "scope", "state-expired", now.Add(-codexHTTPTurnStateTTL-time.Second))
	store.put("fresh", "scope", "state-fresh", now)

	if got := store.get("expired", "scope", now); got != "" {
		t.Fatalf("expired state = %q, want empty", got)
	}
	if got := store.get("fresh", "scope", now); got != "state-fresh" {
		t.Fatalf("fresh state = %q, want state-fresh", got)
	}
	if got := len(store.entries); got != 1 {
		t.Fatalf("entry count = %d, want 1", got)
	}
}
