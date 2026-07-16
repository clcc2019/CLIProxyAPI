package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type credentialRetryLimitError struct {
	cause        error
	mode         string
	model        string
	max          int
	attempted    int
	pinnedAuthID string
	sessionID    string
}

func (e *credentialRetryLimitError) Error() string {
	if e == nil {
		return ""
	}
	mode := strings.TrimSpace(e.mode)
	if mode == "" {
		mode = "request"
	}
	msg := fmt.Sprintf("credential failover stopped during %s after %d credential(s) (max-retry-credentials=%d", mode, e.attempted, e.max)
	if model := strings.TrimSpace(e.model); model != "" {
		msg += ", model=" + model
	}
	if authID := strings.TrimSpace(e.pinnedAuthID); authID != "" {
		msg += ", pinned_auth_id=" + authID
	}
	if sessionID := strings.TrimSpace(e.sessionID); sessionID != "" {
		msg += ", session_id=" + sessionID
	}
	msg += ")"
	if e.cause != nil {
		msg += ": " + e.cause.Error()
	}
	return msg
}

func (e *credentialRetryLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *credentialRetryLimitError) StatusCode() int {
	if e == nil || e.cause == nil {
		return 0
	}
	return statusCodeFromError(e.cause)
}

func (e *credentialRetryLimitError) Headers() http.Header {
	if e == nil || e.cause == nil {
		return nil
	}
	return headersFromError(e.cause)
}

type credentialFailoverRetryLimitError struct {
	authErr *Error
	cause   error
}

func (e *credentialFailoverRetryLimitError) Error() string {
	if e == nil || e.authErr == nil {
		return ""
	}
	return e.authErr.Error()
}

func (e *credentialFailoverRetryLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *credentialFailoverRetryLimitError) StatusCode() int {
	if e == nil || e.authErr == nil {
		return 0
	}
	return e.authErr.StatusCode()
}

func (e *credentialFailoverRetryLimitError) IsAuthScopedFailure() bool {
	return e != nil && isAuthScopedFailure(e.cause)
}

func (e *credentialFailoverRetryLimitError) IsCredentialFailoverFailure() bool {
	return e != nil && isCredentialFailoverFailure(e.cause)
}

func (e *credentialFailoverRetryLimitError) As(target any) bool {
	if e == nil || e.authErr == nil {
		return false
	}
	switch out := target.(type) {
	case **Error:
		*out = e.authErr
		return true
	default:
		return false
	}
}

func (m *Manager) credentialRetryLimitReachedError(ctx context.Context, mode string, providers []string, model string, opts cliproxyexecutor.Options, maxRetryCredentials int, attempted map[string]struct{}, lastErr error) error {
	attemptedCount := len(attempted)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	sessionID, fallbackSessionID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)

	fields := log.Fields{
		"mode":                  mode,
		"model":                 model,
		"providers":             strings.Join(providers, ","),
		"attempted_credentials": attemptedCount,
		"max_retry_credentials": maxRetryCredentials,
	}
	if pinnedAuthID != "" {
		fields["pinned_auth_id"] = pinnedAuthID
	}
	if sessionID != "" {
		fields["session_id"] = sessionID
	}
	if fallbackSessionID != "" && fallbackSessionID != sessionID {
		fields["fallback_session_id"] = fallbackSessionID
	}
	entry := logEntryWithRequestID(ctx).WithFields(fields)
	if lastErr != nil {
		entry.Warnf("auth failover stopped by max-retry-credentials: %v", lastErr)
		if isCredentialFailoverFailure(lastErr) {
			return &credentialFailoverRetryLimitError{
				authErr: &Error{
					Code:       "auth_unavailable",
					Message:    fmt.Sprintf("credential failover stopped during %s after %d credential(s) (max-retry-credentials=%d)", mode, attemptedCount, maxRetryCredentials),
					HTTPStatus: http.StatusServiceUnavailable,
				},
				cause: lastErr,
			}
		}
		return &credentialRetryLimitError{
			cause:        lastErr,
			mode:         mode,
			model:        model,
			max:          maxRetryCredentials,
			attempted:    attemptedCount,
			pinnedAuthID: pinnedAuthID,
			sessionID:    sessionID,
		}
	}
	entry.Warn("auth failover stopped by max-retry-credentials before another credential was selected")
	return &Error{
		Code:    "auth_not_found",
		Message: fmt.Sprintf("no auth available (max-retry-credentials=%d reached after %d credential(s))", maxRetryCredentials, attemptedCount),
	}
}
