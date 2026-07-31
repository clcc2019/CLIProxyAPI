package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	if m.markCleanModelSuccessResult(ctx, result) {
		return
	}
	unlockExecutionGate := func() {}
	if !result.Success {
		unlockExecutionGate = m.lockAuthExecutionGate(result.AuthID)
	}

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	schedulerDirty := false
	persistAuthID := ""
	var schedulerSnapshot *Auth
	var errorEventAuthSnapshot *Auth
	invalidateAuthAffinity := false
	publishErrorEvent := m.shouldPublishErrorEvent(result)

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()
		auth.recordRuntimeResult(now, result.Success)
		persistAuthID = auth.ID

		if result.Success {
			// A successful request that was already in flight when a whole-auth
			// quota cooldown was recorded must not revive that credential. The
			// cooldown is cleared only after its reset path confirms recovery.
			// Otherwise a late success can reset the aggregate state and make an
			// exhausted auth selectable again before its reset time.
			quotaCooldownActive := authScopedQuotaCooldownActive(auth, now)
			if result.Model != "" && !quotaCooldownActive {
				state := lookupModelState(auth, result.Model)
				if !authStateIsClean(auth) || (state != nil && !modelStateIsClean(state)) {
					state = ensureModelState(auth, result.Model)
					resetModelState(state, now)
					updateAggregatedAvailability(auth, now)
					if !hasModelError(auth, now) {
						auth.LastError = nil
						auth.StatusMessage = ""
						auth.Status = StatusActive
					}
					auth.UpdatedAt = now
					shouldResumeModel = true
					clearModelQuota = true
					schedulerDirty = true
				}
			} else if result.Model == "" && !quotaCooldownActive {
				if !authStateIsClean(auth) {
					schedulerDirty = true
				}
				clearAuthStateOnSuccess(auth, now)
			}
		} else {
			authWideFailure := result.AuthScoped || isAuthWideResultError(result.Error)
			if result.Model != "" && !authWideFailure {
				if !isRequestScopedNotFoundResultError(result.Error) && !isSessionContextResultError(result.Error) {
					disableCooling := quotaCooldownDisabledForAuth(auth)
					state := ensureModelState(auth, result.Model)
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
						auth.LastError = cloneError(result.Error)
						auth.StatusMessage = result.Error.Message
					}

					statusCode := statusCodeFromResult(result.Error)
					if isModelSupportResultError(result.Error) {
						next := now.Add(12 * time.Hour)
						state.NextRetryAfter = next
						suspendReason = "model_not_supported"
						shouldSuspendModel = true
					} else {
						switch statusCode {
						case 401:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "unauthorized"
								shouldSuspendModel = true
							}
						case 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "payment_required"
								shouldSuspendModel = true
							}
						case 404:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(12 * time.Hour)
								state.NextRetryAfter = next
								suspendReason = "not_found"
								shouldSuspendModel = true
							}
						case 429:
							var next time.Time
							if !disableCooling {
								next = quotaRecoverAt(now, result.RetryAfter)
							}
							state.NextRetryAfter = next
							state.Quota = QuotaState{
								Exceeded:      true,
								Reason:        "quota",
								NextRecoverAt: next,
							}
							if !disableCooling {
								suspendReason = "quota"
								shouldSuspendModel = true
								setModelQuota = true
							}
						case 408, 500, 502, 503, 504:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(1 * time.Minute)
								state.NextRetryAfter = next
							}
						default:
							state.NextRetryAfter = time.Time{}
						}
					}

					auth.Status = StatusError
					auth.UpdatedAt = now
					updateAggregatedAvailability(auth, now)
					schedulerDirty = true
				}
			} else {
				// Auth-wide failures suspend the credential itself, not just the
				// triggering model. Auth-scoped quota errors and generic 401s both
				// need future routing to move to a different auth file.
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
				// Mark auth-scoped failures so isAuthBlockedForModel treats every
				// model as unavailable, even ones with no per-model state.
				if result.AuthScoped || isAuthWideResultError(result.Error) {
					auth.Quota.AuthScope = true
				}
				if auth.Unavailable && !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
					invalidateAuthAffinity = true
				}
				if result.Model != "" {
					state := ensureModelState(auth, result.Model)
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
					}
					if !auth.NextRetryAfter.IsZero() {
						state.NextRetryAfter = auth.NextRetryAfter
					}
					if auth.Quota.Exceeded {
						state.Quota = auth.Quota
						state.Quota.AuthScope = false
						setModelQuota = true
					}
					suspendReason = authWideSuspendReason(result.Error, result.AuthScoped)
					shouldSuspendModel = true
				}
				schedulerDirty = true
			}
		}

		if schedulerDirty {
			schedulerSnapshot = auth.CloneForScheduler()
		}
		if publishErrorEvent {
			errorEventAuthSnapshot = auth.Clone()
		}
	}
	m.mu.Unlock()
	unlockExecutionGate()
	if persistAuthID != "" {
		m.enqueuePersistAuthID(ctx, persistAuthID)
	}
	if m.scheduler != nil && schedulerSnapshot != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	if invalidateAuthAffinity {
		m.invalidateSessionAffinityForAuth(result.AuthID)
	}

	if clearModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if setModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, result.Model)
	}
	if shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, result.Model)
	} else if shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, result.Model, suspendReason)
	}

	m.recordProxyPoolResult(ctx, result)
	if m.shouldReleaseProxyLeaseForResult(result) {
		m.releaseProxyLease(ctx, result.AuthID)
	}
	m.hook.OnResult(ctx, result)
	m.publishErrorEvent(result, errorEventAuthSnapshot)
}

