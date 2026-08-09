package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

var defaultSSEKeepAlive = []byte(": keep-alive\n\n")

var ErrStreamClosedBeforePayload = errors.New("auth manager stream closed before sending payload")

var errNilStreamChannels = errors.New("stream forwarder received nil data and error channels")
var errNilStreamChunkWriter = errors.New("stream forwarder received data without a chunk writer")

type FirstStreamChunk struct {
	Chunk []byte
	Data  <-chan []byte
	Errs  <-chan *interfaces.ErrorMessage
}

type FirstStreamResultChunk struct {
	Chunk []byte
}

type StreamForwardOptions struct {
	// KeepAliveInterval overrides the configured streaming keep-alive interval.
	// If nil, the configured default is used. If set to <= 0, keep-alives are disabled.
	KeepAliveInterval *time.Duration

	// WriteChunk writes a single data chunk to the response body. It should not flush.
	// It returns true when bytes were actually written to the response body.
	WriteChunk func(chunk []byte) bool

	// WriteTerminalError writes an error payload to the response body when streaming fails
	// after headers have already been committed. It should not flush.
	WriteTerminalError func(errMsg *interfaces.ErrorMessage)

	// WriteDone optionally writes a terminal marker when the upstream data channel closes
	// without an error (e.g. OpenAI's `[DONE]`). It should not flush.
	WriteDone func()

	// WriteKeepAlive optionally writes a keep-alive heartbeat. It should not flush.
	// When nil, a standard SSE comment heartbeat is used.
	WriteKeepAlive func()

	// ValidateChunk validates a provider chunk before it is written. A failure is
	// surfaced as a terminal upstream error.
	ValidateChunk func([]byte) error
}

// AwaitStreamResultFirstChunk waits for the first non-empty payload from the
// direct StreamResult API. Authentication bootstrap has already completed by
// the time this function is called, but the checks keep custom executors safe.
func AwaitStreamResultFirstChunk(ctx context.Context, result *coreexecutor.StreamResult) (FirstStreamResultChunk, *interfaces.ErrorMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if result == nil || result.Chunks == nil {
		return FirstStreamResultChunk{}, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: ErrStreamClosedBeforePayload}, nil
	}
	for {
		if len(result.Buffered) > 0 {
			chunk := result.Buffered[0]
			result.Buffered = result.Buffered[1:]
			if result.Observe != nil {
				result.Observe(chunk)
			}
			if chunk.Err != nil {
				FinalizeStreamResult(result, false)
				return FirstStreamResultChunk{}, errorMessageFromError(chunk.Err, http.StatusInternalServerError), nil
			}
			if len(chunk.Payload) > 0 {
				return FirstStreamResultChunk{Chunk: chunk.Payload}, nil, nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			FinalizeStreamResult(result, false)
			return FirstStreamResultChunk{}, nil, ctx.Err()
		case chunk, ok := <-result.Chunks:
			if !ok {
				FinalizeStreamResult(result, true)
				return FirstStreamResultChunk{}, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: ErrStreamClosedBeforePayload}, nil
			}
			if result.Observe != nil {
				result.Observe(chunk)
			}
			if chunk.Err != nil {
				FinalizeStreamResult(result, false)
				return FirstStreamResultChunk{}, errorMessageFromError(chunk.Err, http.StatusInternalServerError), nil
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			return FirstStreamResultChunk{Chunk: chunk.Payload}, nil, nil
		}
	}
}

// FinalizeStreamResult completes a direct stream lifecycle. It is safe to call
// more than once when the producer supplied an idempotent callback.
func FinalizeStreamResult(result *coreexecutor.StreamResult, completed bool) {
	if result != nil && result.Finalize != nil {
		result.Finalize(completed)
	}
}

