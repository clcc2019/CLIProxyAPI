package handlers

import (
	"net/http"
	"testing"
)

func TestFilterUpstreamHeaders_RemovesConnectionScopedHeaders(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "keep-alive, x-hop-a, x-hop-b")
	src.Add("Connection", "x-hop-c")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("X-Hop-A", "a")
	src.Set("X-Hop-B", "b")
	src.Set("X-Hop-C", "c")
	src.Set("X-Request-Id", "req-1")
	src.Set("Set-Cookie", "session=secret")

	filtered := FilterUpstreamHeaders(src)
	if filtered == nil {
		t.Fatalf("expected filtered headers, got nil")
	}

	requestID := filtered.Get("X-Request-Id")
	if requestID != "req-1" {
		t.Fatalf("expected X-Request-Id to be preserved, got %q", requestID)
	}

	blockedHeaderKeys := []string{
		"Connection",
		"Keep-Alive",
		"X-Hop-A",
		"X-Hop-B",
		"X-Hop-C",
		"Set-Cookie",
	}
	for _, key := range blockedHeaderKeys {
		value := filtered.Get(key)
		if value != "" {
			t.Fatalf("expected %s to be removed, got %q", key, value)
		}
	}
}

func TestFilterUpstreamHeaders_ReturnsNilWhenAllHeadersBlocked(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "x-hop-a")
	src.Set("X-Hop-A", "a")
	src.Set("Set-Cookie", "session=secret")

	filtered := FilterUpstreamHeaders(src)
	if filtered != nil {
		t.Fatalf("expected nil when all headers are filtered, got %#v", filtered)
	}
}

func TestFilterUpstreamHeaders_RemovesGatewayHeadersCaseInsensitive(t *testing.T) {
	src := http.Header{}
	src.Set("X-Litellm-Call-Id", "litellm")
	src.Set("Helicone-Request-Id", "helicone")
	src.Set("X-Portkey-Trace-Id", "portkey")
	src.Set("Cf-Aig-Metadata", "cf")
	src.Set("X-Kong-Request-Id", "kong")
	src.Set("X-Bt-Trace", "bt")
	src.Set("X-Request-Id", "req-1")

	filtered := FilterUpstreamHeaders(src)
	if filtered == nil {
		t.Fatalf("expected preserved headers, got nil")
	}
	if got := filtered.Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("expected X-Request-Id to be preserved, got %q", got)
	}

	for _, key := range []string{
		"X-Litellm-Call-Id",
		"Helicone-Request-Id",
		"X-Portkey-Trace-Id",
		"Cf-Aig-Metadata",
		"X-Kong-Request-Id",
		"X-Bt-Trace",
	} {
		if got := filtered.Get(key); got != "" {
			t.Fatalf("expected %s to be removed, got %q", key, got)
		}
	}
}

func BenchmarkFilterUpstreamHeaders(b *testing.B) {
	src := http.Header{}
	src.Add("Connection", "keep-alive, x-hop-a, x-hop-b")
	src.Add("Connection", "x-hop-c")
	src.Set("Content-Type", "application/json")
	src.Set("X-Hop-A", "a")
	src.Set("X-Hop-B", "b")
	src.Set("X-Hop-C", "c")
	src.Set("X-Litellm-Call-Id", "litellm")
	src.Set("Helicone-Request-Id", "helicone")
	src.Set("X-Request-Id", "req-1")
	src.Set("X-RateLimit-Remaining", "42")

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		filtered := FilterUpstreamHeaders(src)
		if filtered.Get("X-Request-Id") != "req-1" {
			b.Fatal("expected X-Request-Id to be preserved")
		}
	}
}

func BenchmarkFilterUpstreamHeadersAllBlocked(b *testing.B) {
	src := http.Header{}
	src.Add("Connection", "x-hop-a, x-hop-b")
	src.Set("X-Hop-A", "a")
	src.Set("X-Hop-B", "b")
	src.Set("Set-Cookie", "session=secret")
	src.Set("Content-Length", "42")
	src.Set("X-Litellm-Call-Id", "litellm")
	src.Set("Helicone-Request-Id", "helicone")

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if filtered := FilterUpstreamHeaders(src); filtered != nil {
			b.Fatalf("expected all headers to be blocked, got %#v", filtered)
		}
	}
}
