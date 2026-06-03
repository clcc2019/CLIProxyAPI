package executor

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexWebsocketConnectionLimitReachedCode = "websocket_connection_limit_reached"
const codexPreviousResponseNotFoundCode = "previous_response_not_found"
const codexNoToolCallFoundForFunctionOutputMessage = "no tool call found for function call output"

// statusErrWithHeaders decorates a statusErr with response headers that the
// upstream websocket-level error carried. We keep the distinction because
// callers that only care about status+body can keep using the naked
// statusErr, while adapters that need to replay the error as an HTTP
// response must surface the headers unchanged.
type statusErrWithHeaders struct {
	statusErr
	headers http.Header
}

func (e statusErrWithHeaders) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

// parseCodexWebsocketError recognises the websocket-layer {"type": "error", ...}
// frame shape and lifts it into a rich error value. Returns (nil, false) when
// the payload is empty or is a non-error frame.
func parseCodexWebsocketError(payload []byte) (error, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "error" {
		return nil, false
	}
	out := normalizeCodexWebsocketErrorBody(payload)
	status := parseCodexWebsocketErrorStatus(payload, out)
	if status <= 0 {
		status = http.StatusInternalServerError
	}

	headers := parseCodexWebsocketErrorHeaders(payload)
	err := newCodexStatusErr(status, out)
	return statusErrWithHeaders{
		statusErr: err,
		headers:   headers,
	}, true
}

func codexWebsocketConnectionLimitReached(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	for _, path := range []string{"error.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, path).String()), codexWebsocketConnectionLimitReachedCode) {
			return true
		}
	}
	return false
}

func codexWebsocketPreviousResponseNotFound(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	for _, path := range []string{"error.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, path).String()), codexPreviousResponseNotFoundCode) {
			return true
		}
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "error.param").String()), "previous_response_id") {
		return true
	}
	for _, path := range []string{"error.message", "message", "error"} {
		if codexPreviousResponseNotFoundText(gjson.GetBytes(payload, path).String()) {
			return true
		}
	}
	return false
}

func codexPreviousResponseNotFoundText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || !strings.Contains(lower, "not found") {
		return false
	}
	if strings.Contains(lower, "previous_response_id") {
		return true
	}
	return strings.Contains(lower, "previous response")
}

func codexWebsocketNoToolCallFoundForFunctionOutput(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	for _, path := range []string{"error.message", "message", "error"} {
		message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String()))
		if strings.Contains(message, codexNoToolCallFoundForFunctionOutputMessage) {
			return true
		}
	}
	return false
}

// normalizeCodexWebsocketErrorBody canonicalises the heterogeneous shapes
// upstream hands back into a single {"error": {"type": ..., "message": ...}}
// envelope. Every branch must keep the existing type + message fields intact
// so downstream translators (claude/openai/gemini) can inspect them uniformly.
func normalizeCodexWebsocketErrorBody(payload []byte) []byte {
	out := []byte(`{}`)
	errNode := gjson.GetBytes(payload, "error")
	switch {
	case errNode.Exists() && errNode.IsObject():
		out, _ = sjson.SetRawBytes(out, "error", []byte(errNode.Raw))
	case errNode.Exists() && errNode.Type == gjson.String:
		out, _ = sjson.SetBytes(out, "error.message", strings.TrimSpace(errNode.String()))
	case errNode.Exists():
		out, _ = sjson.SetBytes(out, "error.message", strings.TrimSpace(errNode.Raw))
	}

	if message := strings.TrimSpace(gjson.GetBytes(payload, "message").String()); message != "" &&
		strings.TrimSpace(gjson.GetBytes(out, "error.message").String()) == "" {
		out, _ = sjson.SetBytes(out, "error.message", message)
	}

	if errType := strings.TrimSpace(gjson.GetBytes(payload, "error_type").String()); errType != "" &&
		strings.TrimSpace(gjson.GetBytes(out, "error.type").String()) == "" {
		out, _ = sjson.SetBytes(out, "error.type", errType)
	}

	if strings.TrimSpace(gjson.GetBytes(out, "error.type").String()) == "" {
		switch {
		case isCodexUsageLimitError(out):
			out, _ = sjson.SetBytes(out, "error.type", "usage_limit_reached")
		default:
			out, _ = sjson.SetBytes(out, "error.type", "server_error")
		}
	}
	if strings.TrimSpace(gjson.GetBytes(out, "error.message").String()) == "" {
		status := parseCodexWebsocketErrorStatus(payload, out)
		if status <= 0 {
			status = http.StatusInternalServerError
		}
		out, _ = sjson.SetBytes(out, "error.message", http.StatusText(status))
	}
	return out
}

// parseCodexWebsocketErrorStatus pulls the most specific numeric status it
// can find, then lets codexStatusCode reconcile the wire value against the
// error body (e.g. promoting usage-limit errors to 429).
func parseCodexWebsocketErrorStatus(payload []byte, normalizedBody []byte) int {
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code"} {
		if status := int(gjson.GetBytes(payload, path).Int()); status > 0 {
			return codexStatusCode(status, normalizedBody)
		}
	}
	return codexStatusCode(0, normalizedBody)
}

// parseCodexWebsocketErrorHeaders translates the optional top-level "headers"
// map on an upstream error frame into an http.Header. Non-scalar values are
// skipped so malicious or malformed payloads cannot inject array/object
// headers with no obvious string representation.
func parseCodexWebsocketErrorHeaders(payload []byte) http.Header {
	headersNode := gjson.GetBytes(payload, "headers")
	if !headersNode.Exists() || !headersNode.IsObject() {
		return nil
	}
	mapped := make(http.Header)
	headersNode.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" {
			return true
		}
		switch value.Type {
		case gjson.String:
			if v := strings.TrimSpace(value.String()); v != "" {
				mapped.Set(name, v)
			}
		case gjson.Number, gjson.True, gjson.False:
			if v := strings.TrimSpace(value.Raw); v != "" {
				mapped.Set(name, v)
			}
		default:
		}
		return true
	})
	if len(mapped) == 0 {
		return nil
	}
	return mapped
}

// normalizeCodexWebsocketCompletion rewrites the legacy response.done event
// type to the canonical response.completed so downstream translators have a
// single shape to match on. Leaves other event types untouched.
func normalizeCodexWebsocketCompletion(payload []byte) []byte {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.done" {
		updated, err := sjson.SetBytes(payload, "type", "response.completed")
		if err == nil && len(updated) > 0 {
			return updated
		}
	}
	return payload
}

// encodeCodexWebsocketAsSSE produces the SSE "data: <payload>" line fragment
// from a raw JSON event. The trailing newline boundary is the caller's
// responsibility so this function can be composed into larger SSE writers
// without introducing surprise extra-blank lines.
func encodeCodexWebsocketAsSSE(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	line := make([]byte, 0, len("data: ")+len(payload))
	line = append(line, []byte("data: ")...)
	line = append(line, payload...)
	return line
}
