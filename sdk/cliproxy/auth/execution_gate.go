package auth

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// executionAuthBlockedError means the selected credential became unavailable
// after selection but before its request could be sent upstream. It is a local
// admission result, not an upstream failure, so the caller should try another
// eligible credential without consuming the normal credential retry budget.
type executionAuthBlockedError struct {
	cause error
}

func (e *executionAuthBlockedError) Error() string {
	if e == nil || e.cause == nil {
		return "auth unavailable"
	}
	return e.cause.Error()
}

func (e *executionAuthBlockedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *executionAuthBlockedError) IsCredentialFailoverFailure() bool {
	return e != nil
}

func (m *Manager) authExecutionGate(authID string) *sync.RWMutex {
	if m == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	value, _ := m.authExecutionGates.LoadOrStore(authID, &sync.RWMutex{})
	if gate, ok := value.(*sync.RWMutex); ok && gate != nil {
		return gate
	}
	// The map is private to Manager, but recover safely if a malformed value is
	// ever encountered rather than allowing a credential to bypass the gate.
	gate := &sync.RWMutex{}
	m.authExecutionGates.Store(authID, gate)
	return gate
}

func (m *Manager) lockAuthExecutionGate(authID string) func() {
	gate := m.authExecutionGate(authID)
	if gate == nil {
		return func() {}
	}
	gate.Lock()
	return gate.Unlock
}

// admitAuthExecution takes a per-auth read lease across the actual executor
// call. A concurrent failure-state update takes the matching write lease, so
// once a quota cooldown has been recorded, no later local execution can pass
// this check. Work already admitted before the state update is intentionally
// allowed to finish; an in-flight upstream request cannot be unsent.
func (m *Manager) admitAuthExecution(auth *Auth, model string, sameRequestRetry bool) (func(), error) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil, &executionAuthBlockedError{cause: &Error{
			Code:       "auth_not_found",
			Message:    "selected auth is missing",
			HTTPStatus: http.StatusServiceUnavailable,
		}}
	}

	gate := m.authExecutionGate(auth.ID)
	if gate != nil {
		gate.RLock()
	}
	release := func() {
		if gate != nil {
			gate.RUnlock()
		}
	}

	if err := m.authExecutionBlocked(auth, model, time.Now(), sameRequestRetry); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (m *Manager) authExecutionBlocked(selected *Auth, model string, now time.Time, sameRequestRetry bool) error {
	if selected == nil {
		return &executionAuthBlockedError{cause: &Error{
			Code:       "auth_not_found",
			Message:    "selected auth is missing",
			HTTPStatus: http.StatusServiceUnavailable,
		}}
	}

	candidate := selected
	if m != nil {
		m.mu.RLock()
		current := m.auths[selected.ID]
		if current != nil {
			candidate = current
		}
		homeEnabled := m.HomeEnabled()
		m.mu.RUnlock()
		if current == nil && !homeEnabled {
			return &executionAuthBlockedError{cause: &Error{
				Code:       "auth_not_found",
				Message:    "selected auth is no longer registered",
				HTTPStatus: http.StatusServiceUnavailable,
			}}
		}
	}

	blocked, reason, recoverAt := isAuthBlockedForModel(candidate, model, now)
	if !blocked {
		return nil
	}
	// A pool-mode retry is part of the same logical request that was already
	// admitted. Keep its legacy per-model retry behavior (for example, retrying
	// a 502) while still treating disabled auths and auth-wide failures as hard
	// stops. New requests always pass sameRequestRetry=false and cannot bypass
	// a model cooldown.
	if sameRequestRetry && model != "" && reason != blockReasonDisabled &&
		!hasUnauthorizedAuthFailure(candidate) && !authScopedCooldownActive(candidate, now) {
		return nil
	}
	if reason == blockReasonCooldown {
		return &executionAuthBlockedError{cause: newModelCooldownError(model, candidate.Provider, recoverAt.Sub(now))}
	}
	return &executionAuthBlockedError{cause: &Error{
		Code:       "auth_unavailable",
		Message:    "selected auth is unavailable",
		HTTPStatus: http.StatusServiceUnavailable,
	}}
}
