package executor

import (
	"net/http"
	"testing"
)

func TestCodexHeaderGetKnownCanonicalFastPath(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Thread-ID", "thread-1")

	if got := codexHeaderGet(headers, "X-Thread-ID"); got != "thread-1" {
		t.Fatalf("codexHeaderGet canonical X-Thread-ID = %q, want thread-1", got)
	}
}

func TestCodexHeaderGetKnownDirectKeyWins(t *testing.T) {
	headers := http.Header{
		"X-Thread-ID": {"direct"},
		"X-Thread-Id": {"canonical"},
	}

	if got := codexHeaderGet(headers, "X-Thread-ID"); got != "direct" {
		t.Fatalf("codexHeaderGet direct X-Thread-ID = %q, want direct", got)
	}
}

func TestCodexHeaderGetKnownNonStandardKey(t *testing.T) {
	headers := http.Header{}
	headers.Set("Session_id", "session-1")

	if got := codexHeaderGet(headers, "Session_id"); got != "session-1" {
		t.Fatalf("codexHeaderGet Session_id = %q, want session-1", got)
	}
}

func TestCodexHeaderGetKnownLowercaseCanonical(t *testing.T) {
	headers := http.Header{}
	headers.Set(codexHeaderResponsesAPIIncludeTimingMetrics, "true")

	if got := codexHeaderGet(headers, codexHeaderResponsesAPIIncludeTimingMetrics); got != "true" {
		t.Fatalf("codexHeaderGet include timing metrics = %q, want true", got)
	}
}

func TestCodexHeaderGetUnknownFallsBackToHeaderGet(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Unknown-ID", "unknown")

	if got := codexHeaderGet(headers, "X-Unknown-ID"); got != "unknown" {
		t.Fatalf("codexHeaderGet unknown header = %q, want unknown", got)
	}
}

func TestCodexEnsureHeaderPreservesSourcePriority(t *testing.T) {
	target := http.Header{"X-Codex-Beta-Features": {"target"}}
	source := http.Header{"X-Codex-Beta-Features": {"source"}}

	codexEnsureHeader(target, source, "X-Codex-Beta-Features", "fallback")

	if got := codexHeaderGet(target, "X-Codex-Beta-Features"); got != "source" {
		t.Fatalf("codexEnsureHeader target value = %q, want source", got)
	}
}

func TestCodexEnsureHeaderUsesDefaultWhenMissing(t *testing.T) {
	target := http.Header{}

	codexEnsureHeader(target, nil, "X-Codex-Beta-Features", "fallback")

	if got := codexHeaderGet(target, "X-Codex-Beta-Features"); got != "fallback" {
		t.Fatalf("codexEnsureHeader default value = %q, want fallback", got)
	}
}
