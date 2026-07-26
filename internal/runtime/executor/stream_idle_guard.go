package executor

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// providerStreamIdleTimeout bounds how long any provider's SSE stream may sit
// without delivering a byte before the body is force-closed.
//
// Without this guard a stalled upstream — one that keeps the TCP connection
// open but stops writing — parks the reader goroutine forever, because the
// executors build their clients with `Timeout: 0` (no client-level deadline)
// and a healthy socket produces no read error. The request slot, the goroutine
// and the upstream connection all leak until the downstream client gives up.
//
// The value matches codexResponsesStreamIdleTimeout so every provider shares
// one stall policy. It is deliberately generous: long tool calls and extended
// thinking legitimately go minutes between SSE frames, so this is a
// last-resort liveness bound, not a latency budget.
const providerStreamIdleTimeout = codexResponsesStreamIdleTimeout

// streamIdleTimeoutErr is the terminal error surfaced when a stream stalls.
// It reuses the Codex phrasing so operators correlate one message across
// providers, and 408 keeps it inside the retryable band the auth manager
// already understands.
func streamIdleTimeoutErr() statusErr {
	return statusErr{code: http.StatusRequestTimeout, msg: "stream error: idle timeout waiting for SSE"}
}

// guardStreamIdle wraps body with the shared idle watchdog.
//
// It returns the reader to hand to ReadStreamLines plus the guard itself, so
// the caller can consult TimedOut() after the read loop ends. That check is
// mandatory rather than optional: the watchdog trips by closing the body,
// which surfaces to ReadStreamLines as a plain io.EOF — and ReadStreamLines
// maps EOF to nil. A stalled stream is therefore indistinguishable from a
// clean end-of-stream unless TimedOut() is consulted, which is exactly how a
// truncated response would otherwise be reported to the client as success.
//
// Callers must invoke StopTimer once the read loop returns (defer is fine) so
// the timer does not outlive the request.
func guardStreamIdle(body io.ReadCloser, idleTimeout time.Duration) (io.Reader, *idleTimeoutReadCloser) {
	if body == nil || idleTimeout <= 0 {
		return body, nil
	}
	guard := newIdleTimeoutReadCloser(body, idleTimeout)
	return guard, guard
}

// isOpenAIStreamTerminalLine reports whether an OpenAI-format SSE line ends the
// stream. Providers speaking that dialect (Kimi, xAI, openai-compat) close with
// a literal `data: [DONE]` sentinel rather than a typed event.
func isOpenAIStreamTerminalLine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	return bytes.Equal(trimmed, []byte("[DONE]"))
}

// openAIResponsesTerminalEvents are the Responses-protocol events that end a
// turn. "completed" is the success path; the rest are terminal failures the
// read loop reports on its own, so they must still count as "the stream got
// somewhere" rather than as a silent stall.
var openAIResponsesTerminalEvents = map[string]struct{}{
	"response.completed":  {},
	"response.failed":     {},
	"response.incomplete": {},
	"error":               {},
}

// isOpenAIResponsesTerminalPayload reports whether an SSE frame ends a
// Responses-protocol stream. The type may arrive either on the sibling
// `event:` line or inside the JSON payload, so both are checked.
func isOpenAIResponsesTerminalPayload(currentEvent string, payload []byte) bool {
	if _, ok := openAIResponsesTerminalEvents[strings.TrimSpace(currentEvent)]; ok {
		return true
	}
	if !gjson.ValidBytes(payload) {
		return false
	}
	_, ok := openAIResponsesTerminalEvents[gjson.GetBytes(payload, "type").String()]
	return ok
}

// resolveStreamIdleError folds a stall verdict into the read loop's error.
//
// `readErr` is whatever ReadStreamLines returned and `progressed` reports
// whether the stream already reached a terminal/complete state. When the
// watchdog fired without the stream completing, the idle timeout replaces
// readErr — including the readErr == nil case, which is the truncation the
// caller would otherwise report as a successful stream.
func resolveStreamIdleError(guard *idleTimeoutReadCloser, readErr error, progressed bool) error {
	if guard == nil || !guard.TimedOut() {
		return readErr
	}
	if progressed && readErr == nil {
		// The stream already delivered its terminal event; the watchdog only
		// fired while the body was draining afterwards. Nothing was lost, so
		// this must not be demoted into an error.
		return nil
	}
	return streamIdleTimeoutErr()
}
