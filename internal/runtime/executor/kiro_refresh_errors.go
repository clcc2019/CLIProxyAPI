package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrKiroRefreshInvalidGrant signals that the Kiro refresh token is permanently
// invalid (e.g. rotated out, session expired, or revoked). Callers MUST NOT
// retry with the same refresh token; the auth entry requires a human re-login.
//
// This is distinct from transient network/5xx errors which should be retried,
// and from 401 on the upstream CodeWhisperer API which can still be recovered
// by refreshing with a valid refresh token.
var ErrKiroRefreshInvalidGrant = errors.New("kiro: refresh token invalid_grant — re-login required")

// kiroRefreshPermanentError wraps ErrKiroRefreshInvalidGrant with upstream
// context (OAuth error code and description) so operators can understand why
// the refresh was rejected without losing the sentinel identity needed for
// errors.Is checks.
type kiroRefreshPermanentError struct {
	OAuthCode string
	Detail    string
}

func (e *kiroRefreshPermanentError) Error() string {
	parts := []string{ErrKiroRefreshInvalidGrant.Error()}
	if e.OAuthCode != "" {
		parts = append(parts, fmt.Sprintf("code=%s", e.OAuthCode))
	}
	if e.Detail != "" {
		parts = append(parts, fmt.Sprintf("detail=%s", e.Detail))
	}
	return strings.Join(parts, " ")
}

func (e *kiroRefreshPermanentError) Unwrap() error { return ErrKiroRefreshInvalidGrant }

// IsPermanentAuthError implements cliproxyauth.PermanentAuthError. The
// conductor uses this to stop the auto-refresh loop for this auth.
func (e *kiroRefreshPermanentError) IsPermanentAuthError() bool { return true }

// StatusCode lets existing status-based routing (isUnauthorizedStatusErr,
// conductor's applyAuthFailureState) treat the error as 401 so the executor
// does not swallow it. The conductor's classification then picks up the
// sentinel to decide whether to suspend retries permanently.
func (e *kiroRefreshPermanentError) StatusCode() int { return 401 }

// isKiroRefreshPermanent reports whether err (or anything it wraps) indicates
// the refresh token itself is no longer usable.
func isKiroRefreshPermanent(err error) bool {
	return errors.Is(err, ErrKiroRefreshInvalidGrant)
}

type kiroUpstreamEventError struct {
	Kind       string
	Message    string
	statusCode int
}

func (e *kiroUpstreamEventError) Error() string {
	if e == nil {
		return "kiro API error"
	}
	parts := []string{"kiro API error"}
	if strings.TrimSpace(e.Kind) != "" {
		parts = append(parts, strings.TrimSpace(e.Kind))
	}
	if strings.TrimSpace(e.Message) != "" {
		parts = append(parts, strings.TrimSpace(e.Message))
	}
	return strings.Join(parts, ": ")
}

func (e *kiroUpstreamEventError) StatusCode() int {
	if e == nil || e.statusCode == 0 {
		return http.StatusInternalServerError
	}
	return e.statusCode
}

func (e *kiroUpstreamEventError) RetryAfter() *time.Duration { return nil }

func (e *kiroUpstreamEventError) IsAuthScopedFailure() bool {
	return e != nil && e.StatusCode() == http.StatusTooManyRequests && isKiroQuotaExhausted429Message(e.Error())
}

func newKiroUpstreamEventError(event map[string]any, fallbackKind string) error {
	if len(event) == 0 {
		return &kiroUpstreamEventError{Kind: fallbackKind, statusCode: http.StatusInternalServerError}
	}
	kind := firstNonEmptyKiroString(
		firstKiroString(event, "__type", "_type", "errorType", "error_type", "error", "code"),
		fallbackKind,
	)
	message := firstKiroString(event, "message", "errorMessage", "error_message", "detail", "reason")
	statusCode := kiroEventErrorStatusCode(event, kind, message)
	return &kiroUpstreamEventError{Kind: kind, Message: message, statusCode: statusCode}
}

