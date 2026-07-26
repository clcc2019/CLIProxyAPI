package auth

import "net/http"

// Attempt kinds label why a credential attempt ended. They exist so operators
// can filter a failover chain by cause without pattern-matching free-form
// upstream error text, which varies per provider and changes without notice.
const (
	// attemptKindQuota: the credential is rate limited or out of quota.
	attemptKindQuota = "quota"
	// attemptKindUnauthorized: the credential was rejected (401/403).
	attemptKindUnauthorized = "unauthorized"
	// attemptKindModelUnsupported: this credential cannot serve the model.
	attemptKindModelUnsupported = "model_unsupported"
	// attemptKindInvalidRequest: the request itself is bad; failover is futile
	// and the loop stops rather than burning further credentials.
	attemptKindInvalidRequest = "invalid_request"
	// attemptKindUpstreamError: upstream returned 5xx.
	attemptKindUpstreamError = "upstream_error"
	// attemptKindTransport: no HTTP status — connection or proxy failure.
	attemptKindTransport = "transport"
	// attemptKindOther: classified nowhere above.
	attemptKindOther = "other"
)

// classifyAttemptFailure maps a credential failure to a stable kind.
//
// Order matters: the specific, actionable classifications are checked before
// the status-code fallback, because a single status covers several causes
// (a 429 can be either a rate limit or a hard quota exhaustion, and a 404 can
// mean either "no such model" or "no such endpoint").
func classifyAttemptFailure(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case isUsageLimitExhaustedFailure(err):
		return attemptKindQuota
	case isModelSupportError(err):
		return attemptKindModelUnsupported
	case isRequestInvalidError(err):
		return attemptKindInvalidRequest
	case isUnauthorizedError(err):
		return attemptKindUnauthorized
	}
	switch status := statusCodeFromError(err); {
	case status == http.StatusTooManyRequests:
		return attemptKindQuota
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return attemptKindUnauthorized
	case status >= 500:
		return attemptKindUpstreamError
	case status > 0:
		return attemptKindOther
	default:
		// No HTTP status at all means the request never got a response:
		// dial failure, proxy error, TLS failure, or a stream that died
		// before any status was observed.
		return attemptKindTransport
	}
}