// MarkAuthQuotaCooldown marks a credential as auth-scoped quota exhausted using
// a known recovery time, such as the reset_at value returned by a usage API.
func (m *Manager) MarkAuthQuotaCooldown(ctx context.Context, authID string, recoverAt time.Time) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	now := time.Now()
	if recoverAt.IsZero() {
		recoverAt = now.Add(quotaRefreshInterval)
	}
	if !recoverAt.After(now) {
		return
	}
	unlockExecutionGate := m.lockAuthExecutionGate(authID)

	persistAuthID := ""
	var schedulerSnapshot *Auth
	m.mu.Lock()
	if auth, ok := m.auths[authID]; ok && auth != nil {
		if quotaCooldownDisabledForAuth(auth) {
			m.mu.Unlock()
			unlockExecutionGate()
			return
		}
		auth.Unavailable = true
		auth.Status = StatusError
		auth.StatusMessage = "quota exhausted"
		auth.NextRetryAfter = recoverAt
		auth.Quota = QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: recoverAt,
			AuthScope:     true,
		}
		auth.LastError = &Error{
			Code:       "rate_limited",
			Message:    "codex usage quota exhausted",
			HTTPStatus: http.StatusTooManyRequests,
		}
		auth.UpdatedAt = now
		persistAuthID = auth.ID
		schedulerSnapshot = auth.CloneForScheduler()
	}
	m.mu.Unlock()
	unlockExecutionGate()

	if persistAuthID != "" {
		m.enqueuePersistAuthID(ctx, persistAuthID)
	}
	if m.scheduler != nil && schedulerSnapshot != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	if persistAuthID != "" {
		m.invalidateSessionAffinityForAuth(persistAuthID)
	}
}

// ClearAuthQuotaCooldown clears local quota cooldown state after an upstream
// rate-limit reset credit has been redeemed successfully.
func (m *Manager) ClearAuthQuotaCooldown(ctx context.Context, authID string) bool {
	return m.clearAuthQuotaCooldown(ctx, authID, true)
}

// ClearAuthQuotaCooldownFromUsage clears only an auth-wide quota cooldown
// after the provider's usage endpoint confirms that the credential has
// headroom. Model-specific cooldowns are intentionally retained because a
// usage response cannot establish that every model is available again.
func (m *Manager) ClearAuthQuotaCooldownFromUsage(ctx context.Context, authID string) bool {
	return m.clearAuthQuotaCooldown(ctx, authID, false)
}

// ClearExpiredQuotaCooldowns clears quota cooldowns whose known recovery time
// has passed. Once the provider's reset boundary is reached, retaining the old
// runtime state causes credential files to remain labelled "quota exhausted"
// even though they are eligible for use again.
//
// Only cooldowns with an explicit NextRecoverAt are cleared. A quota error
// without a known reset time remains intact until a successful usage check or
// request confirms that it has recovered.
func (m *Manager) ClearExpiredQuotaCooldowns(ctx context.Context) int {
	if m == nil {
		return 0
	}
	now := time.Now()
	authIDs := make([]string, 0)
	m.mu.RLock()
	for authID, auth := range m.auths {
		if authQuotaCooldownExpired(auth, now) || authHasExpiredModelQuotaCooldown(auth, now) {
			authIDs = append(authIDs, authID)
		}
	}
	m.mu.RUnlock()

	cleared := 0
	for _, authID := range authIDs {
		if m.clearExpiredQuotaCooldownsForAuth(ctx, authID, now) {
			cleared++
		}
	}
	return cleared
}

