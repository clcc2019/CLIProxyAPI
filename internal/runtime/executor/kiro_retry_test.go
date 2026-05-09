package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestPermanentAuthErrorSatisfiesInterfaces is a compile-time guard that
// kiroRefreshPermanentError and kiroAuthScopedQuotaError still satisfy
// their respective interfaces.
func TestPermanentAuthErrorSatisfiesInterfaces(t *testing.T) {
	var _ cliproxyauth.PermanentAuthError = (*kiroRefreshPermanentError)(nil)
	var _ cliproxyauth.AuthScopedFailure = (*kiroAuthScopedQuotaError)(nil)
}

func TestDoKiroRequestWithFallbackRetry_DoesNotReplayTransientFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	exec := NewKiroExecutor(nil)
	prepared := &kiroPreparedRequest{
		firstPayload: []byte(`{}`),
		endpoints: []kiroEndpointConfig{
			{URL: server.URL, Origin: "AI_EDITOR", Name: "only"},
		},
	}

	_, _, err := exec.doKiroRequestWithFallbackRetry(context.Background(), &cliproxyauth.Auth{ID: "no-replay-test"}, prepared, "token")
	if err == nil {
		t.Fatal("expected upstream error, got nil")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestDoKiroRequestWithFallbackRetry_DoesNotAuthScopeThrottling429(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"__type":"ThrottlingException"}`))
	}))
	t.Cleanup(server.Close)

	exec := NewKiroExecutor(nil)
	prepared := &kiroPreparedRequest{
		firstPayload: []byte(`{}`),
		endpoints: []kiroEndpointConfig{
			{URL: server.URL, Origin: "AI_EDITOR", Name: "only"},
		},
	}

	_, _, err := exec.doKiroRequestWithFallbackRetry(context.Background(), &cliproxyauth.Auth{ID: "no-retry-test"}, prepared, "token")
	if err == nil {
		t.Fatal("expected 429 error, got nil")
	}
	var scoped cliproxyauth.AuthScopedFailure
	if errors.As(err, &scoped) && scoped.IsAuthScopedFailure() {
		t.Fatalf("throttling 429 must not be auth-scoped, got %T: %v", err, err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestShouldTryNextKiroEndpoint_DoesNotFallback429(t *testing.T) {
	if shouldTryNextKiroEndpoint(statusErr{code: http.StatusTooManyRequests, msg: "quota"}) {
		t.Fatal("429 must not fall back to another Kiro endpoint on the same auth")
	}
	if !shouldTryNextKiroEndpoint(statusErr{code: http.StatusServiceUnavailable, msg: "upstream unavailable"}) {
		t.Fatal("503 should still fall back to the next endpoint")
	}
}
