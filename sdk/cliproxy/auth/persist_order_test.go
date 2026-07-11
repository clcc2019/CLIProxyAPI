package auth

import (
	"context"
	"sync"
	"testing"
)

type orderedTokenPersistStore struct {
	mu           sync.Mutex
	firstStarted chan struct{}
	releaseFirst chan struct{}
	saves        []string
	active       int
	maxActive    int
}

func (s *orderedTokenPersistStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *orderedTokenPersistStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	call := len(s.saves) + s.active
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()

	if call == 0 {
		close(s.firstStarted)
		<-s.releaseFirst
	}

	token := ""
	if auth != nil && auth.Metadata != nil {
		token, _ = auth.Metadata["refresh_token"].(string)
	}
	s.mu.Lock()
	s.saves = append(s.saves, token)
	s.active--
	s.mu.Unlock()
	return "", nil
}

func (s *orderedTokenPersistStore) Delete(context.Context, string) error { return nil }

func TestManagerPersistDoesNotWriteStaleTokenAfterLatestSnapshot(t *testing.T) {
	store := &orderedTokenPersistStore{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	manager := NewManager(store, nil, nil)
	oldAuth := &Auth{
		ID:       "persist-token-order",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), oldAuth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	newAuth := oldAuth.Clone()
	newAuth.Metadata["access_token"] = "new-access"
	newAuth.Metadata["refresh_token"] = "new-refresh"
	if _, err := manager.Update(WithSkipPersist(context.Background()), newAuth); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- manager.persist(context.Background(), newAuth) }()
	<-store.firstStarted

	staleDone := make(chan error, 1)
	go func() { staleDone <- manager.persist(context.Background(), oldAuth) }()
	close(store.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("latest persist error = %v", err)
	}
	if err := <-staleDone; err != nil {
		t.Fatalf("stale persist error = %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.maxActive != 1 {
		t.Fatalf("concurrent Store.Save calls = %d, want 1", store.maxActive)
	}
	if len(store.saves) != 2 {
		t.Fatalf("save count = %d, want 2", len(store.saves))
	}
	for index, token := range store.saves {
		if token != "new-refresh" {
			t.Fatalf("save[%d] refresh_token = %q, want latest token", index, token)
		}
	}
}