func (m *Manager) clearExpiredQuotaCooldownsForAuth(ctx context.Context, authID string, now time.Time) bool {
	if m == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}

	unlockExecutionGate := m.lockAuthExecutionGate(authID)
	persistAuthID := ""
	var schedulerSnapshot *Auth
	invalidateAuthAffinity := false
	clearedModels := make([]string, 0, 4)

	m.mu.Lock()
	if auth, ok := m.auths[authID]; ok && auth != nil && !auth.IsDisabled() {
		wasUnavailable := auth.Unavailable
		authExpired := authQuotaCooldownExpired(auth, now)
		changed := false
		if authExpired {
			clearAuthStateOnSuccess(auth, now)
			changed = true
		}
		for model, state := range auth.ModelStates {
			if model == "" || state == nil || state.Status == StatusDisabled || !modelQuotaCooldownExpired(state, now) {
				continue
			}
			resetModelState(state, now)
			clearedModels = append(clearedModels, model)
			changed = true
		}
		if changed {
			updateAggregatedAvailability(auth, now)
			if !hasModelError(auth, now) {
				auth.Status = StatusActive
				auth.StatusMessage = ""
				auth.LastError = nil
			}
			auth.UpdatedAt = now
			persistAuthID = auth.ID
			schedulerSnapshot = auth.CloneForScheduler()
			invalidateAuthAffinity = wasUnavailable
		}
	}
	m.mu.Unlock()
	unlockExecutionGate()

	if persistAuthID == "" {
		return false
	}
	m.enqueuePersistAuthID(ctx, persistAuthID)
	if m.scheduler != nil && schedulerSnapshot != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	if invalidateAuthAffinity {
		m.invalidateSessionAffinityForAuth(persistAuthID)
	}
	for _, model := range clearedModels {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(persistAuthID, model)
	}
	return true
}

func (m *Manager) clearAuthQuotaCooldown(ctx context.Context, authID string, clearModels bool) bool {
	if m == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	unlockExecutionGate := m.lockAuthExecutionGate(authID)

	now := time.Now()
	persistAuthID := ""
	var schedulerSnapshot *Auth
	invalidateAuthAffinity := false
	clearedModels := make([]string, 0, 4)

	m.mu.Lock()
	if auth, ok := m.auths[authID]; ok && auth != nil && !auth.IsDisabled() {
		wasUnavailable := auth.Unavailable
		changed := false
		authCleared := false
		if authHasQuotaCooldown(auth) {
			clearAuthStateOnSuccess(auth, now)
			changed = true
			authCleared = true
		}
		if clearModels {
			for model, state := range auth.ModelStates {
				if model == "" || state == nil || state.Status == StatusDisabled {
					continue
				}
				if modelStateHasQuotaCooldown(state) {
					resetModelState(state, now)
					clearedModels = append(clearedModels, model)
					changed = true
				}
			}
		}
		if changed {
			updateAggregatedAvailability(auth, now)
			if authCleared {
				auth.Status = StatusActive
				auth.StatusMessage = ""
				auth.LastError = nil
			}
			auth.UpdatedAt = now
			persistAuthID = auth.ID
			schedulerSnapshot = auth.CloneForScheduler()
			invalidateAuthAffinity = wasUnavailable
		}
	}
	m.mu.Unlock()
	unlockExecutionGate()

	if persistAuthID == "" {
		return false
	}
	m.enqueuePersistAuthID(ctx, persistAuthID)
	if m.scheduler != nil && schedulerSnapshot != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	if invalidateAuthAffinity {
		m.invalidateSessionAffinityForAuth(persistAuthID)
	}
	for _, model := range clearedModels {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(persistAuthID, model)
	}
	return true
}

func authHasQuotaCooldown(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Quota.Exceeded || auth.Quota.AuthScope || strings.EqualFold(auth.Quota.Reason, "quota") {
		return true
	}
	if strings.Contains(strings.ToLower(auth.StatusMessage), "quota") {
		return true
	}
	if auth.LastError != nil {
		code := strings.ToLower(strings.TrimSpace(auth.LastError.Code))
		return code == "rate_limited" || code == "usage_limit_reached" || code == "quota_exceeded"
	}
	return false
}

func authQuotaCooldownExpired(auth *Auth, now time.Time) bool {
	if auth == nil || auth.IsDisabled() || auth.Quota.NextRecoverAt.IsZero() || auth.Quota.NextRecoverAt.After(now) {
		return false
	}
	return authHasQuotaCooldown(auth)
}

