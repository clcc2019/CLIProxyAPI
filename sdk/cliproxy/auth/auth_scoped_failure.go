package auth

import "errors"

// AuthScopedFailure marks an executor error whose consequence applies to the
// entire auth (i.e., the credential itself is no longer usable for any model),
// rather than only the model that triggered the error.
//
// Kiro is the canonical example: its AGENTIC_REQUEST quota is a single shared
// bucket across all models, so a 429 on one model means every model on that
// auth will fail next. Without this marker, the conductor treats the 429 as
// per-model, a session-affinity binding stays on the depleted auth for
// unrelated models, and requests appear to "scatter" across credentials.
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
		return a.IsAuthScopedFailure()
	}
	return false
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
		return f.IsCredentialFailoverFailure()
	}
	return false
}
