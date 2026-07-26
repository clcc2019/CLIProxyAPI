package auth

import (
	"errors"
	"net/http"
	"strings"
)

// AuthScopedFailure marks an executor error whose consequence applies to the
// entire auth (i.e., the credential itself is no longer usable for any model),
// rather than only the model that triggered the error.
//
// Some upstreams expose a single shared quota bucket across all models, so a
// 429 on one model means every model on that auth will fail next. Without this
// marker, the conductor treats the 429 as per-model, a session-affinity binding
// stays on the depleted auth for unrelated models, and requests appear to
// "scatter" across credentials.
//
// Executors signal auth-scoped failures by returning an error that implements
// this interface. The conductor then suspends the auth as a whole instead of
// only the failing model.
type AuthScopedFailure interface {
	error
	IsAuthScopedFailure() bool
}

// isAuthScopedFailure reports whether err (or any error it wraps) signals an
// auth-wide failure that should suspend every model on the auth.
func isAuthScopedFailure(err error) bool {
	if err == nil {
		return false
	}
	var a AuthScopedFailure
	if errors.As(err, &a) && a != nil {
		if a.IsAuthScopedFailure() {
			return true
		}
	}
	return isUsageLimitExhaustedFailure(err)
}

// CredentialFailoverFailure marks an executor error that should abandon the
// currently selected credential for this request and try the next available
// credential after executor-local retries have been exhausted.
type CredentialFailoverFailure interface {
	error
	IsCredentialFailoverFailure() bool
}

func isCredentialFailoverFailure(err error) bool {
	if err == nil {
		return false
	}
	var f CredentialFailoverFailure
	if errors.As(err, &f) && f != nil {
		if f.IsCredentialFailoverFailure() {
			return true
		}
	}
	return isUsageLimitExhaustedFailure(err)
}

// isUsageLimitExhaustedFailure recognizes the quota-exhaustion response that
// Codex returns without requiring every executor to wrap it in both conductor
// marker interfaces. It is deliberately narrower than a generic 429: ordinary
// request-rate limiting should retain the executor's existing retry behavior.
func isUsageLimitExhaustedFailure(err error) bool {
	if err == nil {
		return false
	}
	raw := strings.ToLower(err.Error())
	if statusCodeFromError(err) != http.StatusTooManyRequests && !strings.Contains(raw, "http 429") {
		return false
	}
	return strings.Contains(raw, "usage limit has been reached") ||
		strings.Contains(raw, "you've hit your usage limit") ||
		strings.Contains(raw, "usage_limit_reached") ||
		strings.Contains(raw, "insufficient_quota")
}
