package auth

import (
	"context"
	"strings"
	"sync"
)

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	authID := strings.TrimSpace(auth.ID)
	lock := m.persistLockForAuth(authID)
	lock.Lock()
	defer lock.Unlock()

	// Always resolve the snapshot after acquiring the per-auth write lock. A
	// deferred persist may have captured an old token before a synchronous OAuth
	// refresh; resolving here prevents that stale snapshot from being written
	// after the newly rotated refresh token.
	persistAuth := auth.Clone()
	if authID != "" {
		m.mu.RLock()
		if current := m.auths[authID]; current != nil {
			persistAuth = current.Clone()
		}
		m.mu.RUnlock()
	}
	if persistAuth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(persistAuth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if persistAuth.Metadata == nil {
		return nil
	}
	stripProxyPoolLeaseForPersist(persistAuth)
	persistAuth.SetRuntimeStateMetadata()
	_, err := m.store.Save(ctx, persistAuth)
	return err
}

func (m *Manager) persistLockForAuth(authID string) *sync.Mutex {
	if m == nil {
		return &sync.Mutex{}
	}
	key := strings.TrimSpace(authID)
	if key == "" {
		key = "<anonymous>"
	}
	lock := &sync.Mutex{}
	actual, _ := m.persistLocks.LoadOrStore(key, lock)
	return actual.(*sync.Mutex)
}
