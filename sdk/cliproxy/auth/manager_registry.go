package auth

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	applyDefaultRefreshInterval(auth)
	auth.EnsureIndex()
	m.applyProxyPoolLease(ctx, auth)
	m.mu.Lock()
	if m.shouldSuppressRemovedAuthUpsertLocked(auth) {
		m.mu.Unlock()
		return nil, nil
	}
	existing := m.auths[auth.ID]
	continuityChanged := authCredentialPrincipalChanged(existing, auth)
	if continuityChanged {
		m.invalidateAuthAffinityLocked(auth.ID)
		clearAuthRuntimeState(auth)
		if m.runtimeStates != nil {
			delete(m.runtimeStates, auth.ID)
		}
	} else {
		auth.ApplyRuntimeStateFromMetadata()
		m.applyPersistedRuntimeStateLocked(auth)
	}
	if existing != nil && !continuityChanged {
		// Register is normally used for initial loading, but a caller can also
		// upsert an existing auth. Do not let a stale file snapshot overwrite a
		// quota cooldown learned while this manager is running.
		preserveRuntimeState(existing, auth)
	}
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	if continuityChanged {
		m.resetAuthContinuityLocked(ctx, auth.ID)
	}
	m.mu.Unlock()
	if continuityChanged {
		m.invalidatePreviousResponseStoreForAuth(ctx, auth.ID)
	}
	m.clearRouteAwareCaches()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone.CloneForScheduler())
	}
	m.queueRefreshReschedule(auth.ID)
	_ = m.persist(ctx, auth)
	if errRuntime := m.persistRuntimeState(ctx, auth); errRuntime != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth runtime state: %v", errRuntime)
	}
	m.reconcileProxyPoolLeasesAfterAuthChange(ctx)
	if current, okCurrent := m.GetByID(auth.ID); okCurrent && current != nil {
		auth = current
	}
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	applyDefaultRefreshInterval(auth)
	m.applyProxyPoolLease(ctx, auth)
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	if !executionAuthPrincipalMatches(ctx, existing) {
		var existingClone *Auth
		if existing != nil {
			existingClone = existing.Clone()
		}
		m.mu.Unlock()
		return existingClone, nil
	}
	if isRefreshUpdate(ctx) && (!ok || authRefreshSuppressed(existing)) {
		var existingClone *Auth
		if existing != nil {
			existingClone = existing.Clone()
		}
		m.mu.Unlock()
		return existingClone, nil
	}
	if !ok && m.shouldSuppressRemovedAuthUpsertLocked(auth) {
		m.mu.Unlock()
		return nil, nil
	}
	if ok && existing != nil && isExecutionAuthProfileUpdate(ctx) {
		var changed bool
		auth, changed = mergeCodexExecutionAuthProfile(existing, auth)
		if !changed {
			existingClone := existing.Clone()
			m.mu.Unlock()
			return existingClone, nil
		}
	}
	continuityChanged := ok && existing != nil && authCredentialPrincipalChanged(existing, auth)
	if continuityChanged && isRefreshUpdate(ctx) {
		existingClone := existing.Clone()
		m.mu.Unlock()
		return existingClone, nil
	}
	if ok && existing != nil && !continuityChanged {
		if isRefreshUpdate(ctx) {
			preserveEditableAuthFileOptions(existing, auth)
		}
		if !auth.indexAssigned && auth.Index == "" {
			auth.Index = existing.Index
			auth.indexAssigned = existing.indexAssigned
		}
		preserveRuntimeState(existing, auth)
		if !existing.IsDisabled() && !auth.IsDisabled() {
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
		}
	} else if !continuityChanged {
		auth.ApplyRuntimeStateFromMetadata()
		m.applyPersistedRuntimeStateLocked(auth)
	}
	if continuityChanged {
		m.invalidateAuthAffinityLocked(auth.ID)
		clearAuthRuntimeState(auth)
		if m.runtimeStates != nil {
			delete(m.runtimeStates, auth.ID)
		}
	}
	auth.EnsureIndex()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	if continuityChanged {
		m.resetAuthContinuityLocked(ctx, auth.ID)
	}
	m.mu.Unlock()
	if continuityChanged {
		m.invalidatePreviousResponseStoreForAuth(ctx, auth.ID)
	}
	m.clearRouteAwareCaches()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone.CloneForScheduler())
	}
	m.queueRefreshReschedule(auth.ID)
	if m.shouldReleaseProxyLeaseForAuth(authClone) {
		m.releaseProxyLease(ctx, auth.ID)
		clearProxyPoolLease(auth)
	}
	persistErr := m.persist(ctx, auth)
	if errRuntime := m.persistRuntimeState(ctx, auth); errRuntime != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth runtime state: %v", errRuntime)
	}
	// Refresh updates normally skip reconciliation because token-only changes do
	// not affect proxy eligibility. A Codex plan transition to Free is different:
	// applyProxyPoolLease released its scarce lease above, so wake reconciliation
	// after the Free snapshot is installed and let the next paid auth take it.
	if !isRefreshUpdate(ctx) || isFreeCodexAuth(authClone) {
		m.reconcileProxyPoolLeasesAfterAuthChange(ctx)
		if current, okCurrent := m.GetByID(auth.ID); okCurrent && current != nil {
			auth = current
		}
	}
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	if persistErr != nil {
		return auth.Clone(), persistErr
	}
	return auth.Clone(), nil
}

