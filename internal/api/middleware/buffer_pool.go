// Package middleware — capture buffer pooling.
//
// The request/response capture paths allocate a bytes.Buffer per request. At
// high QPS that is a real source of GC pressure. We pool buffers in two size
// classes so the hot path reuses a pre-grown []byte instead of reallocating
// every time, while oversized buffers (that blew past their soft cap) are
// dropped on the floor to avoid retaining the long tail.
package middleware

import (
	"bytes"
	"sync"
)

const (
	// requestBufferSoftCap is the largest buffer we hand back to the pool.
	// The per-request limit is 1 MiB; we keep anything up to that size and
	// discard buffers that grew larger than the cap after reset.
	requestBufferSoftCap = 1 << 20 // 1 MiB

	// responseBufferSoftCap mirrors the response body limit.
	responseBufferSoftCap = 4 << 20 // 4 MiB
)

var (
	requestBufferPool = sync.Pool{
		New: func() any {
			return &bytes.Buffer{}
		},
	}

	responseBufferPool = sync.Pool{
		New: func() any {
			return &bytes.Buffer{}
		},
	}
)

// acquireRequestBuffer returns a reset *bytes.Buffer from the pool.
func acquireRequestBuffer() *bytes.Buffer {
	buf := requestBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// releaseRequestBuffer returns buf to the pool, dropping it if it outgrew the
// soft cap so the pool does not retain memory for outlier requests.
func releaseRequestBuffer(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if buf.Cap() > requestBufferSoftCap {
		return
	}
	buf.Reset()
	requestBufferPool.Put(buf)
}

// acquireResponseBuffer returns a reset *bytes.Buffer from the pool.
func acquireResponseBuffer() *bytes.Buffer {
	buf := responseBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// releaseResponseBuffer returns buf to the pool or drops it if oversized.
func releaseResponseBuffer(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if buf.Cap() > responseBufferSoftCap {
		return
	}
	buf.Reset()
	responseBufferPool.Put(buf)
}
