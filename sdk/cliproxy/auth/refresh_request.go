package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

func (m *Manager) refreshAuthShared(ctx context.Context, id string, useLimit bool) (*Auth, error) {
	if id == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result, err, _ := m.refreshGroup.Do(id, func() (any, error) {
		if !m.acquireRefreshSlot(ctx, useLimit) {
			return managerRefreshOutcome{err: ctx.Err()}, nil
		}
		defer m.releaseRefreshSlot(useLimit)
		updated, refreshErr := m.refreshAuthOnce(ctx, id)
		return managerRefreshOutcome{auth: updated, err: refreshErr}, nil
	})
	if err != nil {
		return nil, err
	}
	outcome, ok := result.(managerRefreshOutcome)
	if !ok {
		return m.authCloneByID(id), nil
	}
	if outcome.auth == nil && outcome.err == nil {
		outcome.auth = m.authCloneByID(id)
	}
	return outcome.auth, outcome.err
}

func (m *Manager) RefreshAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	return m.coordinatedRefreshForRequest(ctx, auth)
}

func (m *Manager) RefreshAuthIfNeeded(ctx context.Context, auth *Auth) (*Auth, error) {
	if m == nil || auth == nil {
		return auth, nil
	}
	current := auth
	if auth.ID != "" {
		if snapshot := m.authCloneByID(auth.ID); snapshot != nil {
			current = snapshot
		}
	}
	if !m.authAutoRefreshEnabled(current) || !m.shouldRefresh(current, time.Now()) {
		return current.Clone(), nil
	}
	return m.coordinatedRefreshForRequest(ctx, current)
}

