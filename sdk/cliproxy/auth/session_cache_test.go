package auth

import (
	"testing"
	"time"
)

func TestSessionCacheCapacityEvictsOldest(t *testing.T) {
	cache := NewSessionCacheWithCapacity(time.Hour, 2)
	defer cache.Stop()
	cache.Set("session-1", "auth-1")
	cache.Set("session-2", "auth-2")
	cache.Set("session-3", "auth-3")
	if _, ok := cache.Get("session-1"); ok {
		t.Fatal("oldest session was not evicted")
	}
	if _, ok := cache.Get("session-2"); !ok {
		t.Fatal("session-2 was unexpectedly evicted")
	}
	if _, ok := cache.Get("session-3"); !ok {
		t.Fatal("session-3 was unexpectedly evicted")
	}
}