func PendingStreamError(errs <-chan *interfaces.ErrorMessage) (*interfaces.ErrorMessage, bool) {
	if errs == nil {
		return nil, false
	}
	for {
		select {
		case errMsg, ok := <-errs:
			if !ok {
				return nil, false
			}
			if errMsg == nil {
				continue
			}
			return errMsg, true
		default:
			return nil, false
		}
	}
}

func ErrorMessageCause(errMsg *interfaces.ErrorMessage) error {
	if errMsg == nil {
		return nil
	}
	if errMsg.Error != nil {
		return errMsg.Error
	}
	status := errMsg.StatusCode
	if status < http.StatusBadRequest {
		status = http.StatusInternalServerError
	}
	text := strings.TrimSpace(http.StatusText(status))
	if text == "" {
		text = "stream error"
	}
	return errors.New(text)
}

func AwaitStreamFirstChunk(ctx context.Context, data <-chan []byte, errs <-chan *interfaces.ErrorMessage) (FirstStreamChunk, *interfaces.ErrorMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	closedBeforePayload := func() *interfaces.ErrorMessage {
		return &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: ErrStreamClosedBeforePayload}
	}
	if data == nil && errs == nil {
		return FirstStreamChunk{}, closedBeforePayload(), nil
	}
	for {
		select {
		case <-ctx.Done():
			return FirstStreamChunk{}, nil, ctx.Err()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				if data == nil {
					return FirstStreamChunk{}, closedBeforePayload(), nil
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			return FirstStreamChunk{}, errMsg, nil
		case chunk, ok := <-data:
			if !ok {
				if errMsg, okPendingErr := PendingStreamError(errs); okPendingErr {
					return FirstStreamChunk{}, errMsg, nil
				}
				return FirstStreamChunk{}, closedBeforePayload(), nil
			}
			return FirstStreamChunk{Chunk: chunk, Data: data, Errs: errs}, nil, nil
		}
	}
}

func (h *BaseAPIHandler) ForwardStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, opts StreamForwardOptions) {
	writeKeepAlive := opts.WriteKeepAlive
	if writeKeepAlive == nil {
		writeKeepAlive = func() {
			_, _ = c.Writer.Write(defaultSSEKeepAlive)
		}
	}

	keepAliveInterval := StreamingKeepAliveInterval(h.Cfg)
	if opts.KeepAliveInterval != nil {
		keepAliveInterval = *opts.KeepAliveInterval
	}
	var keepAlive *time.Ticker
	var keepAliveC <-chan time.Time
	if keepAliveInterval > 0 {
		keepAlive = time.NewTicker(keepAliveInterval)
		defer keepAlive.Stop()
		keepAliveC = keepAlive.C
	}

	flushNow := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	failNilStreamChannels := func() {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errNilStreamChannels}
		if opts.WriteTerminalError != nil {
			opts.WriteTerminalError(errMsg)
			flushNow()
		}
		cancel(errNilStreamChannels)
	}

	if data == nil && errs == nil {
		failNilStreamChannels()
		return
	}
	requestCtx := c.Request.Context()

	var terminalErr *interfaces.ErrorMessage
	for {
		select {
		case <-requestCtx.Done():
			cancel(requestCtx.Err())
			return
		case chunk, ok := <-data:
			if !ok {
				// Prefer surfacing a terminal error if one is pending.
				if terminalErr == nil {
					terminalErr, _ = PendingStreamError(errs)
				}
				if terminalErr != nil {
					if opts.WriteTerminalError != nil {
						opts.WriteTerminalError(terminalErr)
					}
					flushNow()
					cancel(ErrorMessageCause(terminalErr))
					return
				}
				if opts.WriteDone != nil {
					opts.WriteDone()
				}
				flushNow()
				cancel(nil)
				return
			}
			if opts.WriteChunk == nil {
				errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errNilStreamChunkWriter}
				if opts.WriteTerminalError != nil {
					opts.WriteTerminalError(errMsg)
					flushNow()
				}
				cancel(errNilStreamChunkWriter)
				return
			}
			if opts.WriteChunk(chunk) {
				flushNow()
			}
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				if data == nil {
					failNilStreamChannels()
					return
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			terminalErr = errMsg
			if opts.WriteTerminalError != nil {
				opts.WriteTerminalError(errMsg)
				flushNow()
			}
			cancel(ErrorMessageCause(errMsg))
			return
		case <-keepAliveC:
			writeKeepAlive()
			flushNow()
		}
	}
}

