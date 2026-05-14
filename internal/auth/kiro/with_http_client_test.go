package kiro

import (
	"net/http"
	"testing"
	"time"
)

// TestKiroAuthWithHTTPClientReplacesClient ensures the management quota path
// can swap the HTTP client used for upstream getUsageLimits / listAvailableModels
// requests, so callers can inject a proxy-aware client built with
// helps.NewProxyAwareHTTPClient.
func TestKiroAuthWithHTTPClientReplacesClient(t *testing.T) {
	original := &http.Client{Timeout: time.Second}
	auth := &KiroAuth{httpClient: original}

	custom := &http.Client{Timeout: 5 * time.Second}
	if got := auth.WithHTTPClient(custom); got != auth {
		t.Fatalf("WithHTTPClient should return the receiver, got %p want %p", got, auth)
	}
	if auth.client() != custom {
		t.Fatalf("client() = %p, want injected client %p", auth.client(), custom)
	}

	// Passing nil must not clear an already-configured client; the call is a
	// no-op so the management handler can invoke it unconditionally.
	auth.WithHTTPClient(nil)
	if auth.client() != custom {
		t.Fatalf("nil injection cleared the http client; got %p", auth.client())
	}

	// A nil receiver must not panic.
	var nilAuth *KiroAuth
	if got := nilAuth.WithHTTPClient(custom); got != nil {
		t.Fatalf("nil receiver should return nil, got %p", got)
	}
}
