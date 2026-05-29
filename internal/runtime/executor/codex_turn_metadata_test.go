package executor

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCodexBuildTurnMetadataHeaderKeepsBaseFields(t *testing.T) {
	header := codexBuildTurnMetadataHeader(codexTurnRequestKind, "session-1", "thread-1", "", "turn-1", codexDefaultSandboxTag, "window-1", 1700000000123)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(header), &parsed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got, _ := parsed["request_kind"].(string); got != codexTurnRequestKind {
		t.Fatalf("request_kind = %q, want %q", got, codexTurnRequestKind)
	}
	if got, _ := parsed["session_id"].(string); got != "session-1" {
		t.Fatalf("session_id = %q, want %q", got, "session-1")
	}
	if got, _ := parsed["thread_id"].(string); got != "thread-1" {
		t.Fatalf("thread_id = %q, want %q", got, "thread-1")
	}
	if got := parsed["thread_source"]; got != nil {
		t.Fatalf("thread_source = %#v, want nil", got)
	}
	if got, _ := parsed["turn_id"].(string); got != "turn-1" {
		t.Fatalf("turn_id = %q, want %q", got, "turn-1")
	}
	if got, _ := parsed["sandbox"].(string); got != codexDefaultSandboxTag {
		t.Fatalf("sandbox = %q, want %q", got, codexDefaultSandboxTag)
	}
	if got, _ := parsed["window_id"].(string); got != "window-1" {
		t.Fatalf("window_id = %q, want %q", got, "window-1")
	}
	if got, _ := parsed["turn_started_at_unix_ms"].(float64); int64(got) != 1700000000123 {
		t.Fatalf("turn_started_at_unix_ms = %.0f, want %d", got, int64(1700000000123))
	}
	if got := parsed["workspaces"]; got != nil {
		t.Fatalf("workspaces = %#v, want nil", got)
	}
}

func TestCodexEnsureTurnMetadataHeaderAugmentsClientHeader(t *testing.T) {
	headers := http.Header{}
	source := http.Header{}
	source.Set(codexHeaderTurnMetadata, `{"turn_id":"turn-client","sandbox":"danger-full-access"}`)

	codexEnsureTurnMetadataHeader(headers, source, codexTurnMetadataDefaults{
		requestKind:            codexTurnRequestKind,
		sessionID:              "session-1",
		threadID:               "thread-1",
		turnID:                 "turn-generated",
		sandbox:                codexDefaultSandboxTag,
		windowID:               "window-1",
		turnStartedAtUnixMilli: 1700000000123,
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(headers.Get(codexHeaderTurnMetadata)), &parsed); err != nil {
		t.Fatalf("turn metadata should be valid JSON: %v", err)
	}
	for key, want := range map[string]string{
		"request_kind": codexTurnRequestKind,
		"session_id":   "session-1",
		"thread_id":    "thread-1",
		"turn_id":      "turn-client",
		"sandbox":      "danger-full-access",
		"window_id":    "window-1",
	} {
		if got, _ := parsed[key].(string); got != want {
			t.Fatalf("%s = %q, want %q in %s", key, got, want, headers.Get(codexHeaderTurnMetadata))
		}
	}
	if got, _ := parsed["turn_started_at_unix_ms"].(float64); int64(got) != 1700000000123 {
		t.Fatalf("turn_started_at_unix_ms = %.0f, want %d", got, int64(1700000000123))
	}
}

func BenchmarkCodexBuildTurnMetadataHeader(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = codexBuildTurnMetadataHeader(codexTurnRequestKind, "session-1", "thread-1", "", "turn-1", codexDefaultSandboxTag, "window-1", 1700000000123)
	}
}
