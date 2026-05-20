package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var validKiroRetryTestPayload = []byte(`{"conversationState":{"conversationId":"c","currentMessage":{"userInputMessage":{"content":"hi","modelId":"auto","origin":"AI_EDITOR"}}}}`)

// TestPermanentAuthErrorSatisfiesInterfaces is a compile-time guard that
// kiroRefreshPermanentError and kiroAuthScopedQuotaError still satisfy
// their respective interfaces.
func TestPermanentAuthErrorSatisfiesInterfaces(t *testing.T) {
	var _ cliproxyauth.PermanentAuthError = (*kiroRefreshPermanentError)(nil)
	var _ cliproxyauth.AuthScopedFailure = (*kiroAuthScopedQuotaError)(nil)
	var _ cliproxyauth.CredentialFailoverFailure = (*kiroAuthScopedQuotaError)(nil)
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
		firstPayload: validKiroRetryTestPayload,
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
		firstPayload: validKiroRetryTestPayload,
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

func TestDoKiroRequestWithFallbackRetry_RetriesMonthlyRequestCountThenFailover(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`))
	}))
	t.Cleanup(server.Close)

	exec := NewKiroExecutor(nil)
	prepared := &kiroPreparedRequest{
		firstPayload: validKiroRetryTestPayload,
		endpoints: []kiroEndpointConfig{
			{URL: server.URL, Origin: "AI_EDITOR", Name: "only"},
		},
	}

	_, _, err := exec.doKiroRequestWithFallbackRetry(context.Background(), &cliproxyauth.Auth{ID: "monthly-limit-test"}, prepared, "token")
	if err == nil {
		t.Fatal("expected 402 error, got nil")
	}
	if calls != kiroMonthlyRequestCountSameAuthRetries+1 {
		t.Fatalf("calls = %d, want %d", calls, kiroMonthlyRequestCountSameAuthRetries+1)
	}
	var scoped cliproxyauth.AuthScopedFailure
	if !errors.As(err, &scoped) || !scoped.IsAuthScopedFailure() {
		t.Fatalf("expected auth-scoped monthly limit error, got %T: %v", err, err)
	}
	var failover cliproxyauth.CredentialFailoverFailure
	if !errors.As(err, &failover) || !failover.IsCredentialFailoverFailure() {
		t.Fatalf("expected credential failover error, got %T: %v", err, err)
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

func TestShouldTryNextKiroEndpointFor_RuntimeForbiddenFallback(t *testing.T) {
	runtimeEndpoint := kiroEndpointConfig{Name: "KiroRuntime"}
	qEndpoint := kiroEndpointConfig{Name: "AmazonQ"}

	if !shouldTryNextKiroEndpointFor(runtimeEndpoint, statusErr{code: http.StatusForbidden, msg: "runtime endpoint unavailable"}) {
		t.Fatal("runtime 403 without credential signal should fall back to legacy endpoints")
	}
	if shouldTryNextKiroEndpointFor(qEndpoint, statusErr{code: http.StatusForbidden, msg: "runtime endpoint unavailable"}) {
		t.Fatal("non-runtime 403 must not fall back")
	}
	if shouldTryNextKiroEndpointFor(runtimeEndpoint, statusErr{code: http.StatusForbidden, msg: "invalid bearer token"}) {
		t.Fatal("runtime credential 403 must not be hidden by endpoint fallback")
	}
}
