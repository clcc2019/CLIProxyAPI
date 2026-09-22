package auth

import (
	"net/http"
	"time"
)

// homeDispatchRetryAfterError carries a trusted retry-after duration from Home.
type homeDispatchRetryAfterError struct {
	cause           error
	retryAfter      time.Duration
	requestRetry    int
	hasRequestRetry bool
}

func (e *homeDispatchRetryAfterError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}
func (e *homeDispatchRetryAfterError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *homeDispatchRetryAfterError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	d := e.retryAfter
	return &d
}

type homeRetryRoundExhaustedError struct {
	cause      error
	retryAfter time.Duration
}

func (e *homeRetryRoundExhaustedError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}
func (e *homeRetryRoundExhaustedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *homeRetryRoundExhaustedError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	d := e.retryAfter
	return &d
}

type authUnavailableError struct {
	message    string
	retryAfter time.Duration
}

func (e *authUnavailableError) Error() string {
	if e == nil || e.message == "" {
		return "authentication unavailable"
	}
	return e.message
}
func (e *authUnavailableError) Headers() http.Header {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	return safeRetryAfterHeader(e.retryAfter)
}
