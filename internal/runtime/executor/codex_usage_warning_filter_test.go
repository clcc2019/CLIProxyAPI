package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const codexUsageLimitHeadsUpText = "⚠ Heads up, you have less than 10% of your 5h limit left. Run /status for a breakdown."

func TestCodexShouldSuppressUsageWarningEvent(t *testing.T) {
	payload := []byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + codexUsageLimitHeadsUpText + `"}]},"output_index":1}`)

	if !codexShouldSuppressUsageWarningEvent(codexEventOutputItemDone, payload) {
		t.Fatal("expected Codex usage warning output item to be suppressed")
	}

	escapedStatus := []byte(`{"type":"response.output_text.delta","delta":"Heads up, you have less than 10% of your 5h limit left. Run \/status for a breakdown."}`)
	if !codexShouldSuppressUsageWarningEvent(codexEventOutputTextDelta, escapedStatus) {
		t.Fatal("expected Codex usage warning text delta with escaped /status to be suppressed")
	}

	textDone := []byte(`{"type":"response.output_text.done","item_id":"msg-warning","text":"` + codexUsageLimitHeadsUpText + `"}`)
	if !codexShouldSuppressUsageWarningEvent(codexEventOutputTextDone, textDone) {
		t.Fatal("expected Codex usage warning output text done event to be suppressed")
	}

	itemAdded := []byte(`{"type":"response.output_item.added","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + codexUsageLimitHeadsUpText + `"}]},"output_index":1}`)
	if !codexShouldSuppressUsageWarningEvent(codexEventOutputItemAdded, itemAdded) {
		t.Fatal("expected Codex usage warning output item added event to be suppressed")
	}

	contentPart := []byte(`{"type":"response.content_part.done","item_id":"msg-warning","part":{"type":"output_text","text":"` + codexUsageLimitHeadsUpText + `"}}`)
	if !codexShouldSuppressUsageWarningEvent(codexEventContentPartDone, contentPart) {
		t.Fatal("expected Codex usage warning content part event to be suppressed")
	}

	normal := []byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"normal answer"}]},"output_index":1}`)
	if codexShouldSuppressUsageWarningEvent(codexEventOutputItemDone, normal) {
		t.Fatal("normal assistant output should not be suppressed")
	}
}

func TestCodexUsageWarningStreamFilterDropsSplitUsageWarning(t *testing.T) {
	filter := newCodexUsageWarningStreamFilter()
	parts := []string{
		"⚠ Heads up, you have ",
		"less than 10% of your ",
		"5h limit left. Run /status for a breakdown.",
	}
	for _, part := range parts {
		payload := []byte(`{"type":"response.output_text.delta","item_id":"msg-warning","delta":"` + part + `"}`)
		if got := filter.Filter(codexEventOutputTextDelta, payload); len(got) != 0 {
			t.Fatalf("split usage warning event was forwarded: %#v", got)
		}
	}
}

func TestCodexUsageWarningStreamFilterFlushesNonWarningPrefix(t *testing.T) {
	filter := newCodexUsageWarningStreamFilter()
	first := []byte(`{"type":"response.output_text.delta","item_id":"msg-real","delta":"Heads up, you have "}`)
	if got := filter.Filter(codexEventOutputTextDelta, first); len(got) != 0 {
		t.Fatalf("first prefix event should be held, got %#v", got)
	}

	second := []byte(`{"type":"response.output_text.delta","item_id":"msg-real","delta":"a review waiting."}`)
	got := filter.Filter(codexEventOutputTextDelta, second)
	if len(got) != 2 {
		t.Fatalf("non-warning prefix should flush held event and current event, got %d", len(got))
	}
	if string(got[0].payload) != string(first) || string(got[1].payload) != string(second) {
		t.Fatalf("flushed events out of order: %#v", got)
	}
}

func TestCodexCompletedUsageWarningScrubAllowsRecordedOutputRecovery(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},"output_index":0}`))

	completed, ok := streamState.processEventDataWithType(codexEventCompleted, []byte(`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+codexUsageLimitHeadsUpText+`"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`), true)
	if !ok {
		t.Fatal("expected completed event")
	}
	if got := gjson.GetBytes(completed.data, "response.output.#").Int(); got != 1 {
		t.Fatalf("response.output length = %d, want 1; payload=%s", got, completed.data)
	}
	if got := gjson.GetBytes(completed.data, "response.output.0.content.0.text").String(); got != "ok" {
		t.Fatalf("recovered output text = %q, want ok; payload=%s", got, completed.data)
	}
}

func TestCodexUsageWarningFilterCaseInsensitive(t *testing.T) {
	text := "Heads Up, you have Less Than 10% Limit Left. Run /Status for a breakdown."
	if !codexTextLooksLikeUsageLimitWarning(text) {
		t.Fatal("expected mixed-case usage warning text to match")
	}

	payload := []byte(`{"type":"response.output_text.delta","delta":"Heads Up, you have Less Than 10% Limit Left. Run \/Status for a breakdown."}`)
	if !codexPayloadMayContainUsageLimitWarning(payload) {
		t.Fatal("expected mixed-case usage warning payload to pass prefilter")
	}
}

func TestCodexUsageWarningPrefixTextNormalizesWithoutLowercaseCopy(t *testing.T) {
	text := " ⚠ Heads Up, you have Less Than 10% of your 5h limit left. Run /Status"
	want := "heads up you have less than 10% of your 5h limit left run /status"
	if got := codexUsageWarningPrefixText(text); got != want {
		t.Fatalf("codexUsageWarningPrefixText() = %q, want %q", got, want)
	}
}

func TestCodexTextMayBeUsageLimitWarningPrefixMatchesNormalizedPrefix(t *testing.T) {
	tests := []string{
		" ⚠ Heads",
		"Heads Up, you have Less",
		"Heads Up, you have Less Than",
		"Heads Up, you have Less Than 10% of your 5h limit left",
		"not a usage warning",
		"",
	}
	const marker = "heads up you have less than"
	for _, text := range tests {
		normalized := codexUsageWarningPrefixText(text)
		want := normalized != "" && (strings.HasPrefix(marker, normalized) || strings.HasPrefix(normalized, marker))
		if got := codexTextMayBeUsageLimitWarningPrefix(text); got != want {
			t.Fatalf("codexTextMayBeUsageLimitWarningPrefix(%q) = %v, want %v (normalized %q)", text, got, want, normalized)
		}
	}
}

func BenchmarkCodexTextLooksLikeUsageLimitWarning(b *testing.B) {
	text := "Heads Up, you have Less Than 10% Limit Left. Run /Status for a breakdown."
	for b.Loop() {
		if !codexTextLooksLikeUsageLimitWarning(text) {
			b.Fatal("expected usage warning")
		}
	}
}

func BenchmarkCodexTextMayBeUsageLimitWarningPrefix(b *testing.B) {
	text := " ⚠ Heads Up, you have Less Than 10% of your 5h limit left. Run /Status"
	for b.Loop() {
		if !codexTextMayBeUsageLimitWarningPrefix(text) {
			b.Fatal("expected usage warning prefix")
		}
	}
}

func BenchmarkCodexUsageWarningPrefixText(b *testing.B) {
	text := " ⚠ Heads Up, you have Less Than 10% of your 5h limit left. Run /Status"
	for b.Loop() {
		if got := codexUsageWarningPrefixText(text); got == "" {
			b.Fatal("expected normalized prefix")
		}
	}
}

func BenchmarkCodexPayloadMayContainUsageLimitWarning(b *testing.B) {
	payload := []byte(`{"type":"response.output_text.delta","delta":"Heads Up, you have Less Than 10% Limit Left. Run \/Status for a breakdown."}`)
	for b.Loop() {
		if !codexPayloadMayContainUsageLimitWarning(payload) {
			b.Fatal("expected usage warning payload")
		}
	}
}