// ForwardStreamResult forwards a direct executor stream without introducing a
// relay goroutine or split data/error channels.
func (h *BaseAPIHandler) ForwardStreamResult(c *gin.Context, flusher http.Flusher, cancel func(error), result *coreexecutor.StreamResult, opts StreamForwardOptions) {
	writeKeepAlive := opts.WriteKeepAlive
	if writeKeepAlive == nil {
		writeKeepAlive = func() {
			_, _ = c.Writer.Write(defaultSSEKeepAlive)
		}
	}
	keepAliveInterval := StreamingKeepAliveInterval(h.Cfg)
	if opts.KeepAliveInterval != nil {
		keepAliveInterval = *opts.KeepAliveInterval
	}
	var keepAlive *time.Ticker
	var keepAliveC <-chan time.Time
	if keepAliveInterval > 0 {
		keepAlive = time.NewTicker(keepAliveInterval)
		defer keepAlive.Stop()
		keepAliveC = keepAlive.C
	}
	flushNow := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	cancelWith := func(err error) {
		if cancel != nil {
			cancel(err)
		}
	}
	writeError := func(errMsg *interfaces.ErrorMessage) {
		if opts.WriteTerminalError != nil {
			opts.WriteTerminalError(errMsg)
			flushNow()
		}
		cancelWith(ErrorMessageCause(errMsg))
	}
	if result == nil || result.Chunks == nil {
		FinalizeStreamResult(result, false)
		writeError(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errNilStreamChannels})
		return
	}
	requestCtx := c.Request.Context()
	for {
		if len(result.Buffered) > 0 {
			chunk := result.Buffered[0]
			result.Buffered = result.Buffered[1:]
			if result.Observe != nil {
				result.Observe(chunk)
			}
			if chunk.Err != nil {
				FinalizeStreamResult(result, false)
				writeError(errorMessageFromError(chunk.Err, http.StatusInternalServerError))
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			if opts.ValidateChunk != nil {
				if err := opts.ValidateChunk(chunk.Payload); err != nil {
					FinalizeStreamResult(result, false)
					writeError(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
					return
				}
			}
			if opts.WriteChunk == nil {
				FinalizeStreamResult(result, false)
				writeError(&interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errNilStreamChunkWriter})
				return
			}
			if opts.WriteChunk(chunk.Payload) {
				flushNow()
			}
			continue
		}
		select {
		case <-requestCtx.Done():
			FinalizeStreamResult(result, false)
			cancelWith(requestCtx.Err())
			return
		case chunk, ok := <-result.Chunks:
			if !ok {
				FinalizeStreamResult(result, true)
				if opts.WriteDone != nil {
					opts.WriteDone()
				}
				flushNow()
				cancelWith(nil)
				return
			}
			if result.Observe != nil {
				result.Observe(chunk)
			}
			if chunk.Err != nil {
				FinalizeStreamResult(result, false)
				writeError(errorMessageFromError(chunk.Err, http.StatusInternalServerError))
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			if opts.ValidateChunk != nil {
				if err := opts.ValidateChunk(chunk.Payload); err != nil {
					FinalizeStreamResult(result, false)
					writeError(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
					return
				}
			}
			if opts.WriteChunk == nil {
				FinalizeStreamResult(result, false)
				writeError(&interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errNilStreamChunkWriter})
				return
			}
			if opts.WriteChunk(chunk.Payload) {
				flushNow()
			}
		case <-keepAliveC:
			writeKeepAlive()
			flushNow()
		}
	}
}