// Remove deletes an auth entry from the in-memory manager state. The backing
// credential store is intentionally not touched here; callers that delete an
// auth file or remote credential record must do that explicitly before or after
// removing the runtime entry.
func (m *Manager) Remove(ctx context.Context, id string) (*Auth, error) {
	if m == nil {
		return nil, nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var removed *Auth
	var loop *authAutoRefreshLoop
	var runtimeStore RuntimeStateStore
	now := time.Now()
	m.mu.Lock()
	if existing := m.auths[id]; existing != nil {
		removed = existing.Clone()
		delete(m.auths, id)
		if tombstone, ok := authRemovalTombstoneFor(removed, now); ok {
			if m.removedAuths == nil {
				m.removedAuths = make(map[string]authRemovalTombstone)
			}
			m.removedAuths[id] = tombstone
		}
	}
	if m.runtimeStates != nil {
		delete(m.runtimeStates, id)
	}
	poolAuthID := strings.ToLower(id)
	for key := range m.modelPoolOffsets {
		if authIDFromModelPoolOffsetKey(key) == poolAuthID {
			delete(m.modelPoolOffsets, key)
		}
	}
	loop = m.refreshLoop
	runtimeStore = m.runtimeStateStore
	m.mu.Unlock()
	m.resetAuthContinuity(ctx, id)

	if authProxyPoolAssigned(removed) {
		m.releaseProxyLease(ctx, id)
	}
	m.discardPendingPersistAuthID(id)
	if loop != nil {
		loop.remove(id)
	}
	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	m.clearRouteAwareCaches()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	registry.GetGlobalRegistry().UnregisterClient(id)

	var runtimeErr error
	if runtimeStore != nil && !shouldSkipPersist(ctx) {
		if err := runtimeStore.Delete(ctx, id); err != nil {
			runtimeErr = err
		}
	}

	if removed != nil {
		notification := removed.Clone()
		notification.Disabled = true
		notification.Status = StatusDisabled
		if strings.TrimSpace(notification.StatusMessage) == "" {
			notification.StatusMessage = "removed"
		}
		notification.UpdatedAt = now
		m.hook.OnAuthUpdated(ctx, notification)
	}
	return removed, runtimeErr
}

func (m *Manager) resetAuthContinuity(ctx context.Context, authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	m.invalidateSessionAffinityForAuth(authID)
	m.invalidatePreviousResponsesForAuth(ctx, authID)
	m.mu.RLock()
	m.resetAuthContinuityLocked(ctx, authID)
	m.mu.RUnlock()
}

func (m *Manager) invalidateAuthAffinityLocked(authID string) {
	if m.previousResponseAuths != nil {
		m.previousResponseAuths.InvalidateAuth(authID)
	}
	m.invalidateSessionAffinityForAuthLocked(authID, m.selector)
}

// resetAuthContinuityLocked blocks manager callbacks from old in-flight
// requests while executor state is cleared. Caller must hold m.mu.
func (m *Manager) resetAuthContinuityLocked(ctx context.Context, authID string) {
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, executor := range m.executors {
		executors = append(executors, executor)
	}
	for _, executor := range executors {
		if resetter, ok := executor.(AuthContinuityResetter); ok && resetter != nil {
			resetter.ResetAuthContinuity(authID)
		}
	}
}

func authCredentialPrincipalChanged(existing, replacement *Auth) bool {
	if existing == nil || replacement == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(existing.Provider), strings.TrimSpace(replacement.Provider)) {
		return true
	}
	oldKind, oldPrincipal := authCredentialPrincipal(existing)
	newKind, newPrincipal := authCredentialPrincipal(replacement)
	return oldKind != "" && newKind != "" && (oldKind != newKind || oldPrincipal != newPrincipal)
}