func kiroEventErrorStatusCode(event map[string]any, kind, message string) int {
	if code := int64Field(event, "statusCode", "status_code", "httpStatusCode", "http_status_code"); code >= 100 && code <= 599 {
		return int(code)
	}
	lower := strings.ToLower(strings.TrimSpace(kind + " " + message + " " + firstKiroString(event, "status")))
	switch {
	case strings.Contains(lower, "throttl"), strings.Contains(lower, "too many"), strings.Contains(lower, "rate limit"), isKiroTransientModelCapacity429Message(lower):
		return http.StatusTooManyRequests
	case strings.Contains(lower, "unauthor"), strings.Contains(lower, "invalid bearer"), strings.Contains(lower, "expired bearer"):
		return http.StatusUnauthorized
	case strings.Contains(lower, "accessdenied"), strings.Contains(lower, "access denied"), strings.Contains(lower, "forbidden"):
		return http.StatusForbidden
	case strings.Contains(lower, "notfound"), strings.Contains(lower, "not found"):
		return http.StatusNotFound
	case strings.Contains(lower, "validation"), strings.Contains(lower, "badrequest"), strings.Contains(lower, "bad request"),
		strings.Contains(lower, "invalid"), strings.Contains(lower, "malformed"), strings.Contains(lower, "improperly formed"),
		strings.Contains(lower, "serialization"), strings.Contains(lower, "deserialization"), strings.Contains(lower, "failed_precondition"),
		strings.Contains(lower, "failed precondition"):
		return http.StatusBadRequest
	case strings.Contains(lower, "timeout"):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// classifyKiroOAuthError returns a permanent error when the upstream OAuth
// response body identifies an irrecoverable condition; otherwise returns nil
// and the caller should continue with the original status-based error.
//
// AWS SSO-OIDC and the kiro.dev social endpoint both return standard OAuth
// error envelopes on 400 (RFC 6749 §5.2). The codes that mean "this refresh
// token can never be used again" are:
//
//	invalid_grant     — refresh token expired, revoked, or already rotated
//	invalid_client    — clientId/clientSecret no longer accepted
//	unauthorized_client — client is not allowed to use this grant type
//	access_denied     — user or admin revoked consent
func classifyKiroOAuthError(statusCode int, body []byte) error {
	if statusCode < 400 || statusCode >= 500 {
		return nil
	}
	if len(body) == 0 {
		return nil
	}
	var errResp struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		// kiro.dev social endpoint sometimes uses camelCase
		ErrorCamel string `json:"errorCode"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(body, &errResp); err != nil {
		return nil
	}
	code := strings.TrimSpace(errResp.Error)
	if code == "" {
		code = strings.TrimSpace(errResp.ErrorCamel)
	}
	detail := strings.TrimSpace(errResp.ErrorDescription)
	if detail == "" {
		detail = strings.TrimSpace(errResp.Message)
	}
	switch strings.ToLower(code) {
	case "invalid_grant",
		"invalid_client",
		"unauthorized_client",
		"access_denied",
		"invalidgrantexception",
		"invalidclientexception":
		return &kiroRefreshPermanentError{OAuthCode: code, Detail: detail}
	}
	return nil
}

// kiroAuthScopedQuotaError wraps a 429 from Kiro's CodeWhisperer /
// generateAssistantResponse endpoint to signal that the underlying
// AGENTIC_REQUEST bucket is shared across every model on this auth. The
// conductor treats the failure as auth-wide rather than model-specific,
// which in turn lets session-affinity slide off the depleted credential
// onto the next available one instead of scattering requests across
// multiple auths by model.
type kiroAuthScopedQuotaError struct {
	inner statusErr
}

func (e *kiroAuthScopedQuotaError) Error() string             { return e.inner.Error() }
func (e *kiroAuthScopedQuotaError) StatusCode() int           { return e.inner.StatusCode() }
func (e *kiroAuthScopedQuotaError) Unwrap() error             { return e.inner }
func (e *kiroAuthScopedQuotaError) IsAuthScopedFailure() bool { return true }

// RetryAfter is forwarded so the conductor's retry-hint parsing picks up
// any Retry-After header that doKiroRequest attached to the underlying
// statusErr. Currently statusErr has no retry-after accessor exposed on
// the interface the conductor inspects; implementing it here is harmless
// and future-proofs the wrapping should one be added.
func (e *kiroAuthScopedQuotaError) RetryAfter() *time.Duration { return e.inner.retryAfter }

// wrapKiroAuthScoped upgrades a status error to signal an auth-scoped
// failure when appropriate. Kiro can return 429 for both true plan quota
// exhaustion and short lived throttling. Only the former should suspend the
// whole auth; throttling should remain a normal statusErr so the conductor
// applies model/request cooldown without blacking out every model on this auth.
func wrapKiroAuthScoped(err error) error {
	if err == nil {
		return nil
	}
	var se statusErr
	if !errors.As(err, &se) {
		return err
	}
	if se.code != 429 {
		return err
	}
	if !isKiroQuotaExhausted429Message(se.msg) {
		return err
	}
	return &kiroAuthScopedQuotaError{inner: se}
}

func isKiroQuotaExhausted429Message(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	if isKiroTransientModelCapacity429Message(lower) {
		return false
	}
	quotaSignals := [...]string{
		"quota",
		"usage limit",
		"usage_limit",
		"limit reached",
		"limit has been reached",
		"request limit",
		"monthly limit",
		"daily limit",
		"credit",
		"credits",
		"allowance",
		"exhausted",
		"resource_exhausted",
		"servicequotaexceeded",
		"service quota exceeded",
	}
	for _, signal := range quotaSignals {
		if strings.Contains(lower, signal) {
			return true
		}
	}
	return false
}

func isKiroTransientModelCapacity429Message(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	transientSignals := [...]string{
		"insufficient_model_capacity",
		"insufficient model capacity",
		"experiencing high traffic",
		"high traffic",
		"please try again shortly",
		"selected model is at capacity",
		"model is at capacity",
		"no capacity available",
	}
	for _, signal := range transientSignals {
		if strings.Contains(lower, signal) {
			return true
		}
	}
	return false
}
