// Package clienterror classifies upstream failures caused by the client request.
package clienterror

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// StatusClientClosedRequest is the nginx-style status for a canceled client request.
const StatusClientClosedRequest = 499

var requestFaultCodes = map[string]struct{}{
	"cyber_policy":                {},
	"context_length_exceeded":     {},
	"message_too_big":             {},
	"string_above_max_length":     {},
	"invalid_prompt":              {},
	"invalid_value":               {},
	"unsupported_value":           {},
	"invalid_request_error":       {},
	"previous_response_not_found": {},
}

var requestFaultTypes = map[string]struct{}{
	"invalid_request":       {},
	"invalid_request_error": {},
	"bad_request_error":     {},
	"invalid_prompt":        {},
}

// HTTPStatusFromError returns an explicit status before inferring cancellation statuses.
func HTTPStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) && statusErr != nil {
		if status := statusErr.StatusCode(); status > 0 {
			return status
		}
	}
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return 0
}

// IsRequestFault reports failures that retrying with another credential cannot fix.
func IsRequestFault(status int, err error) bool {
	if status <= 0 {
		status = HTTPStatusFromError(err)
	}
	if status == http.StatusPaymentRequired || status == http.StatusTooManyRequests {
		return false
	}
	if status == http.StatusUnauthorized && hasErrorType(err, "authentication_error") {
		return false
	}
	if HasRequestFault(err) {
		return true
	}
	switch status {
	case http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// HasRequestFault reports an explicit request fault without inferring from HTTP status.
func HasRequestFault(err error) bool {
	return hasRequestFaultBody(err) || (err != nil && IsItemNotPersisted(err.Error()))
}

// IsItemNotPersisted reports a stale Responses item reference created with store=false.
func IsItemNotPersisted(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func hasRequestFaultBody(err error) bool {
	if err == nil || !json.Valid([]byte(strings.TrimSpace(err.Error()))) {
		return false
	}
	for _, path := range []string{"error.code", "code", "response.error.code", "body.error.code"} {
		if _, ok := requestFaultCodes[strings.ToLower(strings.TrimSpace(gjson.Get(err.Error(), path).String()))]; ok {
			return true
		}
	}
	for errorType := range requestFaultTypes {
		if hasErrorType(err, errorType) {
			return true
		}
	}
	return false
}

func hasErrorType(err error, want string) bool {
	if err == nil || !json.Valid([]byte(strings.TrimSpace(err.Error()))) {
		return false
	}
	for _, path := range []string{"error.type", "type", "response.error.type", "body.error.type"} {
		if strings.EqualFold(strings.TrimSpace(gjson.Get(err.Error(), path).String()), want) {
			return true
		}
	}
	return false
}
