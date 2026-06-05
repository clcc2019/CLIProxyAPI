package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

type removeCaptureStore struct {
	mu    sync.Mutex
	saved map[string]*Auth
}

func (s *removeCaptureStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *removeCaptureStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		s.saved = make(map[string]*Auth)
	}
	s.saved[auth.ID] = auth.Clone()
	return "", nil
}

func (s *removeCaptureStore) Delete(context.Context, string) error { return nil }

func (s *removeCaptureStore) savedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

func (s *removeCaptureStore) reset() {
	s.mu.Lock()
	s.saved = nil
	s.mu.Unlock()
}

func TestManagerRemoveSuppressesInFlightRefreshPersistence(t *testing.T) {
	store := &removeCaptureStore{}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &singleflightRefreshTestExecutor{
		provider: "oauth",
		started:  make(chan string, 1),
		release:  make(chan struct{}),
		mutate: func(auth *Auth) *Auth {
			updated := auth.Clone()
			updated.Metadata["access_token"] = "new-access"
			updated.Metadata["refresh_token"] = "new-refresh"
			updated.NextRefreshAfter = time.Now().Add(time.Minute)
			return updated
		},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "oauth-delete-race.json",
		Provider: "oauth",
		Status:   StatusActive,
		Attributes: map[string]string{
			"path": "/tmp/oauth-delete-race.json",
		},
		Metadata: map[string]any{
			"type":          "oauth",
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	store.reset()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = manager.RefreshAuth(context.Background(), auth)
	}()
	waitForRefreshStart(t, executor.started, auth.ID)

	if removed, err := manager.Remove(context.Background(), auth.ID); err != nil || removed == nil {
		t.Fatalf("Remove() removed=%#v err=%v, want removed auth without error", removed, err)
	}
	close(executor.release)
	<-done
	manager.flushPersistQueue()

	if got := store.savedCount(); got != 0 {
		t.Fatalf("saved auth count after in-flight refresh = %d, want 0", got)
	}
	if current, ok := manager.GetByID(auth.ID); ok || current != nil {
		t.Fatalf("expected auth to stay removed, got auth=%#v ok=%v", current, ok)
	}
}
