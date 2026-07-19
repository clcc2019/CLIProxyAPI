package auth

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
// When oauth-refresh.enabled is false or unset, provider-level default
// auto-refresh registrations can still start the loop for those providers.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) bool {
	if interval <= 0 {
		interval = refreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}
	if !m.autoRefreshEnabled() {
		return false
	}

	ctx, cancelCtx := context.WithCancel(parent)
	workers := refreshMaxConcurrency
	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
		workers = cfg.AuthAutoRefreshWorkers
	}
	loop := newAuthAutoRefreshLoop(m, interval, workers)

	m.mu.Lock()
	m.refreshCancel = cancelCtx
	m.refreshLoop = loop
	m.refreshSemaphore = make(chan struct{}, workers)
	m.mu.Unlock()

	initialRebuildAt := time.Now()
	if !m.refreshOnStartupEnabled() {
		initialRebuildAt = initialRebuildAt.Add(interval)
	}
	loop.rebuild(initialRebuildAt)
	go loop.run(ctx)
	return true
}

// StopAutoRefresh cancels the background refresh loop, if running.
// It also stops the selector if it implements StoppableSelector.
func (m *Manager) StopAutoRefresh() {
	m.mu.Lock()
	cancel := m.refreshCancel
	mainSelector := m.selector
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.stopPersistLoop()
	m.stopProxyPoolReconcileLoop()
	// Stop selector if it implements StoppableSelector (e.g., SessionAffinitySelector)
	if stoppable, ok := mainSelector.(StoppableSelector); ok {
		stoppable.Stop()
	}
}

func (m *Manager) checkRefreshes(ctx context.Context) {
	// log.Debugf("checking refreshes")
	now := time.Now()
	snapshot := m.snapshotAuths()
	dueIDs := make([]string, 0, len(snapshot))
	for _, a := range snapshot {
		typ, _ := a.AccountInfo()
		if typ != "api_key" {
			if !m.authAutoRefreshEnabled(a) {
				continue
			}
			if !m.shouldRefresh(a, now) {
				continue
			}
			dueIDs = append(dueIDs, a.ID)
		}
	}
	if len(dueIDs) == 0 {
		m.mu.Lock()
		m.refreshBatchCursor = 0
		m.mu.Unlock()
		return
	}
	sort.Strings(dueIDs)
	limit := m.refreshBatchSize()
	selectedIDs := dueIDs
	if limit > 0 && len(dueIDs) > limit {
		m.mu.Lock()
		start := 0
		if len(dueIDs) > 0 {
			start = m.refreshBatchCursor % len(dueIDs)
		}
		selectedIDs = make([]string, 0, limit)
		for i := 0; i < limit; i++ {
			idx := (start + i) % len(dueIDs)
			selectedIDs = append(selectedIDs, dueIDs[idx])
		}
		m.refreshBatchCursor = (start + limit) % len(dueIDs)
		m.mu.Unlock()
	} else {
		m.mu.Lock()
		m.refreshBatchCursor = 0
		m.mu.Unlock()
	}
	for _, id := range selectedIDs {
		auth, ok := m.GetByID(id)
		if !ok || auth == nil {
			continue
		}
		typ, _ := auth.AccountInfo()
		log.Debugf("checking refresh for %s, %s, %s", auth.Provider, auth.ID, typ)
		if exec := m.executorFor(auth.Provider); exec == nil {
			continue
		}
		if !m.markRefreshPending(auth.ID, now) {
			continue
		}
		go m.refreshAuthWithLimit(ctx, auth.ID)
	}
}

func (m *Manager) refreshAuthWithLimit(ctx context.Context, id string) {
	m.refreshAuthShared(ctx, id, true)
}

func (m *Manager) queueRefreshReschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.queueReschedule(authID)
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if authRefreshSuppressed(a) {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authRefreshSuppressed(auth *Auth) bool {
	return auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth)
}

func authPreferredInterval(a *Auth) time.Duration {
	if d := authConfiguredInterval(a); d > 0 {
		return d
	}
	if a == nil {
		return 0
	}
	return ProviderDefaultRefreshInterval(a.Provider)
}

func authConfiguredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func applyDefaultRefreshInterval(auth *Auth) {
	if auth == nil || !ProviderDefaultAutoRefresh(auth.Provider) || authConfiguredInterval(auth) > 0 {
		return
	}
	interval := ProviderDefaultRefreshInterval(auth.Provider)
	if interval <= 0 {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	seconds := int(interval / time.Second)
	if seconds <= 0 {
		return
	}
	auth.Metadata["refresh_interval_seconds"] = seconds
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		m.mu.Unlock()
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		m.mu.Unlock()
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	m.mu.Unlock()

	m.queueRefreshReschedule(id)
	return true
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	if _, err := m.refreshAuthShared(ctx, id, false); err != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", id).Warnf("auth refresh failed: %v", err)
	}
}

type managerRefreshOutcome struct {
	auth *Auth
	err  error
}