func authHasExpiredModelQuotaCooldown(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	for _, state := range auth.ModelStates {
		if modelQuotaCooldownExpired(state, now) {
			return true
		}
	}
	return false
}

func modelQuotaCooldownExpired(state *ModelState, now time.Time) bool {
	if state == nil || state.Status == StatusDisabled || state.Quota.NextRecoverAt.IsZero() || state.Quota.NextRecoverAt.After(now) {
		return false
	}
	return modelStateHasQuotaCooldown(state)
}

// authScopedCooldownActive reports whether an auth-wide cooldown is still in
// effect, whether it came from quota exhaustion or another auth-scoped error.
func authScopedCooldownActive(auth *Auth, now time.Time) bool {
	if auth == nil || !auth.Unavailable || !auth.Quota.AuthScope {
		return false
	}
	return auth.NextRetryAfter.After(now)
}

// authScopedQuotaCooldownActive reports whether an auth-wide quota block is
// still in effect. It intentionally requires a future NextRetryAfter because
// stale cooldown state is allowed to be cleared by a later successful request.
func authScopedQuotaCooldownActive(auth *Auth, now time.Time) bool {
	return auth != nil && auth.Quota.Exceeded && authScopedCooldownActive(auth, now)
}

func modelStateHasQuotaCooldown(state *ModelState) bool {
	if state == nil {
		return false
	}
	if state.Quota.Exceeded || state.Quota.AuthScope || strings.EqualFold(state.Quota.Reason, "quota") {
		return true
	}
	if strings.Contains(strings.ToLower(state.StatusMessage), "quota") {
		return true
	}
	if state.LastError != nil {
		code := strings.ToLower(strings.TrimSpace(state.LastError.Code))
		return code == "rate_limited" || code == "usage_limit_reached" || code == "quota_exceeded"
	}
	return false
}

func (m *Manager) invalidateSessionAffinityForAuth(authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	if m.previousResponseAuths != nil {
		m.previousResponseAuths.InvalidateAuth(authID)
	}
	seen := make(map[*SessionAffinitySelector]struct{}, 2)
	invalidate := func(selector Selector) {
		affinity, ok := selector.(*SessionAffinitySelector)
		if !ok || affinity == nil {
			return
		}
		if _, okSeen := seen[affinity]; okSeen {
			return
		}
		seen[affinity] = struct{}{}
		affinity.InvalidateAuth(authID)
	}

	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()

	invalidate(selector)
}

func (m *Manager) markCleanModelSuccessResult(ctx context.Context, result Result) bool {
	if m == nil || !result.Success || result.AuthID == "" || result.Model == "" {
		return false
	}

	now := time.Now()
	persistAuthID := ""
	m.mu.RLock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		state := lookupModelState(auth, result.Model)
		if authStateIsClean(auth) && (state == nil || modelStateIsClean(state)) {
			auth.recordRuntimeResult(now, true)
			persistAuthID = auth.ID
		}
	}
	m.mu.RUnlock()
	if persistAuthID == "" {
		return false
	}
	m.enqueuePersistAuthID(ctx, persistAuthID)
	m.recordProxyPoolResult(ctx, result)
	m.hook.OnResult(ctx, result)
	return true
}

func lookupModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" || len(auth.ModelStates) == 0 {
		return nil
	}
	if state := auth.ModelStates[model]; state != nil {
		return state
	}
	baseModel := canonicalModelKey(model)
	if baseModel == "" || baseModel == model {
		return nil
	}
	return auth.ModelStates[baseModel]
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func modelStateIsClean(state *ModelState) bool {
	if state == nil {
		return true
	}
	if state.Status != StatusActive {
		return false
	}
	if state.Unavailable || state.StatusMessage != "" || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		return false
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		return false
	}
	return true
}

func authStateIsClean(auth *Auth) bool {
	if auth == nil {
		return true
	}
	if auth.Status != StatusActive {
		return false
	}
	if auth.Unavailable || auth.StatusMessage != "" || !auth.NextRetryAfter.IsZero() || auth.LastError != nil {
		return false
	}
	if auth.Quota.Exceeded || auth.Quota.Reason != "" || !auth.Quota.NextRecoverAt.IsZero() || auth.Quota.BackoffLevel != 0 {
		return false
	}
	return true
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if len(auth.ModelStates) == 0 {
		clearAggregatedAvailability(auth)
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		hasState = true
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		clearAggregatedAvailability(auth)
		return
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
		auth.Quota.AuthScope = false
	}
}

