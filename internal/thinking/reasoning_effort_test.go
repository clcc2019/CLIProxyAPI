package thinking

import "testing"

func TestExtractReasoningEffortUsesSuffixOverBody(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning_effort":"low"}`), "openai", "gpt-5.4(high)")
	if got != "high" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "high")
	}
}

func TestExtractReasoningEffortConvertsBudgetToLevel(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"thinking":{"type":"enabled","budget_tokens":8192}}`), "claude", "claude-sonnet-4-5")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortSupportsOpenAIResponses(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning":{"effort":"medium"}}`), "openai-response", "gpt-5.4")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortMissingConfigIsEmpty(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai", "gpt-5.4")
	if got != "" {
		t.Fatalf("ExtractReasoningEffort() = %q, want empty", got)
	}
}

func TestExtractTranslatedReasoningEffortNormalizesClaudeAdaptiveEffort(t *testing.T) {
	got := ExtractTranslatedReasoningEffort([]byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":" XHIGH "}}`), "claude")
	if got != "xhigh" {
		t.Fatalf("ExtractTranslatedReasoningEffort() = %q, want xhigh", got)
	}
}

func TestNormalizeReasoningEffortValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: " ", want: ""},
		{name: "known mixed case", value: "\tAdaptive\r\n", want: "adaptive"},
		{name: "unknown lower fallback", value: "Custom-Effort", want: "custom-effort"},
	}

	for i := range tests {
		if got := normalizeReasoningEffortValue(tests[i].value); got != tests[i].want {
			t.Fatalf("%s: got %q, want %q", tests[i].name, got, tests[i].want)
		}
	}
}

func TestConvertLevelToBudgetMatchesMixedCaseLevels(t *testing.T) {
	got, ok := ConvertLevelToBudget("MeDiUm")
	if !ok {
		t.Fatal("ConvertLevelToBudget() ok = false")
	}
	if got != 8192 {
		t.Fatalf("ConvertLevelToBudget() = %d, want 8192", got)
	}

	if _, ok := ConvertLevelToBudget(" medium "); ok {
		t.Fatal("ConvertLevelToBudget() should not trim whitespace")
	}
}

func TestParseSuffixModesMatchMixedCaseWithoutTrimming(t *testing.T) {
	if got, ok := ParseSpecialSuffix("AuTo"); got != ModeAuto || !ok {
		t.Fatalf("ParseSpecialSuffix(AuTo) = %v, %t; want %v, true", got, ok, ModeAuto)
	}
	if got, ok := ParseLevelSuffix("XhIgH"); got != LevelXHigh || !ok {
		t.Fatalf("ParseLevelSuffix(XhIgH) = %q, %t; want %q, true", got, ok, LevelXHigh)
	}
	if _, ok := ParseLevelSuffix(" high "); ok {
		t.Fatal("ParseLevelSuffix should not trim whitespace")
	}
}

func BenchmarkParseLevelSuffixMixedCase(b *testing.B) {
	for b.Loop() {
		if got, ok := ParseLevelSuffix("XhIgH"); got != LevelXHigh || !ok {
			b.Fatalf("ParseLevelSuffix() = %q, %t", got, ok)
		}
	}
}

func TestMapToClaudeEffortMatchesMixedCaseLevels(t *testing.T) {
	tests := []struct {
		name        string
		level       string
		supportsMax bool
		want        string
		wantOK      bool
	}{
		{name: "minimal", level: " Minimal ", want: "low", wantOK: true},
		{name: "medium", level: "MeDiUm", want: "medium", wantOK: true},
		{name: "max supported", level: " XHIGH ", supportsMax: true, want: "max", wantOK: true},
		{name: "max clamped", level: "Max", want: "high", wantOK: true},
		{name: "auto", level: "AUTO", want: "high", wantOK: true},
		{name: "unknown", level: "ultra", wantOK: false},
	}

	for i := range tests {
		got, ok := MapToClaudeEffort(tests[i].level, tests[i].supportsMax)
		if ok != tests[i].wantOK {
			t.Fatalf("%s: ok = %t, want %t", tests[i].name, ok, tests[i].wantOK)
		}
		if got != tests[i].want {
			t.Fatalf("%s: got %q, want %q", tests[i].name, got, tests[i].want)
		}
	}
}

func BenchmarkConvertLevelToBudget(b *testing.B) {
	for b.Loop() {
		if got, ok := ConvertLevelToBudget("MeDiUm"); !ok || got != 8192 {
			b.Fatalf("ConvertLevelToBudget() = %d, %t", got, ok)
		}
	}
}

func BenchmarkMapToClaudeEffort(b *testing.B) {
	for b.Loop() {
		if got, ok := MapToClaudeEffort(" XHIGH ", true); !ok || got != "max" {
			b.Fatalf("MapToClaudeEffort() = %q, %t", got, ok)
		}
	}
}

func BenchmarkNormalizeReasoningEffortValue(b *testing.B) {
	for b.Loop() {
		if got := normalizeReasoningEffortValue(" XHIGH "); got != "xhigh" {
			b.Fatalf("normalizeReasoningEffortValue() = %q", got)
		}
	}
}
