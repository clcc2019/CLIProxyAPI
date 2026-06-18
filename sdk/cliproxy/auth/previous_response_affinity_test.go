package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type fakePreviousResponseStore struct {
	mu       sync.Mutex
	bindings map[string]string
	ttls     map[string]time.Duration
}

func newFakePreviousResponseStore() *fakePreviousResponseStore {
	return &fakePreviousResponseStore{
		bindings: make(map[string]string),
		ttls:     make(map[string]time.Duration),
	}
}

func (s *fakePreviousResponseStore) GetPreviousResponseAuth(_ context.Context, responseID string, ttl time.Duration) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	authID, ok := s.bindings[responseID]
	if ok {
		s.ttls[responseID] = ttl
	}
	return authID, ok, nil
}

func (s *fakePreviousResponseStore) SetPreviousResponseAuth(_ context.Context, responseID, authID string, ttl time.Duration) error {
	s.mu.Lock()
	s.bindings[responseID] = authID
	s.ttls[responseID] = ttl
	s.mu.Unlock()
	return nil
}

func (s *fakePreviousResponseStore) DeletePreviousResponseAuth(_ context.Context, responseID string) error {
	s.mu.Lock()
	delete(s.bindings, responseID)
	delete(s.ttls, responseID)
	s.mu.Unlock()
	return nil
}

func TestPreviousResponseAuthCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newPreviousResponseAuthCache(time.Hour, 2)
	cache.Set("resp_1", "auth_1")
	cache.Set("resp_2", "auth_2")
	if _, ok := cache.GetAndRefresh("resp_1"); !ok {
		t.Fatal("expected resp_1 cache hit")
	}

	cache.Set("resp_3", "auth_3")

	if _, ok := cache.GetAndRefresh("resp_2"); ok {
		t.Fatal("expected least recently used resp_2 to be evicted")
	}
	if authID, ok := cache.GetAndRefresh("resp_1"); !ok || authID != "auth_1" {
		t.Fatalf("resp_1 = %q, %v; want auth_1, true", authID, ok)
	}
	if authID, ok := cache.GetAndRefresh("resp_3"); !ok || authID != "auth_3" {
		t.Fatalf("resp_3 = %q, %v; want auth_3, true", authID, ok)
	}
}

func TestPreviousResponseAffinityLoadsBindingAcrossManagers(t *testing.T) {
	store := newFakePreviousResponseStore()
	first := NewManager(nil, nil, nil)
	second := NewManager(nil, nil, nil)
	first.SetPreviousResponseStore(store)
	second.SetPreviousResponseStore(store)

	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			PreviousResponseAffinityTTL:        "45m",
			PreviousResponseAffinityMaxEntries: 16,
		},
	}
	first.SetConfig(cfg)
	second.SetConfig(cfg)

	first.bindPreviousResponseID(context.Background(), "resp_shared", "auth_1")

	req := cliproxyexecutor.Request{Payload: []byte(`{"previous_response_id":"resp_shared"}`)}
	responseID, authID := second.previousResponsePinnedAuthID(context.Background(), req, cliproxyexecutor.Options{})
	if responseID != "resp_shared" || authID != "auth_1" {
		t.Fatalf("remote affinity = (%q, %q), want (resp_shared, auth_1)", responseID, authID)
	}

	store.mu.Lock()
	delete(store.bindings, "resp_shared")
	ttl := store.ttls["resp_shared"]
	store.mu.Unlock()
	if ttl != 45*time.Minute {
		t.Fatalf("persisted TTL = %s, want 45m", ttl)
	}

	_, authID = second.previousResponsePinnedAuthID(context.Background(), req, cliproxyexecutor.Options{})
	if authID != "auth_1" {
		t.Fatalf("local hot-cache affinity = %q, want auth_1", authID)
	}
}

func TestPreviousResponseAffinityInvalidationDeletesExternalBinding(t *testing.T) {
	store := newFakePreviousResponseStore()
	manager := NewManager(nil, nil, nil)
	manager.SetPreviousResponseStore(store)
	manager.bindPreviousResponseID(context.Background(), "resp_stale", "auth_1")

	manager.invalidatePreviousResponseID(context.Background(), "resp_stale")

	store.mu.Lock()
	_, exists := store.bindings["resp_stale"]
	store.mu.Unlock()
	if exists {
		t.Fatal("external previous-response binding was not deleted")
	}
	if _, ok := manager.previousResponseAuths.GetAndRefresh("resp_stale"); ok {
		t.Fatal("local previous-response binding was not deleted")
	}
}
