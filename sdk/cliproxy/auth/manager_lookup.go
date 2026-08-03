package auth

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// ListByProvider returns cloned auth entries matching provider. Filtering is
// performed before cloning so callers that only need one provider avoid
// copying token-bearing metadata for every auth managed by the process.
func (m *Manager) ListByProvider(provider string) []*Auth {
	if m == nil {
		return nil
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var list []*Auth
	for _, auth := range m.auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		list = append(list, auth.Clone())
	}
	return list
}

// ListManagementSummary returns lightweight auth snapshots for management list
// views. It intentionally avoids copying full token-bearing metadata for every
// credential while preserving the fields needed for filtering, sorting, and cards.
func (m *Manager) ListManagementSummary() []*Auth {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.CloneForManagementSummary())
	}
	return list
}

// ListManagementSummaryWithoutRecentRequests returns the same lightweight
// management snapshots without copying each auth's fixed-size request ring.
// Paginated callers can clone only the candidates that land on the requested
// page when they need to expose recent request buckets.
func (m *Manager) ListManagementSummaryWithoutRecentRequests() []*Auth {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.cloneForManagementSummary(false))
	}
	return list
}

// ListManagementSummaryByIDs returns lightweight management snapshots with
// recent request rings for only the requested auth IDs.
func (m *Manager) ListManagementSummaryByIDs(ids []string) []*Auth {
	if m == nil || len(ids) == 0 {
		return []*Auth{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if auth := m.auths[id]; auth != nil {
			list = append(list, auth.CloneForManagementSummary())
		}
	}
	return list
}

// ListByIDs returns defensive full clones for the requested auth IDs. It is
// used by management filters after a lightweight summary pass so unrelated
// credentials do not have their token-bearing metadata copied.
func (m *Manager) ListByIDs(ids []string) []*Auth {
	if m == nil || len(ids) == 0 {
		return []*Auth{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if auth := m.auths[id]; auth != nil {
			list = append(list, auth.Clone())
		}
	}
	return list
}

// GetByFileName returns a defensive copy of the first auth whose FileName
// exactly matches name. Filtering under the manager lock avoids cloning every
// token-bearing auth merely to locate one management target.
func (m *Manager) GetByFileName(name string) (*Auth, bool) {
	if m == nil {
		return nil, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth != nil && auth.FileName == name {
			return auth.Clone(), true
		}
	}
	return nil, false
}

// GetByName performs the relaxed lookup used by management endpoints: an
// exact ID lookup first, followed by case-insensitive ID, file-name, and base-
// name matching. Only the matching auth is cloned.
func (m *Manager) GetByName(name string) (*Auth, bool) {
	if m == nil {
		return nil, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	lookupBase := strings.TrimSpace(filepath.Base(name))
	m.mu.RLock()
	defer m.mu.RUnlock()
	if auth := m.auths[name]; auth != nil {
		return auth.Clone(), true
	}
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		for _, candidate := range []string{auth.ID, auth.FileName, filepath.Base(auth.FileName)} {
			candidate = strings.TrimSpace(candidate)
			if candidate != "" && (strings.EqualFold(candidate, name) || strings.EqualFold(candidate, lookupBase)) {
				return auth.Clone(), true
			}
		}
	}
	return nil, false
}

// GetByIndex returns a defensive copy of the auth with the stable management
// index. Registered and updated auths already have their index assigned, so
// the common path compares a short string without cloning unrelated entries.
func (m *Manager) GetByIndex(index string) (*Auth, bool) {
	if m == nil {
		return nil, false
	}
	index = strings.TrimSpace(index)
	if index == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		candidate := strings.TrimSpace(auth.Index)
		if candidate == "" {
			// Preserve compatibility with legacy/injected manager entries without
			// mutating shared state while holding a read lock.
			candidate = auth.Clone().EnsureIndex()
		}
		if candidate == index {
			return auth.Clone(), true
		}
	}
	return nil, false
}

// AuthIndexesByID returns the stable management index for each auth without
// cloning token-bearing metadata. The returned map is owned by the caller.
func (m *Manager) AuthIndexesByID() map[string]string {
	indexes := make(map[string]string)
	if m == nil {
		return indexes
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	indexes = make(map[string]string, len(m.auths))
	for id, auth := range m.auths {
		id = strings.TrimSpace(id)
		if id == "" || auth == nil {
			continue
		}
		index := strings.TrimSpace(auth.Index)
		if index == "" {
			index = auth.Clone().EnsureIndex()
		}
		if index != "" {
			indexes[id] = index
		}
	}
	return indexes
}

// AnyAvailableAuthForModel reports whether any matching auth is currently usable.
func (m *Manager) AnyAvailableAuthForModel(providers []string, model string, predicate func(*Auth) bool) bool {
	if m == nil || predicate == nil {
		return false
	}

	providerSet := make(map[string]struct{}, len(providers))
	for i := 0; i < len(providers); i++ {
		providerKey := strings.TrimSpace(strings.ToLower(providers[i]))
		if providerKey == "" {
			continue
		}
		providerSet[providerKey] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	registryRef := registry.GetGlobalRegistry()
	now := time.Now()

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		if !AuthAvailableForModel(auth, model, now) {
			continue
		}
		if predicate(auth) {
			return true
		}
	}

	return false
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// GetExecutionSessionAuthByID retrieves a Home runtime auth scoped to an execution session.
func (m *Manager) GetExecutionSessionAuthByID(sessionID string, authID string) (*Auth, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	if auth == nil {
		return nil, false
	}
	return auth.Clone(), true
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	m.mu.RUnlock()

	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.Lock()
	if sessionID == CloseAllExecutionSessionsID {
		m.clearHomeRuntimeAuthsLocked()
	} else {
		m.clearHomeRuntimeAuthsForSessionLocked(sessionID)
	}
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.Unlock()

	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

// ResetExecutionSession asks all registered executors to invalidate the
// supplied execution session so the next request starts fresh.
func (m *Manager) ResetExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.RLock()
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.RUnlock()

	for i := range executors {
		if resetter, ok := executors[i].(ExecutionSessionResetter); ok && resetter != nil {
			resetter.ResetExecutionSession(sessionID)
		}
	}
}
