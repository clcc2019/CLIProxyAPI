package executor

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func newCodexStatusErr(statusCode int, body []byte) statusErr {
	usageLimit := isCodexUsageLimitError(body)
	errCode := codexStatusCode(statusCode, body)
	if errCode <= 0 {
		errCode = http.StatusInternalServerError
	}
	err := statusErr{code: errCode, msg: string(body)}
	if usageLimit && errCode == http.StatusTooManyRequests {
		err.authScopedFailure = true
		err.credentialFailoverFailure = true
	}
	if retryAfter := parseCodexRetryAfter(errCode, body, time.Now()); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	return err
}

func isCodexModelCapacityError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	if asciifold.ContainsBytes(errorBody, "selected model is at capacity") ||
		asciifold.ContainsBytes(errorBody, "model is at capacity. please try a different model") {
		return true
	}
	if codexStatusResultContainsAnyFold(gjson.GetBytes(errorBody, "error.message"),
		"selected model is at capacity",
		"model is at capacity. please try a different model") {
		return true
	}
	if codexStatusResultContainsAnyFold(gjson.GetBytes(errorBody, "message"),
		"selected model is at capacity",
		"model is at capacity. please try a different model") {
		return true
	}
	return false
}

func isCodexUsageLimitError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	if asciifold.ContainsBytes(errorBody, "usage_limit_reached") ||
		asciifold.ContainsBytes(errorBody, "usage_not_included") ||
		asciifold.ContainsBytes(errorBody, "insufficient_quota") ||
		asciifold.ContainsBytes(errorBody, "rate_limit_exceeded") ||
		asciifold.ContainsBytes(errorBody, "usage limit has been reached") ||
		asciifold.ContainsBytes(errorBody, "you've hit your usage limit") ||
		asciifold.ContainsBytes(errorBody, "upgrade to plus") ||
		asciifold.ContainsBytes(errorBody, "continue using codex") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(codexStatusResultString(gjson.GetBytes(errorBody, "error.type"))), "usage_limit_reached") {
		return true
	}
	if codexStatusResultContainsUsageLimit(gjson.GetBytes(errorBody, "error.code")) {
		return true
	}
	if codexStatusResultContainsUsageLimit(gjson.GetBytes(errorBody, "code")) {
		return true
	}
	if codexStatusResultContainsUsageLimit(gjson.GetBytes(errorBody, "error.message")) {
		return true
	}
	if codexStatusResultContainsUsageLimit(gjson.GetBytes(errorBody, "message")) {
		return true
	}
	return false
}

func isCodexUnauthorizedError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	if asciifold.ContainsBytes(errorBody, "authentication_error") ||
		asciifold.ContainsBytes(errorBody, "unauthorized") ||
		asciifold.ContainsBytes(errorBody, "invalid_api_key") ||
		asciifold.ContainsBytes(errorBody, "invalid bearer") ||
		asciifold.ContainsBytes(errorBody, "token has been invalidated") ||
		(asciifold.ContainsBytes(errorBody, "authentication token") && asciifold.ContainsBytes(errorBody, "invalidated")) ||
		asciifold.ContainsBytes(errorBody, "please try signing in again") ||
		asciifold.ContainsBytes(errorBody, "sign in again") {
		return true
	}
	if codexStatusResultContainsUnauthorized(gjson.GetBytes(errorBody, "error.type")) {
		return true
	}
	if codexStatusResultContainsUnauthorized(gjson.GetBytes(errorBody, "type")) {
		return true
	}
	if codexStatusResultContainsUnauthorized(gjson.GetBytes(errorBody, "error.code")) {
		return true
	}
	if codexStatusResultContainsUnauthorized(gjson.GetBytes(errorBody, "code")) {
		return true
	}
	if codexStatusResultContainsUnauthorized(gjson.GetBytes(errorBody, "error.message")) {
		return true
	}
	if codexStatusResultContainsUnauthorized(gjson.GetBytes(errorBody, "message")) {
		return true
	}
	return false
}