func clearAggregatedAvailability(auth *Auth) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.Quota.AuthScope = false
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

func resultErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		resultErr := cloneError(authErr)
		applyUsageLimitResultClassification(resultErr, err)
		return resultErr
	}
	resultErr := &Error{Message: err.Error()}
	if status := statusCodeFromError(err); status > 0 {
		resultErr.HTTPStatus = status
	}
	applyUsageLimitResultClassification(resultErr, err)
	return resultErr
}

func applyUsageLimitResultClassification(resultErr *Error, err error) {
	if resultErr == nil || !isUsageLimitExhaustedFailure(err) {
		return
	}
	if resultErr.Code == "" {
		resultErr.Code = "usage_limit_reached"
	}
	// Some upstream adapters expose only a textual "HTTP 429" error. Preserve
	// the inferred status so MarkResult records the auth as quota exhausted.
	if resultErr.HTTPStatus == 0 {
		resultErr.HTTPStatus = http.StatusTooManyRequests
	}
}

// applyResultError populates both Error and AuthScoped on a Result from the
// underlying executor error. It preserves existing type information (status
// codes, retry hints) that would otherwise be lost when resultErrorFromError
// flattens the error, and it propagates the AuthScopedFailure marker so
// Auth-wide shared-bucket quota errors suspend the auth globally instead of
// only the triggering model.
func applyResultError(result *Result, err error) {
	if result == nil {
		return
	}
	result.Error = resultErrorFromError(err)
	if isAuthScopedFailure(err) {
		result.AuthScoped = true
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func headersFromError(err error) http.Header {
	if err == nil {
		return nil
	}
	var he interface{ Headers() http.Header }
	if errors.As(err, &he) && he != nil {
		return cloneHTTPHeader(he.Headers())
	}
	return nil
}

func isUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromError(err) == http.StatusUnauthorized {
		return true
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "status 401") || strings.Contains(raw, "401 unauthorized")
}

func hasUnauthorizedAuthFailure(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	return auth.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(auth.LastError.Code, "unauthorized")
}

func refreshErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		cloned := cloneError(authErr)
		if cloned.HTTPStatus == 0 {
			cloned.HTTPStatus = statusCodeFromError(err)
		}
		if cloned.HTTPStatus == 0 && isUnauthorizedError(err) {
			cloned.HTTPStatus = http.StatusUnauthorized
		}
		if cloned.HTTPStatus == http.StatusUnauthorized {
			if cloned.Code == "" {
				cloned.Code = "unauthorized"
			}
			cloned.Retryable = false
		}
		return cloned
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 && isUnauthorizedError(err) {
		statusCode = http.StatusUnauthorized
	}
	resultErr := &Error{Message: err.Error(), HTTPStatus: statusCode}
	if statusCode == http.StatusUnauthorized {
		resultErr.Code = "unauthorized"
		resultErr.Retryable = false
	}
	return resultErr
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	value := *retryAfter
	return &value
}

func maxRetryCredentialsFromMetadata(meta map[string]any, fallback int) int {
	if len(meta) == 0 {
		return fallback
	}
	raw, ok := meta[cliproxyexecutor.MaxRetryCredentialsMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case int:
		if value >= 0 {
			return value
		}
	case int32:
		if value >= 0 {
			return int(value)
		}
	case int64:
		if value >= 0 {
			return int(value)
		}
	case float64:
		if value >= 0 {
			return int(value)
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && parsed >= 0 {
			return parsed
		}
	case []byte:
		parsed, err := strconv.Atoi(strings.TrimSpace(string(value)))
		if err == nil && parsed >= 0 {
			return parsed
		}
	}
	return fallback
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func isAuthWideResultError(err *Error) bool {
	switch statusCodeFromResult(err) {
	case http.StatusUnauthorized:
		return true
	case http.StatusBadRequest:
		return !isModelSupportResultError(err) &&
			!isRequestInvalidResultError(err) &&
			!isSessionContextResultError(err)
	default:
		return false
	}
}

func authWideSuspendReason(err *Error, explicitAuthScoped bool) string {
	switch statusCodeFromResult(err) {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired, http.StatusForbidden:
		return "payment_required"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		if explicitAuthScoped {
			return "auth_scope_quota"
		}
		return "quota"
	default:
		return "auth_scope_failure"
	}
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isModelSupportError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Message)
}

func isRequestScopedNotFoundMessage(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return isRequestScopedNotFoundMessage(err.Message)
}

func isSessionContextErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	if isContextLengthErrorText(lower) {
		return true
	}
	if strings.Contains(lower, "previous_response_not_found") {
		return true
	}
	if strings.Contains(lower, "not found") &&
		(strings.Contains(lower, "previous_response_id") || strings.Contains(lower, "previous response")) {
		return true
	}
	return strings.Contains(lower, "no tool call found") &&
		strings.Contains(lower, "call output")
}

func isSessionContextResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusBadRequest {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(err.Code))
	if code == "previous_response_not_found" || isContextLengthErrorText(code) {
		return true
	}
	return isSessionContextErrorMessage(err.Message)
}

func isContextLengthErrorText(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false
	}
	return strings.Contains(text, "context_too_large") ||
		strings.Contains(text, "context_length_exceeded") ||
		strings.Contains(text, "context window") ||
		strings.Contains(text, "context length") ||
		strings.Contains(text, "too many tokens")
}

func isRequestInvalidResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if isModelSupportResultError(err) {
		return false
	}
	switch statusCodeFromResult(err) {
	case http.StatusBadRequest:
		return isMissingResponsesRequestAnchorErrorMessage(err.Error())
	case http.StatusNotFound:
		return isRequestScopedNotFoundMessage(err.Error())
	case http.StatusUnprocessableEntity:
		return true
	case http.StatusInternalServerError:
		msg := err.Error()
		return strings.Contains(msg, "\"status\":\"UNKNOWN\"") ||
			strings.Contains(msg, "\"status\": \"UNKNOWN\"")
	default:
		return false
	}
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried. Specifically, it treats request-scoped 404
// item misses caused by `store=false`, all 422 responses, and Responses requests
// missing every valid request anchor as request-shape failures, where switching
// auths or pooled upstream models will not help. Other 400s deliberately fall
// through to credential failover so the next auth starts with a fresh upstream
// session instead of reusing polluted provider state.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	var resultErr *Error
	if errors.As(err, &resultErr) && resultErr != nil {
		return isRequestInvalidResultError(resultErr)
	}
	if isModelSupportError(err) {
		return false
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest:
		return isMissingResponsesRequestAnchorErrorMessage(err.Error())
	case http.StatusNotFound:
		return isRequestScopedNotFoundMessage(err.Error())
	case http.StatusUnprocessableEntity:
		return true
	case http.StatusInternalServerError:
		msg := err.Error()
		return strings.Contains(msg, "\"status\":\"UNKNOWN\"") ||
			strings.Contains(msg, "\"status\": \"UNKNOWN\"")
	default:
		return false
	}
}

// isMissingResponsesRequestAnchorErrorMessage identifies the Responses API
// validation error returned when a request has neither new input nor a valid
// continuation/template/conversation reference. This is a request-shape error;
// replaying it with another model or credential cannot make it valid.
func isMissingResponsesRequestAnchorErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" || !strings.Contains(lower, "one of") || !strings.Contains(lower, "must be provided") {
		return false
	}
	for _, field := range [...]string{"input", "previous_response_id", "prompt", "conversation_id"} {
		if !strings.Contains(lower, field) {
			return false
		}
	}
	return true
}

func isMalformedRequestErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"improperly formed request",
		"malformed payload",
		"malformed request",
		"validationexception",
		"validation exception",
		"serializationexception",
		"deserializationexception",
		"schema validation",
		"extra inputs are not permitted",
		"unexpected field",
		"invalid json",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	if isRequestScopedNotFoundResultError(resultErr) || isSessionContextResultError(resultErr) {
		return
	}
	disableCooling := quotaCooldownDisabledForAuth(auth)
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	switch statusCode {
	case 400:
		auth.StatusMessage = "bad_request"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 401:
		auth.StatusMessage = "unauthorized"
		// disable_cooling is intended for quota/capacity retry loops. A 401 means
		// the credential itself is invalid, so keep it out of routing until a
		// refresh or re-login updates the auth state.
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 402, 403:
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 404:
		auth.StatusMessage = "not_found"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		var next time.Time
		if !disableCooling {
			next = quotaRecoverAt(now, retryAfter)
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

func quotaRecoverAt(now time.Time, retryAfter *time.Duration) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	recoverAt := now.Add(quotaRefreshInterval)
	if retryAfter != nil && *retryAfter > 0 {
		if hinted := now.Add(*retryAfter); hinted.After(recoverAt) {
			recoverAt = hinted
		}
	}
	return recoverAt
}
