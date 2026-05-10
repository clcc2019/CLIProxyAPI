package common

import (
	"strings"
	"testing"
	"time"
)

// TestExtractInferenceParamsHandlesMaxTokensSentinel locks in the `-1`
// handling: several clients (Cline, Continue) use `max_tokens: -1` as "use
// maximum" and we must clamp it to KiroMaxOutputTokens, not pass it through.
func TestExtractInferenceParamsHandlesMaxTokensSentinel(t *testing.T) {
	p := ExtractInferenceParams([]byte(`{"max_tokens":-1}`))
	if p.MaxTokens != KiroMaxOutputTokens {
		t.Fatalf("max_tokens=-1 should clamp to %d, got %d", KiroMaxOutputTokens, p.MaxTokens)
	}
}

// TestExtractInferenceParamsHasFlags verifies the Has* sentinels correctly
// discriminate "unset" from "set to zero". A request that explicitly asks
// for temperature: 0 (deterministic) must not be confused with one that
// omits the field, because the former needs to be forwarded and the latter
// must be dropped.
func TestExtractInferenceParamsHasFlags(t *testing.T) {
	p := ExtractInferenceParams([]byte(`{"temperature":0,"top_p":0}`))
	if !p.HasTemperature {
		t.Fatalf("HasTemperature should be true when temperature=0 explicitly")
	}
	if !p.HasTopP {
		t.Fatalf("HasTopP should be true when top_p=0 explicitly")
	}
	p2 := ExtractInferenceParams([]byte(`{}`))
	if p2.HasTemperature || p2.HasTopP {
		t.Fatalf("Has* should be false when field omitted: %+v", p2)
	}
	if p2.HasAnyInferenceConfig() {
		t.Fatalf("HasAnyInferenceConfig should be false when nothing set")
	}
}

// TestInjectTimestampContextFormat verifies the banner is prepended and
// uses the expected date format. Changing the format risks breaking
// upstream log analysis, so the test pins the exact shape.
func TestInjectTimestampContextFormat(t *testing.T) {
	fixed := time.Date(2026, 5, 9, 22, 47, 0, 0, time.UTC)
	got := InjectTimestampContext("Existing system prompt.", fixed)
	if !strings.HasPrefix(got, "[Context: Current time is 2026-05-09 22:47:00 UTC]") {
		t.Fatalf("timestamp banner missing or malformed: %q", got)
	}
	if !strings.Contains(got, "Existing system prompt.") {
		t.Fatalf("original prompt lost: %q", got)
	}
	// Empty prompt: banner only, no trailing separator.
	got2 := InjectTimestampContext("", fixed)
	if strings.HasSuffix(got2, "\n") {
		t.Fatalf("empty-prompt banner must not carry trailing newline: %q", got2)
	}
}

// TestAppendSystemHintEmptyCases guards against two regressions: injecting
// an empty hint must not add a stray newline; prepending to an empty prompt
// must not leave a leading newline either.
func TestAppendSystemHintEmptyCases(t *testing.T) {
	if got := AppendSystemHint("Base", ""); got != "Base" {
		t.Fatalf("empty hint must be a no-op, got %q", got)
	}
	if got := AppendSystemHint("", "hint"); got != "hint" {
		t.Fatalf("empty prompt should return hint verbatim, got %q", got)
	}
	if got := AppendSystemHint("Base", "hint"); got != "Base\nhint" {
		t.Fatalf("join format mismatch, got %q", got)
	}
}

// TestPrependThinkingHintStructure pins the <thinking_mode> control tag
// format. Changes break Kiro's reasoning extraction upstream.
func TestPrependThinkingHintStructure(t *testing.T) {
	got := PrependThinkingHint("user prompt")
	if !strings.HasPrefix(got, "<thinking_mode>enabled</thinking_mode>") {
		t.Fatalf("thinking hint missing opening tag: %q", got)
	}
	if !strings.Contains(got, "<max_thinking_length>16000</max_thinking_length>") {
		t.Fatalf("thinking length missing: %q", got)
	}
	if !strings.Contains(got, "user prompt") {
		t.Fatalf("original prompt lost: %q", got)
	}
}

// TestBuildFallbackSystemPromptContentEmpty protects the invariant that
// whitespace-only prompts produce no fallback — Kiro rejects empty
// currentMessage.content; the builder must synthesise real content instead.
func TestBuildFallbackSystemPromptContentEmpty(t *testing.T) {
	if got := BuildFallbackSystemPromptContent(""); got != "" {
		t.Fatalf("empty prompt should produce empty content, got %q", got)
	}
	if got := BuildFallbackSystemPromptContent("   \n\t"); got != "" {
		t.Fatalf("whitespace-only prompt should produce empty content, got %q", got)
	}
	got := BuildFallbackSystemPromptContent("Be concise.")
	if !strings.Contains(got, "--- SYSTEM PROMPT ---") || !strings.Contains(got, "--- END SYSTEM PROMPT ---") {
		t.Fatalf("fencing missing from fallback: %q", got)
	}
}

// TestNormalizeOriginCoverage covers every arm of the switch. Quota routing
// breaks if this mapping drifts; the test is intentionally exhaustive.
func TestNormalizeOriginCoverage(t *testing.T) {
	cases := map[string]string{
		"KIRO_CLI":       "CLI",
		"AMAZON_Q":       "CLI",
		"KIRO_AI_EDITOR": "AI_EDITOR",
		"KIRO_IDE":       "AI_EDITOR",
		"CLI":            "CLI",       // already canonical
		"AI_EDITOR":      "AI_EDITOR", // already canonical
		"":               "",          // pass-through
		"UNKNOWN":        "UNKNOWN",   // pass-through
	}
	for in, want := range cases {
		if got := NormalizeOrigin(in); got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}