func codexStatusCode(statusCode int, body []byte) int {
	if isCodexUnauthorizedError(body) {
		return http.StatusUnauthorized
	}
	if isCodexUsageLimitError(body) || isCodexModelCapacityError(body) {
		return http.StatusTooManyRequests
	}
	if statusCode <= 0 {
		switch codexErrorCode(body) {
		case "context_length_exceeded", "context_too_large", "invalid_prompt", "cyber_policy":
			return http.StatusBadRequest
		case "server_is_overloaded", "slow_down":
			return http.StatusServiceUnavailable
		}
	}
	return statusCode
}

func codexErrorCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if code := codexErrorCodeFromResult(gjson.GetBytes(body, "error.code")); code != "" {
		return code
	}
	if code := codexErrorCodeFromResult(gjson.GetBytes(body, "code")); code != "" {
		return code
	}
	return ""
}

func parseCodexStreamTerminalError(eventType string, eventData []byte) (statusErr, bool) {
	if err, ok := codexTerminalStreamContextLengthErr(eventData); ok {
		return err, true
	}
	switch strings.TrimSpace(eventType) {
	case "error":
		err, ok := parseCodexWebsocketError(eventData)
		if !ok || err == nil {
			return statusErr{}, false
		}
		if withHeaders, ok := err.(statusErrWithHeaders); ok {
			return withHeaders.statusErr, true
		}
		if plain, ok := err.(statusErr); ok {
			return plain, true
		}
		return statusErr{}, false
	case "response.failed":
		body := normalizeCodexResponseFailedErrorBody(eventData)
		status := parseCodexResponseFailedStatus(eventData, body)
		if status <= 0 {
			status = http.StatusInternalServerError
		}
		return newCodexStatusErr(status, body), true
	default:
		return statusErr{}, false
	}
}

func codexStreamIdleTimeoutErr() statusErr {
	return statusErr{code: http.StatusRequestTimeout, msg: "stream error: idle timeout waiting for SSE"}
}

func codexResponseIncompleteErr(reason string) statusErr {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "unknown"
	}
	body := []byte(`{"error":{}}`)
	body, _ = sjson.SetBytes(body, "error.message", "Incomplete response returned, reason: "+reason)
	body, _ = sjson.SetBytes(body, "error.type", "incomplete_response")
	body, _ = sjson.SetBytes(body, "error.code", "response_incomplete")
	body, _ = sjson.SetBytes(body, "error.reason", reason)
	return newCodexStatusErr(http.StatusBadGateway, body)
}

func codexResponseIncompleteEventErr(eventData []byte) statusErr {
	return codexResponseIncompleteErr(codexResponseIncompleteReason(eventData))
}

func codexResponseIncompleteReason(eventData []byte) string {
	reason := gjson.GetBytes(eventData, "response.incomplete_details.reason").String()
	if strings.TrimSpace(reason) == "" {
		return "unknown"
	}
	return reason
}

func codexTerminalStreamContextLengthErr(eventData []byte) (statusErr, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()
	var body []byte
	switch eventType {
	case "error":
		body = codexTerminalErrorBody(eventData, "error")
		if len(body) == 0 {
			body = codexTerminalTopLevelErrorBody(eventData)
		}
	case "response.failed":
		body = codexTerminalErrorBody(eventData, "response.error")
		if len(body) == 0 {
			body = codexTerminalErrorBody(eventData, "error")
		}
	default:
		return statusErr{}, false
	}
	if len(body) == 0 || !codexTerminalErrorIsContextLength(body) {
		return statusErr{}, false
	}
	return newCodexStatusErr(http.StatusBadRequest, normalizeCodexContextLengthErrorBody(body)), true
}

