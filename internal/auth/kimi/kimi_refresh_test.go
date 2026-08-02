package kimi

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

// countingTransport returns a fixed status and records how many round trips
// were made, so tests can assert that permanent failures are not replayed.
type countingTransport struct {
	status int
	calls  atomic.Int32
}

type kimiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kimiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return &http.Response{
		StatusCode: t.status,
		Body:       io.NopCloser(strings.NewReader(`{"error":"nope"}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestRefreshErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		err           *RefreshError
		wantPermanent bool
		wantStatus    int
	}{
		{"401 rejected", &RefreshError{statusCode: http.StatusUnauthorized, permanent: true}, true, 401},
		{"403 rejected", &RefreshError{statusCode: http.StatusForbidden, permanent: true}, true, 403},
		{"500 transient", &RefreshError{statusCode: http.StatusInternalServerError, body: "boom"}, false, 500},
		{"429 transient", &RefreshError{statusCode: http.StatusTooManyRequests}, false, 429},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.IsPermanentAuthError(); got != tt.wantPermanent {
				t.Errorf("IsPermanentAuthError() = %v, want %v", got, tt.wantPermanent)
			}
			if got := tt.err.StatusCode(); got != tt.wantStatus {
				t.Errorf("StatusCode() = %d, want %d", got, tt.wantStatus)
			}
			if tt.err.Error() == "" {
				t.Error("Error() is empty")
			}
		})
	}

	// A 403 must be reachable through errors.As, since that is how the
	// conductor decides to park rather than retry. Before RefreshError existed
	// this only worked for 401, and only by matching the literal text.
	var target *RefreshError
	forbidden := error(&RefreshError{statusCode: http.StatusForbidden, permanent: true})
	if !errors.As(forbidden, &target) || !target.IsPermanentAuthError() {
		t.Error("403 not reachable as a permanent RefreshError")
	}

	// Nil receiver must not panic.
	var nilErr *RefreshError
	if nilErr.Error() != "" || nilErr.StatusCode() != 0 || nilErr.IsPermanentAuthError() {
		t.Error("nil RefreshError misbehaves")
	}
}

// A permanent rejection must not be retried: replaying a revoked refresh token
// only burns quota.
func TestRefreshTokenWithRetryStopsOnPermanentRejection(t *testing.T) {
	rt := &countingTransport{status: http.StatusForbidden}
	client := &TokenRefreshClient{httpClient: &http.Client{Transport: rt}}

	_, err := client.RefreshTokenWithRetry(context.Background(), "revoked", 3)
	if err == nil {
		t.Fatal("expected the 403 to surface as an error")
	}
	var refreshErr *RefreshError
	if !errors.As(err, &refreshErr) || !refreshErr.IsPermanentAuthError() {
		t.Fatalf("permanent classification lost: %T: %v", err, err)
	}
	if calls := rt.calls.Load(); calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (403 must not be retried)", calls)
	}
}

// The backoff must observe cancellation rather than sitting through the
// remaining schedule.
func TestRefreshTokenWithRetryHonorsContextCancellation(t *testing.T) {
	rt := &countingTransport{status: http.StatusInternalServerError}
	client := &TokenRefreshClient{httpClient: &http.Client{Transport: rt}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.RefreshTokenWithRetry(ctx, "tok", 5); err == nil {
		t.Fatal("expected an error")
	}
	if calls := rt.calls.Load(); calls > 1 {
		t.Errorf("upstream calls = %d; cancelled context should stop after the first attempt", calls)
	}
}

func TestRefreshTokenDeduplicatesConcurrentRefreshAcrossClients(t *testing.T) {
	var calls int32
	started := make(chan struct{})
	secondUpstream := make(chan struct{})
	release := make(chan struct{})
	var firstOnce sync.Once
	var secondOnce sync.Once

	transport := kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		callNumber := atomic.AddInt32(&calls, 1)
		if callNumber == 1 {
			firstOnce.Do(func() { close(started) })
		} else {
			secondOnce.Do(func() { close(secondUpstream) })
		}
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600,"scope":"coding"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	clientA := &TokenRefreshClient{httpClient: &http.Client{Transport: transport}, deviceID: "device-a"}
	clientB := &TokenRefreshClient{httpClient: &http.Client{Transport: transport}, deviceID: "device-b"}
	refreshToken := "kimi-singleflight-refresh-token"

	type refreshResult struct {
		token *KimiTokenData
		err   error
	}
	results := make(chan refreshResult, 2)
	refresh := func(client *TokenRefreshClient) {
		token, err := client.RefreshToken(context.Background(), refreshToken)
		results <- refreshResult{token: token, err: err}
	}

	go refresh(clientA)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first refresh request")
	}

	secondLaunched := make(chan struct{})
	go func() {
		close(secondLaunched)
		refresh(clientB)
	}()
	<-secondLaunched
	select {
	case <-secondUpstream:
		t.Fatal("second concurrent refresh reached the upstream")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("refresh %d failed: %v", i, result.err)
		}
		if result.token == nil || result.token.AccessToken != "new-access" || result.token.RefreshToken != "new-refresh" {
			t.Fatalf("unexpected refresh %d result: %+v", i, result.token)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}
