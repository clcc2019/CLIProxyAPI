package auth

import (
	"context"
	"strings"
	"time"
)

func (m *Manager) handleExecutionRefreshUpdate(ctx context.Context, updated *Auth) {
	if m == nil || updated == nil || updated.ID == "" {
		return
	}
	updateCtx := context.Background()
	if ctx != nil {
		updateCtx = context.WithoutCancel(ctx)
	}
	updateCtx = withExecutionAuthPrincipalSnapshot(updateCtx, ctx)
	if _, err := m.Update(withRefreshUpdate(updateCtx), updated); err != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", updated.ID).Warnf("failed to persist refreshed auth: %v", err)
	}
}

func (m *Manager) handleExecutionAuthUpdate(ctx context.Context, updated *Auth) {
	if m == nil || updated == nil || updated.ID == "" {
		return
	}
	updateCtx := context.Background()
	if ctx != nil {
		updateCtx = context.WithoutCancel(ctx)
	}
	updateCtx = withExecutionAuthPrincipalSnapshot(updateCtx, ctx)
	if isExecutionAuthProfileUpdate(ctx) {
		updateCtx = withExecutionAuthProfileUpdate(updateCtx)
	}
	if _, err := m.Update(updateCtx, updated); err != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", updated.ID).Warnf("failed to persist auth update: %v", err)
	}
}

func (m *Manager) handleExecutionRateLimitUpdate(ctx context.Context, authID string, snapshots []RateLimitSnapshot) {
	if m == nil {
		return
	}
	m.UpdateRateLimits(ctx, authID, snapshots)
}

func (m *Manager) executionAuthPrincipalMatches(ctx context.Context, candidate *Auth) bool {
	if m == nil || candidate == nil || strings.TrimSpace(candidate.ID) == "" {
		return false
	}
	m.mu.RLock()
	current := m.auths[candidate.ID]
	matches := current != nil && executionAuthPrincipalMatches(ctx, current) && !authCredentialPrincipalChanged(current, candidate)
	m.mu.RUnlock()
	return matches
}

func (m *Manager) executionAuthPrincipalMatchesID(ctx context.Context, authID string) bool {
	if m == nil || strings.TrimSpace(authID) == "" {
		return false
	}
	m.mu.RLock()
	matches := executionAuthPrincipalMatches(ctx, m.auths[authID])
	m.mu.RUnlock()
	return matches
}

func (m *Manager) UpdateRateLimits(ctx context.Context, authID string, snapshots []RateLimitSnapshot) {
	if m == nil || strings.TrimSpace(authID) == "" || len(snapshots) == 0 {
		return
	}
	now := time.Now().UTC()
	persistAuthID := ""
	var schedulerSnapshot *Auth

	m.mu.Lock()
	if auth, ok := m.auths[authID]; ok && auth != nil && executionAuthPrincipalMatches(ctx, auth) {
		changed := mergeRateLimitSnapshots(auth, snapshots, now)
		if changed {
			auth.UpdatedAt = now
			persistAuthID = auth.ID
			schedulerSnapshot = auth.CloneForScheduler()
		}
	}
	m.mu.Unlock()

	if persistAuthID != "" {
		m.enqueuePersistAuthID(ctx, persistAuthID)
	}
	if m.scheduler != nil && schedulerSnapshot != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
}

func mergeRateLimitSnapshots(auth *Auth, snapshots []RateLimitSnapshot, now time.Time) bool {
	if auth == nil || len(snapshots) == 0 {
		return false
	}
	changed := false
	for _, snapshot := range snapshots {
		if !rateLimitSnapshotHasData(snapshot) {
			continue
		}
		key := normalizeRateLimitID(snapshot.LimitID)
		if key == "" {
			key = "codex"
		}
		snapshot.LimitID = key
		if snapshot.UpdatedAt.IsZero() {
			snapshot.UpdatedAt = now
		}
		snapshot = cloneRateLimitSnapshot(snapshot)
		if auth.RateLimits == nil {
			auth.RateLimits = make(map[string]RateLimitSnapshot, len(snapshots))
		}
		current, exists := auth.RateLimits[key]
		if exists {
			snapshot = mergeRateLimitSnapshot(current, snapshot)
		}
		if exists && rateLimitSnapshotsEqual(current, snapshot) {
			continue
		}
		auth.RateLimits[key] = snapshot
		changed = true
	}
	return changed
}