// coordinatedRefreshForRequest serializes request-time refreshes through the
// same singleflight group used by the auto-refresh loop. Without this, the
// executor's own Refresh call could race the background loop on the same
// auth, using the rotating refresh token twice and bricking credentials
// with invalid_grant on the second attempt.
//
// Returns the freshly refreshed auth after the Manager has persisted it,
// or the error the executor's Refresh produced. On permanent failure
// (invalid_grant / revoked token), the error implements PermanentAuthError
// so callers can stop retrying with the same credentials.
func (m *Manager) coordinatedRefreshForRequest(ctx context.Context, auth *Auth) (*Auth, error) {
	if m == nil || auth == nil || auth.ID == "" {
		return auth, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id := auth.ID
	result, err, _ := m.refreshGroup.Do(id, func() (any, error) {
		m.mu.RLock()
		current := m.auths[id]
		if current == nil {
			m.mu.RUnlock()
			return managerRefreshOutcome{auth: auth.Clone()}, nil
		}
		if current.Disabled || current.Status == StatusDisabled {
			currentClone := current.Clone()
			m.mu.RUnlock()
			return managerRefreshOutcome{auth: currentClone}, nil
		}
		exec := m.executors[current.Provider]
		cloned := current.Clone()
		m.mu.RUnlock()
		if exec == nil {
			return managerRefreshOutcome{auth: cloned}, nil
		}
		updated, refreshErr := exec.Refresh(ctx, cloned)
		if refreshErr != nil {
			// Bubble up permanent failures so the conductor can park the
			// auth; the auto-refresh loop will also see the error on its
			// next tick and apply the same treatment.
			if isPermanentRefreshError(refreshErr) {
				refreshErrInfo := refreshErrorFromError(refreshErr)
				unauthorized := refreshErrInfo != nil && refreshErrInfo.StatusCode() == http.StatusUnauthorized
				shouldPersist := false
				m.mu.Lock()
				if cur := m.auths[id]; !authRefreshSuppressed(cur) {
					now := time.Now()
					if unauthorized {
						cur.NextRefreshAfter = time.Time{}
					} else {
						cur.NextRefreshAfter = now.Add(refreshPermanentBackoff)
					}
					cur.LastError = refreshErrInfo
					cur.Status = StatusError
					cur.Unavailable = true
					cur.StatusMessage = "refresh token invalid — re-login required"
					m.auths[id] = cur
					shouldPersist = true
					if m.scheduler != nil {
						m.scheduler.upsertAuth(cur.CloneForScheduler())
					}
				}
				m.mu.Unlock()
				m.queueRefreshReschedule(id)
				if shouldPersist {
					m.enqueuePersistAuthID(ctx, id)
				}
			}
			return managerRefreshOutcome{err: refreshErr}, nil
		}
		if updated == nil {
			return managerRefreshOutcome{auth: cloned}, nil
		}
		// Persist the rotated credentials through Manager.Update so the new
		// refresh token lands on disk synchronously before the caller uses
		// the returned auth. Update also resets NextRefreshAfter using the
		// executor's intended interval, and rebroadcasts to the scheduler.
		persisted, updateErr := m.Update(withRefreshUpdate(ctx), updated)
		if updateErr != nil {
			logEntryWithRequestID(ctx).WithField("auth_id", id).Warnf("coordinated refresh: persist failed: %v", updateErr)
			return managerRefreshOutcome{auth: updated, err: updateErr}, nil
		}
		if persisted != nil {
			return managerRefreshOutcome{auth: persisted}, nil
		}
		return managerRefreshOutcome{auth: updated}, nil
	})
	if err != nil {
		return nil, err
	}
	outcome, ok := result.(managerRefreshOutcome)
	if !ok {
		return m.authCloneByID(id), nil
	}
	if outcome.auth == nil && outcome.err == nil {
		outcome.auth = m.authCloneByID(id)
	}
	return outcome.auth, outcome.err
}

func (m *Manager) authCloneByID(id string) *Auth {
	if m == nil || id == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if auth := m.auths[id]; auth != nil {
		return auth.Clone()
	}
	return nil
}

func (m *Manager) acquireRefreshSlot(ctx context.Context, useLimit bool) bool {
	if !useLimit || m.refreshSemaphore == nil {
		return true
	}
	select {
	case m.refreshSemaphore <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *Manager) releaseRefreshSlot(useLimit bool) {
	if !useLimit || m.refreshSemaphore == nil {
		return
	}
	<-m.refreshSemaphore
}

func (m *Manager) refreshAuthOnce(ctx context.Context, id string) (*Auth, error) {
	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	var cloned *Auth
	if !authRefreshSuppressed(auth) {
		exec = m.executors[auth.Provider]
		cloned = auth.Clone()
	}
	m.mu.RUnlock()
	if cloned == nil || exec == nil {
		return nil, nil
	}
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return cloned, err
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		// Permanent auth errors (invalid_grant / invalid_client / revoked
		// refresh token) must not be retried on the background cadence —
		// doing so burns upstream quota and keeps the auth permanently in a
		// failing state. Park it until an operator re-logs in.
		permanent := isPermanentRefreshError(err)
		refreshErrInfo := refreshErrorFromError(err)
		unauthorized := refreshErrInfo != nil && refreshErrInfo.StatusCode() == http.StatusUnauthorized
		shouldReschedule := false
		shouldPersist := false
		m.mu.Lock()
		if current := m.auths[id]; !authRefreshSuppressed(current) {
			if permanent {
				if unauthorized {
					current.NextRefreshAfter = time.Time{}
				} else {
					current.NextRefreshAfter = now.Add(refreshPermanentBackoff)
				}
				current.Status = StatusError
				current.Unavailable = true
				current.StatusMessage = "refresh token invalid — re-login required"
			} else {
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			}
			current.LastError = refreshErrInfo
			m.auths[id] = current
			shouldReschedule = true
			shouldPersist = true
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.CloneForScheduler())
			}
		}
		m.mu.Unlock()
		if permanent {
			log.Warnf("auth %s (%s) parked for re-login: %v", auth.ID, auth.Provider, err)
		}
		if shouldReschedule {
			m.queueRefreshReschedule(id)
		}
		if shouldPersist {
			m.enqueuePersistAuthID(ctx, id)
		}
		return m.authCloneByID(id), err
	}
	if updated == nil {
		updated = cloned
	}
	executorNextRefreshAfter := updated.NextRefreshAfter
	inheritedNextRefreshAfter := cloned.NextRefreshAfter
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	if !executorNextRefreshAfter.IsZero() &&
		executorNextRefreshAfter.After(now) &&
		!executorNextRefreshAfter.Equal(inheritedNextRefreshAfter) {
		updated.NextRefreshAfter = executorNextRefreshAfter
	} else if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	persisted, updateErr := m.Update(withRefreshUpdate(ctx), updated)
	if updateErr != nil {
		return updated, updateErr
	}
	if persisted != nil {
		return persisted, nil
	}
	return updated, nil
}