func authCredentialPrincipal(auth *Auth) (string, string) {
	if auth == nil {
		return "", ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "codex" {
		kind := CodexAuthKind(auth)
		if accountID := codexCredentialIdentityValue(auth, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
			return provider + ":account", accountID
		}
		if kind == CodexAuthKindAgentIdentity {
			if runtimeID := codexCredentialIdentityValue(auth, "agent_runtime_id", "agentRuntimeId", "agentRuntimeID"); runtimeID != "" {
				return provider + ":" + kind, runtimeID
			}
		}
		if kind == CodexAuthKindOAuth {
			if email := codexCredentialIdentityValue(auth, "email"); email != "" {
				return provider + ":" + kind, strings.ToLower(email)
			}
			return "", ""
		}
	}
	if auth.Attributes != nil {
		if apiKey := strings.TrimSpace(auth.Attributes["api_key"]); apiKey != "" {
			return provider + ":api_key", apiKey
		}
	}
	if auth.Metadata != nil {
		for _, key := range []string{"email", "username", "user_id", "userId"} {
			if principal := strings.TrimSpace(metadataString(auth.Metadata, key)); principal != "" {
				return provider + ":oauth", strings.ToLower(principal)
			}
		}
	}
	return "", ""
}

// CredentialPrincipal returns the stable credential subject used to isolate
// executor state when an auth ID can be rebound to another account.
func CredentialPrincipal(auth *Auth) (string, string) {
	return authCredentialPrincipal(auth)
}

func clearAuthRuntimeState(auth *Auth) {
	if auth == nil {
		return
	}
	auth.Success = 0
	auth.Failed = 0
	auth.recentRequests = nil
	auth.Unavailable = false
	auth.Quota = QuotaState{}
	auth.RateLimits = nil
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.ModelStates = nil
	if auth.Status != StatusDisabled {
		auth.Status = StatusActive
		auth.StatusMessage = ""
	}
	if auth.Metadata != nil {
		delete(auth.Metadata, runtimeStateMetadataKey)
	}
}

func authIDFromModelPoolOffsetKey(key string) string {
	if key == "" {
		return ""
	}
	if idx := strings.IndexByte(key, '|'); idx >= 0 {
		return key[:idx]
	}
	return key
}

func (m *Manager) shouldSuppressRemovedAuthUpsertLocked(auth *Auth) bool {
	if m == nil || auth == nil || strings.TrimSpace(auth.ID) == "" || len(m.removedAuths) == 0 {
		return false
	}
	tombstone, ok := m.removedAuths[auth.ID]
	if !ok {
		return false
	}
	if authRestoresRemovedFile(auth, tombstone) {
		delete(m.removedAuths, auth.ID)
		return false
	}
	return true
}

func authRestoresRemovedFile(auth *Auth, tombstone authRemovalTombstone) bool {
	path := authFilePathForRemoval(auth)
	if path == "" {
		path = strings.TrimSpace(tombstone.Path)
	}
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if tombstone.RemovedAt.IsZero() {
		return true
	}
	if info.ModTime().After(tombstone.RemovedAt) {
		return true
	}
	return false
}

func authRemovalTombstoneFor(auth *Auth, now time.Time) (authRemovalTombstone, bool) {
	if auth == nil || auth.ID == "" || authRuntimeOnly(auth) {
		return authRemovalTombstone{}, false
	}
	path := authFilePathForRemoval(auth)
	if path == "" {
		return authRemovalTombstone{}, false
	}
	return authRemovalTombstone{Path: path, RemovedAt: now}, true
}

func authFilePathForRemoval(auth *Auth) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["path"])
}

func authRuntimeOnly(auth *Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.RLock()
	if m.store == nil {
		m.mu.RUnlock()
		return nil
	}
	store := m.store
	m.mu.RUnlock()

	items, err := store.List(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.auths = make(map[string]*Auth, len(items))
	m.removedAuths = make(map[string]authRemovalTombstone)
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		auth.EnsureIndex()
		applyDefaultRefreshInterval(auth)
		auth.ApplyRuntimeStateFromMetadata()
		m.applyPersistedRuntimeStateLocked(auth)
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	return nil
}
