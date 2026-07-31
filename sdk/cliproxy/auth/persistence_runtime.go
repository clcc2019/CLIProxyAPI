package auth

import (
	"context"
	"strings"
	"time"
)

func (m *Manager) persistRuntimeState(ctx context.Context, auth *Auth) error {
	if m == nil || m.runtimeStateStore == nil || auth == nil || auth.ID == "" {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	state := auth.RuntimeStateSnapshot()
	if err := m.runtimeStateStore.Save(ctx, auth.ID, state); err != nil {
		return err
	}
	m.mu.Lock()
	if m.runtimeStates != nil {
		m.runtimeStates[auth.ID] = state
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) applyPersistedRuntimeStateLocked(auth *Auth) {
	if m == nil || auth == nil || auth.ID == "" || len(m.runtimeStates) == 0 {
		return
	}
	state, ok := m.runtimeStates[auth.ID]
	if !ok {
		return
	}
	auth.ApplyRuntimeState(state)
}

func preserveRuntimeState(existing, auth *Auth) {
	if existing == nil || auth == nil {
		return
	}
	mu := existing.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()
	auth.Success = existing.Success
	auth.Failed = existing.Failed
	auth.recentRequests = cloneRecentRequestRing(existing.recentRequests)
	// Request-time auth preparation works from an execution snapshot and calls
	// Update when it has filled in metadata. That snapshot can predate a quota
	// cooldown written by a concurrent usage probe. Preserve an active auth-wide
	// quota block so the metadata update cannot revive the credential before its
	// known reset time.
	if !auth.IsDisabled() && authScopedQuotaCooldownActive(existing, time.Now()) {
		auth.Status = existing.Status
		auth.StatusMessage = existing.StatusMessage
		auth.Unavailable = existing.Unavailable
		auth.Quota = existing.Quota
		auth.LastError = cloneError(existing.LastError)
		auth.NextRetryAfter = existing.NextRetryAfter
		if existing.UpdatedAt.After(auth.UpdatedAt) {
			auth.UpdatedAt = existing.UpdatedAt
		}
	}
}

var refreshPreservedMetadataKeys = []string{
	"prefix",
	"proxy_url",
	"proxy-url",
	"proxyUrl",
	"priority",
	"note",
	"headers",
	"excluded_models",
	"excluded-models",
	"excludedModels",
	"disable_cooling",
	"disable-cooling",
	"disableCooling",
	"user_agent",
	"user-agent",
	"userAgent",
	"codex_client_profile_pinned",
	"originator",
	"Originator",
	"websockets",
	"websocket",
	"service_tier_passthrough",
	"service-tier-passthrough",
	"serviceTierPassthrough",
	"fast",
	"websocket_handshake_debug",
}

var refreshPreservedAttributeKeys = []string{
	"path",
	"source",
	"priority",
	AttributeWeight,
	"note",
	"proxy_url",
	"excluded_models",
	"excluded_models_hash",
	"websockets",
	"service_tier_passthrough",
	"user_agent",
	"user-agent",
	"userAgent",
	proxyPoolAssignedAttribute,
}

func preserveEditableAuthFileOptions(existing, auth *Auth) {
	if existing == nil || auth == nil {
		return
	}
	auth.Prefix = existing.Prefix
	auth.ProxyURL = existing.ProxyURL

	if auth.Metadata == nil && len(existing.Metadata) > 0 {
		auth.Metadata = make(map[string]any, len(existing.Metadata))
	}
	for _, key := range refreshPreservedMetadataKeys {
		if existing.Metadata != nil {
			if value, ok := existing.Metadata[key]; ok {
				auth.Metadata[key] = value
				continue
			}
		}
		if auth.Metadata != nil {
			delete(auth.Metadata, key)
		}
	}

	if auth.Attributes == nil && len(existing.Attributes) > 0 {
		auth.Attributes = make(map[string]string, len(existing.Attributes))
	}
	for _, key := range refreshPreservedAttributeKeys {
		if existing.Attributes != nil {
			if value, ok := existing.Attributes[key]; ok {
				auth.Attributes[key] = value
				continue
			}
		}
		if auth.Attributes != nil {
			delete(auth.Attributes, key)
		}
	}
	for key := range auth.Attributes {
		if strings.HasPrefix(key, "header:") {
			delete(auth.Attributes, key)
		}
	}
	if len(existing.Attributes) > 0 {
		for key, value := range existing.Attributes {
			if strings.HasPrefix(key, "header:") && strings.TrimSpace(value) != "" {
				auth.Attributes[key] = value
			}
		}
	}
	ApplyAuthFileOptionsFromMetadata(auth)
	ApplyCodexMetadataFromMetadata(auth)
	ApplyCustomHeadersFromMetadata(auth)
}

func (m *Manager) enqueuePersistAuthID(ctx context.Context, authID string) {
	if m == nil || shouldSkipPersist(ctx) {
		return
	}
	if !m.persistEnabled.Load() {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}

	m.persistMu.Lock()
	if m.persistIDs == nil {
		m.persistIDs = make(map[string]struct{}, 32)
	}
	m.persistIDs[authID] = struct{}{}
	if m.persistWake == nil || m.persistCancel == nil {
		m.startPersistLoopLocked()
	}
	wake := m.persistWake
	m.persistMu.Unlock()

	select {
	case wake <- struct{}{}:
	default:
	}
}

func (m *Manager) discardPendingPersistAuthID(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	m.persistMu.Lock()
	if m.persistIDs != nil {
		delete(m.persistIDs, authID)
	}
	m.persistMu.Unlock()
}

func (m *Manager) startPersistLoopLocked() {
	if m == nil {
		return
	}
	if m.persistWake != nil && m.persistCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	m.persistWake = wake
	m.persistCancel = cancel
	m.persistWG.Add(1)
	go m.runPersistLoop(ctx, wake)
}

func (m *Manager) stopPersistLoop() {
	if m == nil {
		return
	}

	m.persistMu.Lock()
	cancel := m.persistCancel
	m.persistCancel = nil
	m.persistWake = nil
	m.persistMu.Unlock()

	if cancel != nil {
		cancel()
		m.persistWG.Wait()
	}
}

func (m *Manager) runPersistLoop(ctx context.Context, wake <-chan struct{}) {
	defer m.persistWG.Done()

	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)

	resetTimer := func() {
		if timer == nil {
			timer = time.NewTimer(persistDebounceWindow)
			timerC = timer.C
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(persistDebounceWindow)
	}

	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		timerC = nil
	}

	for {
		select {
		case <-ctx.Done():
			stopTimer()
			m.flushPersistQueue()
			return
		case <-wake:
			resetTimer()
		case <-timerC:
			stopTimer()
			m.flushPersistQueue()
		}
	}
}

func (m *Manager) flushPersistQueue() {
	if m == nil {
		return
	}

	m.persistMu.Lock()
	if len(m.persistIDs) == 0 {
		m.persistMu.Unlock()
		return
	}
	ids := make([]string, 0, len(m.persistIDs))
	for id := range m.persistIDs {
		ids = append(ids, id)
	}
	clear(m.persistIDs)
	m.persistMu.Unlock()

	snapshots := make([]*Auth, 0, len(ids))
	m.mu.RLock()
	for _, id := range ids {
		auth := m.auths[id]
		if auth == nil {
			continue
		}
		snapshots = append(snapshots, auth.Clone())
	}
	m.mu.RUnlock()

	for _, snapshot := range snapshots {
		if err := m.persist(context.Background(), snapshot); err != nil {
			logEntryWithRequestID(context.Background()).
				WithField("auth_id", snapshot.ID).
				Warnf("deferred auth persist failed: %v", err)
		}
		if err := m.persistRuntimeState(context.Background(), snapshot); err != nil {
			logEntryWithRequestID(context.Background()).
				WithField("auth_id", snapshot.ID).
				Warnf("deferred auth runtime state persist failed: %v", err)
		}
	}
}