func codexTerminalErrorBody(eventData []byte, path string) []byte {
	errorResult := gjson.GetBytes(eventData, path)
	if !errorResult.Exists() {
		return nil
	}
	body := []byte(`{"error":{}}`)
	if errorResult.Type == gjson.JSON {
		body, _ = sjson.SetRawBytes(body, "error", []byte(errorResult.Raw))
	} else if message := strings.TrimSpace(errorResult.String()); message != "" {
		body, _ = sjson.SetBytes(body, "error.message", message)
	}
	if strings.TrimSpace(gjson.GetBytes(body, "error.message").String()) == "" {
		if message := strings.TrimSpace(gjson.GetBytes(eventData, "response.error.message").String()); message != "" {
			body, _ = sjson.SetBytes(body, "error.message", message)
		}
	}
	if strings.TrimSpace(gjson.GetBytes(body, "error.message").String()) == "" {
		if code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String()); code != "" {
			body, _ = sjson.SetBytes(body, "error.message", code)
		}
	}
	if strings.TrimSpace(gjson.GetBytes(body, "error.message").String()) == "" {
		if errorType := strings.TrimSpace(gjson.GetBytes(body, "error.type").String()); errorType != "" {
			body, _ = sjson.SetBytes(body, "error.message", errorType)
		}
	}
	return body
}

func codexTerminalTopLevelErrorBody(eventData []byte) []byte {
	message := strings.TrimSpace(gjson.GetBytes(eventData, "message").String())
	code := strings.TrimSpace(gjson.GetBytes(eventData, "code").String())
	errorType := strings.TrimSpace(gjson.GetBytes(eventData, "error_type").String())
	param := strings.TrimSpace(gjson.GetBytes(eventData, "param").String())
	if message == "" && code == "" && errorType == "" && param == "" {
		return nil
	}
	body := []byte(`{"error":{}}`)
	if message != "" {
		body, _ = sjson.SetBytes(body, "error.message", message)
	}
	if code != "" {
		body, _ = sjson.SetBytes(body, "error.code", code)
	}
	if errorType != "" {
		body, _ = sjson.SetBytes(body, "error.type", errorType)
	}
	if param != "" {
		body, _ = sjson.SetBytes(body, "error.param", param)
	}
	if strings.TrimSpace(gjson.GetBytes(body, "error.message").String()) == "" {
		if code != "" {
			body, _ = sjson.SetBytes(body, "error.message", code)
		} else if errorType != "" {
			body, _ = sjson.SetBytes(body, "error.message", errorType)
		}
	}
	return body
}

func codexTerminalErrorIsContextLength(body []byte) bool {
	switch codexErrorCode(body) {
	case "context_length_exceeded", "context_too_large":
		return true
	}
	for _, message := range []string{
		strings.TrimSpace(codexStatusResultString(gjson.GetBytes(body, "error.message"))),
		strings.TrimSpace(codexStatusResultString(gjson.GetBytes(body, "message"))),
	} {
		if codexErrorMessageMentionsContextLength(message) {
			return true
		}
	}
	return asciifold.ContainsBytes(body, "context window") ||
		asciifold.ContainsBytes(body, "context length") ||
		asciifold.ContainsBytes(body, "maximum context") ||
		asciifold.ContainsBytes(body, "max context") ||
		asciifold.ContainsBytes(body, "too many tokens")
}

func codexErrorMessageMentionsContextLength(message string) bool {
	return asciifold.Contains(message, "context window") ||
		asciifold.Contains(message, "context length") ||
		asciifold.Contains(message, "maximum context") ||
		asciifold.Contains(message, "max context") ||
		asciifold.Contains(message, "too many tokens")
}

func normalizeCodexContextLengthErrorBody(body []byte) []byte {
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = "context length exceeded"
	}
	out := []byte(`{"error":{}}`)
	out, _ = sjson.SetBytes(out, "error.message", message)
	out, _ = sjson.SetBytes(out, "error.type", "invalid_request_error")
	out, _ = sjson.SetBytes(out, "error.code", "context_too_large")
	if param := strings.TrimSpace(gjson.GetBytes(body, "error.param").String()); param != "" {
		out, _ = sjson.SetBytes(out, "error.param", param)
	}
	return out
}

func codexStatusResultString(result gjson.Result) string {
	if result.Type == gjson.String {
		return result.Str
	}
	return result.String()
}

