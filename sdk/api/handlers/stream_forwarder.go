package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

var defaultSSEKeepAlive = []byte(": keep-alive\n\n")

var errNilStreamChannels = errors.New("stream forwarder received nil data and error channels")
var errNilStreamChunkWriter = errors.New("stream forwarder received data without a chunk writer")

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
}

func PendingStreamError(errs <-chan *interfaces.ErrorMessage) (*interfaces.ErrorMessage, bool) {
	if errs == nil {
		return nil, false
	}
	select {
	case errMsg, ok := <-errs:
		if !ok {
			return nil, false
		}
		return errMsg, true
	default:
		return nil, false
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
					select {
					case errMsg, ok := <-errs:
						if ok && errMsg != nil {
							terminalErr = errMsg
						}
					default:
					}
				}
				if terminalErr != nil {
					if opts.WriteTerminalError != nil {
						opts.WriteTerminalError(terminalErr)
					}
					flushNow()
					cancel(terminalErr.Error)
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
			if errMsg != nil {
				terminalErr = errMsg
				if opts.WriteTerminalError != nil {
					opts.WriteTerminalError(errMsg)
					flushNow()
				}
			}
			var execErr error
			if errMsg != nil {
				execErr = errMsg.Error
			}
			cancel(execErr)
			return
		case <-keepAliveC:
			writeKeepAlive()
			flushNow()
		}
	}
}
