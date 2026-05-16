package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestConcurrencyLimitMiddleware_ZeroIsNoOp(t *testing.T) {
	router := gin.New()
	router.Use(ConcurrencyLimitMiddleware(0, 5))
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("iteration %d: got %d, want 200", i, w.Code)
		}
	}
}

func TestConcurrencyLimitMiddleware_RejectsExcess(t *testing.T) {
	const limit = 2
	router := gin.New()
	router.Use(ConcurrencyLimitMiddleware(limit, 7))

	gate := make(chan struct{})
	router.GET("/slow", func(c *gin.Context) {
		<-gate
		c.Status(http.StatusOK)
	})

	var (
		ok       atomic.Int32
		rejected atomic.Int32
		started  sync.WaitGroup
	)

	const total = 5
	started.Add(total)

	results := make(chan int, total)
	for i := 0; i < total; i++ {
		go func() {
			started.Done()
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/slow", nil)
			router.ServeHTTP(w, req)
			results <- w.Code
		}()
	}

	// Give the limiter time to admit `limit` and reject the rest.
	started.Wait()
	deadline := time.After(2 * time.Second)
	for rejected.Load() < total-limit {
		select {
		case code := <-results:
			switch code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusServiceUnavailable:
				rejected.Add(1)
			default:
				t.Fatalf("unexpected status %d", code)
			}
		case <-deadline:
			t.Fatalf("timeout waiting for rejections (ok=%d rejected=%d)", ok.Load(), rejected.Load())
		}
		// Re-check immediately to avoid the rejection branch ending the loop
		// before all admitted requests are still pending.
		if ok.Load()+rejected.Load() == total-limit+limit-limit {
			break
		}
	}

	close(gate)

	timeout := time.After(2 * time.Second)
	for ok.Load()+rejected.Load() < total {
		select {
		case code := <-results:
			switch code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusServiceUnavailable:
				rejected.Add(1)
			}
		case <-timeout:
			t.Fatalf("timeout waiting for admitted requests to drain (ok=%d rejected=%d)", ok.Load(), rejected.Load())
		}
	}

	if ok.Load() != int32(limit) {
		t.Fatalf("ok=%d, want %d", ok.Load(), limit)
	}
	if rejected.Load() != int32(total-limit) {
		t.Fatalf("rejected=%d, want %d", rejected.Load(), total-limit)
	}
}

func TestConcurrencyLimitMiddleware_ReleasesAfterCompletion(t *testing.T) {
	router := gin.New()
	router.Use(ConcurrencyLimitMiddleware(1, 0))
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < 50; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("iteration %d: got %d, want 200 (semaphore not released?)", i, w.Code)
		}
	}
}

func TestConcurrencyLimitMiddleware_SetsRetryAfter(t *testing.T) {
	const limit = 1
	router := gin.New()
	router.Use(ConcurrencyLimitMiddleware(limit, 12))

	gate := make(chan struct{})
	router.GET("/", func(c *gin.Context) {
		<-gate
		c.Status(http.StatusOK)
	})

	var (
		blocked  = make(chan struct{})
		rejected = make(chan *httptest.ResponseRecorder, 1)
	)

	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		close(blocked)
		router.ServeHTTP(w, req)
	}()

	<-blocked
	// Spin until the first request is actually inside the handler before firing
	// the second one; with a 1-slot semaphore this is a tight race otherwise.
	deadline := time.After(time.Second)
	for {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		router.ServeHTTP(w, req)
		if w.Code == http.StatusServiceUnavailable {
			rejected <- w
			break
		}
		select {
		case <-deadline:
			t.Fatal("never observed a rejection within 1s")
		default:
		}
	}

	close(gate)
	w := <-rejected
	if got := w.Header().Get("Retry-After"); got != "12" {
		t.Fatalf("Retry-After=%q, want %q", got, "12")
	}
}
