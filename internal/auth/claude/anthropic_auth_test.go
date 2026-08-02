package claude

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensWithRetry_429BlocksImmediateReplay(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
					Header:     http.Header{"Retry-After": []string{"60"}},
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected 429 refresh error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("expected status 429 in error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt after 429, got %d", got)
	}

	_, err = auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected immediate blocked refresh error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected blocked retry to avoid a second refresh call, got %d attempts", got)
	}
	if blockedUntil := claudeRefreshBlockedUntil("dummy_refresh_token"); !blockedUntil.After(time.Now()) {
		t.Fatalf("expected blocked-until timestamp to be set, got %v", blockedUntil)
	}
}

func TestRefreshTokens_DeduplicatesConcurrentRefresh(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				once.Do(func() { close(started) })
				<-release
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(`{
						"access_token":"new-access",
						"refresh_token":"new-refresh",
						"token_type":"Bearer",
						"expires_in":3600,
						"account":{"email_address":"shared@example.com"}
					}`)),
					Header:  make(http.Header),
					Request: req,
				}, nil
			}),
		},
	}

	results := make(chan *ClaudeTokenData, 2)
	errs := make(chan error, 2)
	runRefresh := func() {
		td, err := auth.RefreshTokens(context.Background(), "shared-refresh-token")
		results <- td
		errs <- err
	}

	go runRefresh()
	go runRefresh()

	<-started
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	tokenResults := make([]*ClaudeTokenData, 0, 2)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("expected refresh to succeed, got %v", err)
		}
		td := <-results
		if td == nil || td.AccessToken != "new-access" {
			t.Fatalf("expected refreshed access token, got %#v", td)
		}
		tokenResults = append(tokenResults, td)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 upstream refresh call, got %d", got)
	}
	tokenResults[0].AccessToken = "caller-mutation"
	if got := tokenResults[1].AccessToken; got != "new-access" {
		t.Fatalf("second caller observed shared token mutation: %q", got)
	}
}

func TestRefreshTokens_HonorsInFlightCancellation(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	started := make(chan struct{})
	requestCancelled := make(chan struct{})
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		close(requestCancelled)
		return nil, req.Context().Err()
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := auth.RefreshTokens(ctx, "claude-cancel-in-flight")
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RefreshTokens() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RefreshTokens() did not return after cancellation")
	}
	select {
	case <-requestCancelled:
	case <-time.After(time.Second):
		t.Fatal("refresh HTTP request did not observe cancellation")
	}
}

// A transient 5xx must not end the refresh: the retry loop should reach the
// eventual success. This is what the executor gains by calling
// RefreshTokensWithRetry instead of RefreshTokens.
func TestRefreshTokensWithRetry_RecoversFromTransientFailure(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if atomic.AddInt32(&calls, 1) == 1 {
					return &http.Response{
						StatusCode: http.StatusServiceUnavailable,
						Body:       io.NopCloser(strings.NewReader(`{"error":"temporarily unavailable"}`)),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(`{
						"access_token":"recovered-access",
						"refresh_token":"recovered-refresh",
						"token_type":"Bearer",
						"expires_in":3600,
						"account":{"email_address":"user@example.com"}
					}`)),
					Header:  make(http.Header),
					Request: req,
				}, nil
			}),
		},
	}

	td, err := auth.RefreshTokensWithRetry(context.Background(), "transient-token", 3)
	if err != nil {
		t.Fatalf("expected recovery after a transient failure, got %v", err)
	}
	if td == nil || td.AccessToken != "recovered-access" {
		t.Fatalf("unexpected token data: %+v", td)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (one failure, one success)", got)
	}
}

// Retrying must not weaken the non-retryable paths: a 429 arms the refresh
// block, which is itself non-retryable, so the loop must stop after one attempt
// rather than hammering an endpoint that just told us to back off.
func TestRefreshTokensWithRetry_StopsOnNonRetryable(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				header := make(http.Header)
				header.Set("Retry-After", "60")
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
					Header:     header,
					Request:    req,
				}, nil
			}),
		},
	}

	if _, err := auth.RefreshTokensWithRetry(context.Background(), "blocked-token", 3); err == nil {
		t.Fatal("expected the 429 to surface as an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (429 must not be retried)", got)
	}
}
