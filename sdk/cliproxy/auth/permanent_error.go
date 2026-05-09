package auth

import (
	"errors"
	"time"
)

// refreshPermanentBackoff parks an auth whose refresh token is permanently
// invalid. The interval is long enough to stop the auto-refresh loop from
// burning upstream quota but finite so operator intervention (re-login,
// edit, or auth file replacement) is picked up on the next tick.
const refreshPermanentBackoff = 24 * time.Hour

// PermanentAuthError marks an error returned from ProviderExecutor.Refresh as
// unrecoverable without human intervention (revoked refresh token, rotated
// client credentials, revoked consent). The conductor uses this marker to
// suspend auto-refresh retries until the operator re-authenticates.
type PermanentAuthError interface {
	error
	IsPermanentAuthError() bool
}

// isPermanentRefreshError reports whether err (or any error it wraps)
// indicates the credentials cannot recover without operator intervention.
func isPermanentRefreshError(err error) bool {
	if err == nil {
		return false
	}
	var p PermanentAuthError
	if errors.As(err, &p) && p != nil {
		return p.IsPermanentAuthError()
	}
	return false
}