func codexErrorCodeFromResult(result gjson.Result) string {
	code := strings.TrimSpace(codexStatusResultString(result))
	if code == "" {
		return ""
	}
	switch {
	case strings.EqualFold(code, "context_length_exceeded"):
		return "context_length_exceeded"
	case strings.EqualFold(code, "context_too_large"):
		return "context_too_large"
	case strings.EqualFold(code, "invalid_prompt"):
		return "invalid_prompt"
	case strings.EqualFold(code, "cyber_policy"):
		return "cyber_policy"
	case strings.EqualFold(code, "server_is_overloaded"):
		return "server_is_overloaded"
	case strings.EqualFold(code, "slow_down"):
		return "slow_down"
	default:
		return strings.ToLower(code)
	}
}

func codexStatusResultContainsAnyFold(result gjson.Result, substrs ...string) bool {
	candidate := strings.TrimSpace(codexStatusResultString(result))
	if candidate == "" {
		return false
	}
	for _, substr := range substrs {
		if asciifold.Contains(candidate, substr) {
			return true
		}
	}
	return false
}

func codexStatusResultContainsUsageLimit(result gjson.Result) bool {
	candidate := strings.TrimSpace(codexStatusResultString(result))
	if candidate == "" {
		return false
	}
	return asciifold.Contains(candidate, "usage_limit_reached") ||
		asciifold.Contains(candidate, "usage_not_included") ||
		asciifold.Contains(candidate, "insufficient_quota") ||
		asciifold.Contains(candidate, "rate_limit_exceeded") ||
		asciifold.Contains(candidate, "usage limit has been reached") ||
		asciifold.Contains(candidate, "you've hit your usage limit") ||
		asciifold.Contains(candidate, "upgrade to plus") ||
		asciifold.Contains(candidate, "continue using codex")
}

func codexStatusResultContainsUnauthorized(result gjson.Result) bool {
	candidate := strings.TrimSpace(codexStatusResultString(result))
	if candidate == "" {
		return false
	}
	return asciifold.Contains(candidate, "authentication_error") ||
		asciifold.Contains(candidate, "unauthorized") ||
		asciifold.Contains(candidate, "invalid_api_key") ||
		asciifold.Contains(candidate, "invalid bearer") ||
		asciifold.Contains(candidate, "token has been invalidated") ||
		(asciifold.Contains(candidate, "authentication token") && asciifold.Contains(candidate, "invalidated")) ||
		asciifold.Contains(candidate, "please try signing in again") ||
		asciifold.Contains(candidate, "sign in again")
}

func normalizeCodexResponseFailedErrorBody(eventData []byte) []byte {
	out := []byte(`{}`)
	errNode := gjson.GetBytes(eventData, "response.error")
	switch {
	case errNode.Exists() && errNode.IsObject():
		out, _ = sjson.SetRawBytes(out, "error", []byte(errNode.Raw))
	case errNode.Exists() && errNode.Type == gjson.String:
		out, _ = sjson.SetBytes(out, "error.message", strings.TrimSpace(errNode.String()))
	case errNode.Exists():
		out, _ = sjson.SetBytes(out, "error.message", strings.TrimSpace(errNode.Raw))
	}

	code := codexErrorCode(out)
	if strings.TrimSpace(gjson.GetBytes(out, "error.type").String()) == "" {
		switch {
		case isCodexUsageLimitError(out):
			out, _ = sjson.SetBytes(out, "error.type", "usage_limit_reached")
		case code == "invalid_prompt" || code == "cyber_policy":
			out, _ = sjson.SetBytes(out, "error.type", "invalid_request_error")
		default:
			out, _ = sjson.SetBytes(out, "error.type", "server_error")
		}
	}
	if strings.TrimSpace(gjson.GetBytes(out, "error.message").String()) == "" {
		if code == "cyber_policy" {
			out, _ = sjson.SetBytes(out, "error.message", "This request has been flagged for possible cybersecurity risk.")
		} else {
			out, _ = sjson.SetBytes(out, "error.message", "response.failed")
		}
	}
	return out
}

func parseCodexResponseFailedStatus(eventData []byte, normalizedBody []byte) int {
	for _, path := range []string{"response.status", "response.status_code", "response.error.status", "response.error.status_code"} {
		if status := int(gjson.GetBytes(eventData, path).Int()); status > 0 {
			return codexStatusCode(status, normalizedBody)
		}
	}
	return codexStatusCode(0, normalizedBody)
}