// mergeRateLimitSnapshot keeps fields from the last complete upstream
// response when a later response is partial. Codex may omit credits or one of
// the windows in an otherwise successful usage response; replacing the whole
// snapshot would make the management list lose previously observed quota.
func mergeRateLimitSnapshot(current, incoming RateLimitSnapshot) RateLimitSnapshot {
	merged := cloneRateLimitSnapshot(incoming)
	if merged.LimitID == "" {
		merged.LimitID = current.LimitID
	}
	if merged.LimitName == "" {
		merged.LimitName = current.LimitName
	}
	if merged.PlanType == "" {
		merged.PlanType = current.PlanType
	}
	if merged.RateLimitReachedType == "" {
		merged.RateLimitReachedType = current.RateLimitReachedType
	}
	merged.Primary = mergeRateLimitWindow(current.Primary, incoming.Primary)
	merged.Secondary = mergeRateLimitWindow(current.Secondary, incoming.Secondary)
	if incoming.Credits == nil {
		merged.Credits = cloneRateLimitSnapshot(current).Credits
	}
	return merged
}

func mergeRateLimitWindow(current, incoming *RateLimitWindow) *RateLimitWindow {
	if incoming == nil {
		if current == nil {
			return nil
		}
		return cloneRateLimitWindow(current)
	}
	merged := cloneRateLimitWindow(incoming)
	if current == nil {
		return merged
	}
	if merged.WindowMinutes == nil && current.WindowMinutes != nil {
		value := *current.WindowMinutes
		merged.WindowMinutes = &value
	}
	if merged.ResetsAt == nil && current.ResetsAt != nil {
		value := *current.ResetsAt
		merged.ResetsAt = &value
	}
	return merged
}

func cloneRateLimitWindow(window *RateLimitWindow) *RateLimitWindow {
	if window == nil {
		return nil
	}
	clone := *window
	if window.WindowMinutes != nil {
		value := *window.WindowMinutes
		clone.WindowMinutes = &value
	}
	if window.ResetsAt != nil {
		value := *window.ResetsAt
		clone.ResetsAt = &value
	}
	return &clone
}

func normalizeRateLimitID(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, "-", "_")
	return value
}

func rateLimitSnapshotHasData(snapshot RateLimitSnapshot) bool {
	return snapshot.Primary != nil ||
		snapshot.Secondary != nil ||
		snapshot.Credits != nil ||
		strings.TrimSpace(snapshot.LimitName) != "" ||
		strings.TrimSpace(snapshot.PlanType) != "" ||
		strings.TrimSpace(snapshot.RateLimitReachedType) != ""
}

func rateLimitSnapshotsEqual(a, b RateLimitSnapshot) bool {
	a.UpdatedAt = time.Time{}
	b.UpdatedAt = time.Time{}
	return a.LimitID == b.LimitID &&
		a.LimitName == b.LimitName &&
		a.PlanType == b.PlanType &&
		a.RateLimitReachedType == b.RateLimitReachedType &&
		rateLimitWindowsEqual(a.Primary, b.Primary) &&
		rateLimitWindowsEqual(a.Secondary, b.Secondary) &&
		rateLimitCreditsEqual(a.Credits, b.Credits)
}

func rateLimitWindowsEqual(a, b *RateLimitWindow) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.UsedPercent == b.UsedPercent &&
		int64PtrEqual(a.WindowMinutes, b.WindowMinutes) &&
		int64PtrEqual(a.ResetsAt, b.ResetsAt)
}

func rateLimitCreditsEqual(a, b *CreditsSnapshot) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.HasCredits == b.HasCredits && a.Unlimited == b.Unlimited && a.Balance == b.Balance
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
