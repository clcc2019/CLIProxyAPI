package executor

import (
	"errors"
	"io"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// codexResponsesAggregateIdleTimeout bounds how long the aggregate reader will
// wait for a new byte from the upstream stream before force-closing the body.
// 10 minutes matches the upstream heartbeat cadence plus generous slack.
const codexResponsesAggregateIdleTimeout = 10 * time.Minute

// codexAggregateCapturedBodyMaxBytes caps the raw body we keep for request
// logging. Declared as a var so tests can lower the budget and exercise the
// capture-truncation path without generating multi-megabyte fixtures.
var codexAggregateCapturedBodyMaxBytes int64 = helps.MaxNonStreamResponseBodyBytes

// collectCodexResponseAggregate consumes the entire upstream SSE stream into a
// codexNonStreamHTTPResult without an idle-timeout guard.
// collectCodexResponseAggregateWithIdleTimeout is the preferred entry point in
// production code paths; this zero-timeout variant stays available because
// several unit tests rely on the simpler signature.
func collectCodexResponseAggregate(body io.Reader, captureBody bool) (codexNonStreamHTTPResult, error) {
	return collectCodexResponseAggregateWithIdleTimeout(body, captureBody, 0)
}

// collectCodexResponseAggregateWithIdleTimeout buffers the upstream stream into
// a synthetic non-streaming result. When captureBody is true the raw SSE lines
// are retained for the request log, up to codexAggregateCapturedBodyMaxBytes;
// exceeding that budget flips the reader into "consume but drop" mode so the
// stream still completes and completedData gets populated. Exceeding the
// capture budget is therefore *not* a hard failure — the logging cap should
// never demote a successful upstream response into an error the downstream
// translator has to recover from.
func collectCodexResponseAggregateWithIdleTimeout(body io.Reader, captureBody bool, idleTimeout time.Duration) (codexNonStreamHTTPResult, error) {
	var idleReader *idleTimeoutReadCloser
	if idleTimeout > 0 {
		if readCloser, ok := body.(io.ReadCloser); ok {
			idleReader = newIdleTimeoutReadCloser(readCloser, idleTimeout)
			body = idleReader
		}
	}
	if idleReader != nil {
		defer idleReader.StopTimer()
	}

	result := codexNonStreamHTTPResult{}
	streamState := newCodexStreamCompletionState()
	if captureBody {
		result.body = make([]byte, 0, 1024)
	}
	// Once the request-log capture exceeds its byte budget we drop the rest of
	// the raw body but keep consuming the stream so streamState.completedData
	// can still be filled. Abandoning the whole read the moment the capture
	// buffer overflows would turn a logging-only limit into a hard request
	// failure, which is not what the operator enabled `RequestLog` for.
	captureTruncated := false
	err := helps.ReadStreamLines(body, func(line []byte) error {
		if captureBody && !captureTruncated {
			if int64(len(result.body)+len(line)+1) > codexAggregateCapturedBodyMaxBytes {
				codexMetrics.captureTruncated.Add(1)
				log.Warnf("codex aggregate capture truncated: limit=%d bytes (continuing stream)", codexAggregateCapturedBodyMaxBytes)
				captureTruncated = true
			} else {
				result.body = append(result.body, line...)
				result.body = append(result.body, '\n')
			}
		}
		eventData, ok := codexEventData(line)
		if !ok {
			return nil
		}
		eventType := codexEventType(eventData)
		if terminalErr, ok := parseCodexStreamTerminalError(eventType, eventData); ok {
			result.errorStatus = terminalErr.code
			result.errorBody = []byte(terminalErr.msg)
		}
		// Single structured WARN per terminal event: pulls the three fields
		// operators actually correlate on (eventType, incomplete reason,
		// error message) into one line instead of scattering them across
		// separate per-type logs.
		switch eventType {
		case "response.incomplete":
			codexMetrics.terminalIncomplete.Add(1)
			reason := gjson.GetBytes(eventData, "response.incomplete_details.reason").String()
			if reason == "" {
				reason = "unknown"
			}
			log.Warnf("codex aggregate terminated event=response.incomplete reason=%s", reason)
		case "response.failed":
			codexMetrics.terminalFailed.Add(1)
			message := gjson.GetBytes(eventData, "response.error.message").String()
			if message == "" {
				message = "response.failed"
			}
			code := gjson.GetBytes(eventData, "response.error.code").String()
			if code == "" {
				code = "unknown"
			}
			log.Warnf("codex aggregate terminated event=response.failed code=%s message=%s", code, message)
		}
		if completed, isCompleted := streamState.processEventDataWithType(eventType, eventData, true); isCompleted {
			result.completedData = completed.data
			return errCodexStopStream
		}
		return nil
	})
	if errors.Is(err, errCodexStopStream) {
		return result, nil
	}
	if errors.Is(err, io.ErrUnexpectedEOF) && len(result.completedData) > 0 {
		return result, nil
	}
	return result, err
}

// idleTimeoutReadCloser wraps an io.ReadCloser with a watchdog timer that
// force-closes the underlying body after `idleTimeout` of no-bytes activity.
// Resetting the timer on every read that delivered bytes (n > 0) keeps the
// watchdog aligned with actual upstream activity rather than Go's error
// plumbing, which matters for chunked SSE responses that legitimately return
// (n > 0, io.EOF) on their last read.
type idleTimeoutReadCloser struct {
	io.ReadCloser
	idleTimeout time.Duration
	timer       *time.Timer
}

func newIdleTimeoutReadCloser(body io.ReadCloser, idleTimeout time.Duration) *idleTimeoutReadCloser {
	reader := &idleTimeoutReadCloser{
		ReadCloser:  body,
		idleTimeout: idleTimeout,
	}
	reader.timer = time.AfterFunc(idleTimeout, func() {
		_ = body.Close()
	})
	return reader
}

func (r *idleTimeoutReadCloser) Read(p []byte) (int, error) {
	if r == nil || r.ReadCloser == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := r.ReadCloser.Read(p)
	// Any bytes delivered count as upstream activity, regardless of whether
	// `err` is nil, io.EOF, or a trailer-related sentinel. Only gating the
	// reset on `err == nil` was too narrow: a chunked SSE response legitimately
	// returns `(n > 0, io.EOF)` on its last read, and heartbeat/keepalive
	// payloads that arrive just before an upstream trailer should not be
	// mistaken for an idle stream.
	if n > 0 && r.timer != nil {
		r.timer.Reset(r.idleTimeout)
	}
	return n, err
}

func (r *idleTimeoutReadCloser) Close() error {
	if r == nil || r.ReadCloser == nil {
		return nil
	}
	r.StopTimer()
	return r.ReadCloser.Close()
}

func (r *idleTimeoutReadCloser) StopTimer() {
	if r == nil {
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
}
