package executor

import (
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

	normal := []byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"normal answer"}]},"output_index":1}`)
	if codexShouldSuppressUsageWarningEvent(codexEventOutputItemDone, normal) {
		t.Fatal("normal assistant output should not be suppressed")
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
