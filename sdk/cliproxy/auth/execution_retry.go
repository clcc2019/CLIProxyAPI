package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

func (m *Manager) retrySettings() (int, int, time.Duration) {
	if m == nil {
		return 0, 0, 0
	}
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load())
}

func (m *Manager) requestRetryLimitForAuth(auth *Auth) int {
	retry := 0
	if m != nil {
		retry = int(m.requestRetry.Load())
	}
	if auth != nil {
		if override, ok := auth.RequestRetryOverride(); ok {
			retry = override
		}
	}
	if retry < 0 {
		return 0
	}
	return retry
}

func shouldRetryTransportErrorWithSameAuth(err *Error, retryAttempt int, retryLimit int) bool {
	return retryAttempt >= 0 && retryAttempt < retryLimit && isProxyPoolTransportFailure(err)
}

// isClaudeOverloadedFailure identifies Anthropic's provider-wide transient
// overload response. It does not indicate anything about the credential, so
// it must never advance credential failover.
func isClaudeOverloadedFailure(provider string, err error) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "claude") && statusCodeFromError(err) == 529
}

func shouldRetryClaudeOverloadWithSameAuth(provider string, err error, retryAttempt int, retryLimit int) bool {
	return retryAttempt >= 0 && retryAttempt < retryLimit && isClaudeOverloadedFailure(provider, err)
}

func logSameAuthTransportRetry(ctx context.Context, auth *Auth, provider string, model string, attempt int, maxRetries int, err error) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	logEntryWithRequestID(ctx).WithFields(log.Fields{
		"auth_id":     authID,
		"provider":    provider,
		"model":       model,
		"retry":       attempt,
		"max_retries": maxRetries,
	}).WithError(err).Debug("retrying transient transport failure with same auth")
}

func logSameAuthClaudeOverloadRetry(ctx context.Context, auth *Auth, provider string, model string, attempt int, maxRetries int, err error) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	logEntryWithRequestID(ctx).WithFields(log.Fields{
		"auth_id":     authID,
		"provider":    provider,
		"model":       model,
		"retry":       attempt,
		"max_retries": maxRetries,
	}).WithError(err).Debug("retrying Claude overloaded response with same auth")
}

func borrowAuthIDSet() map[string]struct{} {
	raw := authIDSetPool.Get()
	if raw == nil {
		return make(map[string]struct{}, 16)
	}
	set, ok := raw.(map[string]struct{})
	if !ok || set == nil {
		return make(map[string]struct{}, 16)
	}
	return set
}

func releaseAuthIDSet(set map[string]struct{}) {
	if set == nil {
		return
	}
	if len(set) > 4096 {
		return
	}
	clear(set)
	authIDSetPool.Put(set)
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		blocked, reason, next := isAuthBlockedForModel(auth, checkModel, now)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) retryAllowed(attempt int, providers []string) bool {
	if m == nil || attempt < 0 || len(providers) == 0 {
		return false
	}
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

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
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt < effectiveRetry {
			return true
		}
	}
	return false
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	status := statusCodeFromError(err)
	if status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) || isRequestStopError(err) {
		return 0, false
	}
	wait, found := m.closestCooldownWait(providers, model, attempt)
	if found {
		if wait > maxWait {
			return 0, false
		}
		return wait, true
	}
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if !m.retryAllowed(attempt, providers) {
		return 0, false
	}
	retryAfter := retryAfterFromError(err)
	if retryAfter == nil || *retryAfter <= 0 || *retryAfter > maxWait {
		return 0, false
	}
	return *retryAfter, true
}

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
