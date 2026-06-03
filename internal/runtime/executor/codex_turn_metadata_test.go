package executor

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCodexBuildTurnMetadataHeaderKeepsBaseFields(t *testing.T) {
	header := codexBuildTurnMetadataHeader(codexTurnRequestKind, "session-1", "thread-1", "", "", "", "", "turn-1", codexDefaultSandboxTag, "window-1", 1700000000123)

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

func TestCodexBuildTurnMetadataHeaderIncludesOfficialLineageFields(t *testing.T) {
	header := codexBuildTurnMetadataHeader(codexTurnRequestKind, "session-1", "thread-1", "fork-1", "parent-1", "review", "", "turn-1", codexDefaultSandboxTag, "window-1", 1700000000123)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(header), &parsed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for key, want := range map[string]string{
		"forked_from_thread_id": "fork-1",
		"parent_thread_id":      "parent-1",
		"subagent_kind":         "review",
	} {
		if got, _ := parsed[key].(string); got != want {
			t.Fatalf("%s = %q, want %q in %s", key, got, want, header)
		}
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
		forkedFromThreadID:     "fork-1",
		parentThreadID:         "parent-1",
		subagentKind:           "review",
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
		"request_kind":          codexTurnRequestKind,
		"session_id":            "session-1",
		"thread_id":             "thread-1",
		"forked_from_thread_id": "fork-1",
		"parent_thread_id":      "parent-1",
		"subagent_kind":         "review",
		"turn_id":               "turn-client",
		"sandbox":               "danger-full-access",
		"window_id":             "window-1",
	} {
		if got, _ := parsed[key].(string); got != want {
			t.Fatalf("%s = %q, want %q in %s", key, got, want, headers.Get(codexHeaderTurnMetadata))
		}
	}
	if got, _ := parsed["turn_started_at_unix_ms"].(float64); int64(got) != 1700000000123 {
		t.Fatalf("turn_started_at_unix_ms = %.0f, want %d", got, int64(1700000000123))
	}
}

func TestCodexEnsureTurnMetadataHeaderDerivesOfficialLineageDefaults(t *testing.T) {
	headers := http.Header{}
	source := http.Header{}
	source.Set(codexHeaderParentThreadID, "parent-1")
	source.Set("X-OpenAI-Subagent", "review")

	codexEnsureTurnMetadataHeader(headers, source, codexTurnMetadataDefaults{
		requestKind:            codexTurnRequestKind,
		sessionID:              "session-1",
		threadID:               "thread-1",
		turnID:                 "turn-1",
		sandbox:                codexDefaultSandboxTag,
		windowID:               "window-1",
		turnStartedAtUnixMilli: 1700000000123,
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(headers.Get(codexHeaderTurnMetadata)), &parsed); err != nil {
		t.Fatalf("turn metadata should be valid JSON: %v", err)
	}
	for key, want := range map[string]string{
		"parent_thread_id": "parent-1",
		"subagent_kind":    "review",
	} {
		if got, _ := parsed[key].(string); got != want {
			t.Fatalf("%s = %q, want %q in %s", key, got, want, headers.Get(codexHeaderTurnMetadata))
		}
	}
}

func TestCodexEnsureTurnMetadataHeaderMarksMemgenRequestAsMemory(t *testing.T) {
	headers := http.Header{}
	source := http.Header{}
	source.Set(codexHeaderMemgenRequest, "true")

	codexEnsureTurnMetadataHeader(headers, source, codexTurnMetadataDefaults{
		requestKind:            codexTurnRequestKind,
		sessionID:              "session-1",
		threadID:               "thread-1",
		turnID:                 "turn-1",
		sandbox:                codexDefaultSandboxTag,
		windowID:               "window-1",
		turnStartedAtUnixMilli: 1700000000123,
	})

	assertCodexTurnMetadataString(t, headers.Get(codexHeaderTurnMetadata), "request_kind", codexMemoryRequestKind)
}

func TestCodexMergeResponsesAPIClientMetadataKeepsReservedTurnFields(t *testing.T) {
	headers := http.Header{}
	headers.Set(codexHeaderTurnMetadata, codexBuildTurnMetadataHeader(
		codexTurnRequestKind,
		"session-1",
		"thread-1",
		"fork-1",
		"parent-1",
		"review",
		"user",
		"turn-1",
		codexDefaultSandboxTag,
		"window-1",
		1700000000123,
	))

	codexMergeResponsesAPIClientMetadataIntoTurnMetadataHeader(headers, map[string]string{
		"fiber_run_id":                            "fiber-123",
		"origin":                                  "cli",
		"workspace_kind":                          "project",
		"session_id":                              "client-session",
		"thread_id":                               "client-thread",
		"turn_id":                                 "client-turn",
		"turn_started_at_unix_ms":                 "client-start",
		"forked_from_thread_id":                   "client-fork",
		"parent_thread_id":                        "client-parent",
		"subagent_kind":                           "client-subagent",
		codexRequestKindMetadataPath:              "client-kind",
		codexCompactionMetadataPath:               "client-compaction",
		codexWindowIDMetadataPath:                 "client-window",
		codexClientMetadataInstallationID:         "install-1",
		codexWSClientMetadataTraceparent:          "trace-1",
		codexClientMetadataWSStreamRequestStartMS: "1234",
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(headers.Get(codexHeaderTurnMetadata)), &parsed); err != nil {
		t.Fatalf("turn metadata should be valid JSON: %v", err)
	}
	for key, want := range map[string]string{
		"fiber_run_id":               "fiber-123",
		"origin":                     "cli",
		"workspace_kind":             "project",
		"session_id":                 "session-1",
		"thread_id":                  "thread-1",
		"turn_id":                    "turn-1",
		"forked_from_thread_id":      "fork-1",
		"parent_thread_id":           "parent-1",
		"subagent_kind":              "review",
		codexRequestKindMetadataPath: codexTurnRequestKind,
		codexWindowIDMetadataPath:    "window-1",
	} {
		if got, _ := parsed[key].(string); got != want {
			t.Fatalf("%s = %q, want %q in %s", key, got, want, headers.Get(codexHeaderTurnMetadata))
		}
	}
	if got, _ := parsed["turn_started_at_unix_ms"].(float64); int64(got) != 1700000000123 {
		t.Fatalf("turn_started_at_unix_ms = %.0f, want %d", got, int64(1700000000123))
	}
	for _, key := range []string{
		codexCompactionMetadataPath,
		codexClientMetadataInstallationID,
		codexWSClientMetadataTraceparent,
		codexClientMetadataWSStreamRequestStartMS,
	} {
		if _, ok := parsed[key]; ok {
			t.Fatalf("%s should not be copied into turn metadata: %s", key, headers.Get(codexHeaderTurnMetadata))
		}
	}
}

func TestCodexMergeResponsesAPIClientMetadataDoesNotReplaceExistingCustomFields(t *testing.T) {
	headers := http.Header{}
	headers.Set(codexHeaderTurnMetadata, `{"session_id":"session-1","origin":"server"}`)

	codexMergeResponsesAPIClientMetadataIntoTurnMetadataHeader(headers, map[string]string{
		"origin":       "client",
		"fiber_run_id": "fiber-123",
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(headers.Get(codexHeaderTurnMetadata)), &parsed); err != nil {
		t.Fatalf("turn metadata should be valid JSON: %v", err)
	}
	if got, _ := parsed["origin"].(string); got != "server" {
		t.Fatalf("origin = %q, want server in %s", got, headers.Get(codexHeaderTurnMetadata))
	}
	if got, _ := parsed["fiber_run_id"].(string); got != "fiber-123" {
		t.Fatalf("fiber_run_id = %q, want fiber-123 in %s", got, headers.Get(codexHeaderTurnMetadata))
	}
}

func TestCodexEnsureCompactTurnMetadataHeaderAddsCompactionMetadata(t *testing.T) {
	headers := http.Header{}
	source := http.Header{}
	source.Set(codexHeaderCompactionTrigger, "manual")
	source.Set(codexHeaderCompactionReason, "user-requested")
	source.Set(codexHeaderCompactionPhase, "standalone turn")

	codexEnsureCompactTurnMetadataHeader(headers, source, codexTurnMetadataDefaults{
		sessionID:              "session-1",
		threadID:               "thread-1",
		turnID:                 "turn-1",
		sandbox:                codexDefaultSandboxTag,
		windowID:               "window-1",
		turnStartedAtUnixMilli: 1700000000123,
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(headers.Get(codexHeaderTurnMetadata)), &parsed); err != nil {
		t.Fatalf("turn metadata should be valid JSON: %v", err)
	}
	compaction, ok := parsed["compaction"].(map[string]any)
	if !ok {
		t.Fatalf("compaction metadata missing or wrong type in %s", headers.Get(codexHeaderTurnMetadata))
	}
	for key, want := range map[string]string{
		"trigger":        "manual",
		"reason":         "user_requested",
		"implementation": codexDefaultCompactionImplementation,
		"phase":          "standalone_turn",
		"strategy":       codexDefaultCompactionStrategy,
	} {
		if got, _ := compaction[key].(string); got != want {
			t.Fatalf("compaction.%s = %q, want %q in %s", key, got, want, headers.Get(codexHeaderTurnMetadata))
		}
	}
}

func TestCodexEnsureCompactTurnMetadataHeaderPreservesClientCompactionMetadata(t *testing.T) {
	headers := http.Header{}
	source := http.Header{}
	source.Set(codexHeaderTurnMetadata, `{"turn_id":"turn-1","compaction":{"implementation":"responses_compaction_v2","strategy":"prefix_compaction"}}`)
	source.Set(codexHeaderCompactionImpl, "responses-compact")

	codexEnsureCompactTurnMetadataHeader(headers, source, codexTurnMetadataDefaults{
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
	compaction, ok := parsed["compaction"].(map[string]any)
	if !ok {
		t.Fatalf("compaction metadata missing or wrong type in %s", headers.Get(codexHeaderTurnMetadata))
	}
	if got, _ := compaction["implementation"].(string); got != "responses_compaction_v2" {
		t.Fatalf("compaction.implementation = %q, want responses_compaction_v2", got)
	}
	if got, _ := compaction["strategy"].(string); got != "prefix_compaction" {
		t.Fatalf("compaction.strategy = %q, want prefix_compaction", got)
	}
}

func BenchmarkCodexBuildTurnMetadataHeader(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = codexBuildTurnMetadataHeader(codexTurnRequestKind, "session-1", "thread-1", "", "", "", "", "turn-1", codexDefaultSandboxTag, "window-1", 1700000000123)
	}
}
