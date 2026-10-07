package auth

import (
	"context"
	"strings"
	"sync"
)

// persist saves a snapshot for callers that do not hold a mutation gate. It
// resolves the latest manager snapshot after taking the per-auth write lock so
// deferred persistence does not write an older token after a synchronous update.
func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m == nil || auth == nil {
		return nil
	}
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return nil
	}

	authID := strings.TrimSpace(auth.ID)
	lock := m.persistLockForAuth(authID)
	lock.Lock()
	defer lock.Unlock()

	persistAuth := auth.Clone()
	if authID != "" {
		m.mu.RLock()
		if current := m.auths[authID]; current != nil {
			persistAuth = current.Clone()
		}
		m.mu.RUnlock()
	}
	return m.saveAuthSnapshot(ctx, store, persistAuth)
}

// persistMutation saves the exact mutation snapshot while the caller holds the
// per-auth mutation gate. Store.Save runs without m.mu; only fields returned or
// enriched by the store are merged back. Runtime changes made during storage
// I/O therefore survive even when Store.Save returns an error.
func (m *Manager) persistMutation(ctx context.Context, auth *Auth) error {
	if m == nil || auth == nil {
		return nil
	}
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return nil
	}

	authID := strings.TrimSpace(auth.ID)
	lock := m.persistLockForAuth(authID)
	lock.Lock()
	defer lock.Unlock()

	persistAuth := auth.Clone()
	if !shouldPersistAuth(ctx, persistAuth) {
		return nil
	}
	stripProxyPoolLeaseForPersist(persistAuth)
	persistAuth.SetRuntimeStateMetadata()
	beforeStore := persistAuth.Clone()
	_, errSave := store.Save(ctx, persistAuth)

	// The mutation gate prevents Register/Update/Remove for this ID from
	// changing the published object while this merge is being committed. Other
	// runtime writers may still have changed fields, so merge per-field deltas.
	m.mu.Lock()
	if current := m.auths[authID]; current != nil {
		mergeAuthSaveDelta(current, beforeStore, persistAuth, true)
	}
	m.mu.Unlock()
	return errSave
}

func (m *Manager) saveAuthSnapshot(ctx context.Context, store Store, auth *Auth) error {
	if store == nil || !shouldPersistAuth(ctx, auth) {
		return nil
	}
	stripProxyPoolLeaseForPersist(auth)
	auth.SetRuntimeStateMetadata()
	_, err := store.Save(ctx, auth)
	return err
}

func shouldPersistAuth(ctx context.Context, auth *Auth) bool {
	if auth == nil || shouldSkipPersist(ctx) {
		return false
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return false
		}
	}
	// Skip persistence when metadata is absent (e.g. runtime-only auths).
	return auth.Metadata != nil
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
