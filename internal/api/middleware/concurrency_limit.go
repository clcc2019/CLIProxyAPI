package middleware

import (
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/semaphore"
)

// ConcurrencyLimitMiddleware caps the number of concurrent in-flight requests
// served by the proxy. When the limit is reached, additional requests fail fast
// with HTTP 503 + a Retry-After hint instead of accumulating goroutines and
// upstream connections — the failure mode that turns a slow upstream into an
// OOM kill on the proxy.
//
// The limiter applies before the handler chain, so per-request resources
// (request body buffers, response writer buffers, request logger context) are
// not allocated for rejected calls.
//
// A maxInFlight value of 0 or negative disables the middleware (no-op).
func ConcurrencyLimitMiddleware(maxInFlight int, retryAfterSeconds int) gin.HandlerFunc {
	if maxInFlight <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	if retryAfterSeconds < 0 {
		retryAfterSeconds = 0
	}

	sem := semaphore.NewWeighted(int64(maxInFlight))
	var inFlight atomic.Int64

	retryAfter := strconv.Itoa(retryAfterSeconds)

	return func(c *gin.Context) {
		// Use TryAcquire so we shed load instantly rather than queueing inside
		// the proxy. The client (or its retry layer) is the right place to wait.
		if !sem.TryAcquire(1) {
			if retryAfterSeconds > 0 {
				c.Header("Retry-After", retryAfter)
			}
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error":         "proxy busy: concurrency limit reached",
				"in_flight":     inFlight.Load(),
				"max_in_flight": maxInFlight,
			})
			return
		}
		inFlight.Add(1)
		defer func() {
			inFlight.Add(-1)
			sem.Release(1)
		}()
		c.Next()
	}
}
