package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/gin-gonic/gin"
)

const maxLoggedRequestBodyBytes int64 = 1 << 20 // 1 MiB

type capturedRequestBody struct {
	info          *RequestInfo
	limit         int64
	declaredBytes int64
	buffer        *bytes.Buffer
	hasher        hash.Hash
	observedBytes int64
	truncated     bool
	complete      bool
	released      bool
}

func newCapturedRequestBody(c *gin.Context, info *RequestInfo, limit int64, hashBody bool) *capturedRequestBody {
	if c == nil || c.Request == nil || c.Request.Body == nil || info == nil {
		return nil
	}

	body := c.Request.Body
	capture := &capturedRequestBody{
		info:          info,
		limit:         limit,
		declaredBytes: c.Request.ContentLength,
		buffer:        acquireRequestBuffer(),
	}
	if hashBody {
		capture.hasher = sha256.New()
	}
	c.Request.Body = &capturedRequestReadCloser{
		reader:  io.TeeReader(body, capture),
		closer:  body,
		capture: capture,
	}
	return capture
}

func (c *capturedRequestBody) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	c.observedBytes += int64(len(p))
	if c.hasher != nil {
		if _, err := c.hasher.Write(p); err != nil {
			return 0, err
		}
	}

	c.capturePreview(p)
	if c.buffer != nil {
		c.truncated = c.observedBytes > int64(c.buffer.Len())
	}
	return len(p), nil
}

func (c *capturedRequestBody) capturePreview(chunk []byte) {
	if c.buffer == nil {
		return
	}
	remaining := c.limit - int64(c.buffer.Len())
	if remaining <= 0 {
		return
	}

	writeLen := len(chunk)
	if int64(writeLen) > remaining {
		writeLen = int(remaining)
	}
	_, _ = c.buffer.Write(chunk[:writeLen])
	// NOTE: info.Body is only populated in applyToContext (once, after the
	// body has been fully read) so we do not hand downstream code a pointer
	// that still aliases the pooled buffer.
}

func (c *capturedRequestBody) markComplete() {
	c.complete = true
	c.publishBody()
}

func (c *capturedRequestBody) markClosed() {
	if c.complete {
		c.publishBody()
		return
	}
	if c.declaredBytes == 0 {
		c.complete = true
		c.publishBody()
		return
	}
	if c.declaredBytes > 0 && c.observedBytes == c.declaredBytes {
		c.complete = true
		c.publishBody()
	}
}

// publishBody snapshots the captured preview into info.Body exactly once.
// Cloning decouples info.Body's lifetime from the pooled buffer so release can
// happen without risking torn reads through aliased memory.
func (c *capturedRequestBody) publishBody() {
	if c == nil || c.info == nil || c.buffer == nil {
		return
	}
	if len(c.info.Body) > 0 {
		return
	}
	if c.buffer.Len() == 0 {
		return
	}
	c.info.Body = bytes.Clone(c.buffer.Bytes())
}

func (c *capturedRequestBody) applyToContext(ctx *gin.Context) {
	if ctx == nil {
		return
	}
	c.markClosed()
	if body := c.logBody(); len(body) > 0 {
		ctx.Set(requestBodyOverrideContextKey, body)
	}
}

// release returns the captured buffer to the pool. It is safe to call multiple
// times; subsequent calls are no-ops.
func (c *capturedRequestBody) release() {
	if c == nil || c.released {
		return
	}
	c.released = true
	if c.buffer != nil {
		releaseRequestBuffer(c.buffer)
		c.buffer = nil
	}
}

func (c *capturedRequestBody) logBody() []byte {
	if c == nil {
		return nil
	}
	if c.complete && !c.truncated {
		return nil
	}
	if c.observedBytes == 0 && c.declaredBytes < 0 {
		return nil
	}
	return []byte(c.summary())
}

func (c *capturedRequestBody) summary() string {
	captured := 0
	if c.buffer != nil {
		captured = c.buffer.Len()
	}
	summary := fmt.Sprintf(
		"[request body omitted] captured_bytes=%d observed_bytes=%d complete=%t truncated=%t",
		captured,
		c.observedBytes,
		c.complete,
		c.truncated,
	)
	if c.declaredBytes >= 0 {
		summary += fmt.Sprintf(" declared_bytes=%d", c.declaredBytes)
	}
	if c.hasher != nil {
		summary += " observed_sha256=" + hex.EncodeToString(c.hasher.Sum(nil))
	}
	return summary
}

type capturedRequestReadCloser struct {
	reader  io.Reader
	closer  io.Closer
	capture *capturedRequestBody
}

func (r *capturedRequestReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF && r.capture != nil {
		r.capture.markComplete()
	}
	return n, err
}

func (r *capturedRequestReadCloser) Close() error {
	if r.capture != nil {
		r.capture.markClosed()
	}
	return r.closer.Close()
}
