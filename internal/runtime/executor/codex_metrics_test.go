package executor

import (
	"bytes"
	"strings"
	"testing"
)

// TestCodexMetricsBodyAndPromptHitsMiss exercises the two memo.get() paths and
// confirms that hit/miss counters move independently. Body and prompt memos
// are separate sets of counters so a hit on one must not count as a miss on
// the other.
func TestCodexMetricsBodyAndPromptHitsMiss(t *testing.T) {
	ResetCodexMetrics()
	t.Cleanup(ResetCodexMetrics)

	opts := codexFinalUpstreamBodyOptions{requestKind: codexFinalUpstreamResponses, streamMode: codexStreamFieldTrue}
	// Use a zero-value memo so we don't pollute globals across tests.
	m := &codexFinalUpstreamBodyMemo{}
	input := []byte(`{"model":"gpt-5"}`)
	output := []byte(`{"model":"gpt-5","normalized":true}`)
	// First lookup misses (entry not yet inserted).
	if got := m.get("gpt-5", opts, input); got != nil {
		t.Fatalf("initial get should miss, got %q", got)
	}
	// Populate the entry and confirm the next get is a hit.
	m.set("gpt-5", opts, input, output)
	if got := m.get("gpt-5", opts, input); !bytes.Equal(got, output) {
		t.Fatalf("memo get returned %q, want %q", got, output)
	}

	snap := CodexMetrics()
	if snap.MemoBodyHit != 1 {
		t.Errorf("MemoBodyHit = %d, want 1", snap.MemoBodyHit)
	}
	if snap.MemoBodyMiss != 1 {
		t.Errorf("MemoBodyMiss = %d, want 1", snap.MemoBodyMiss)
	}
	if snap.MemoPromptHit != 0 || snap.MemoPromptMiss != 0 {
		t.Errorf("prompt memo counters leaked: hit=%d miss=%d", snap.MemoPromptHit, snap.MemoPromptMiss)
	}
}

// TestCodexMetricsTerminalAndCaptureCounters guards the aggregate-path counters
// so future refactors cannot silently drop observability hooks that operators
// depend on.
func TestCodexMetricsTerminalAndCaptureCounters(t *testing.T) {
	ResetCodexMetrics()
	t.Cleanup(ResetCodexMetrics)

	originalLimit := codexAggregateCapturedBodyMaxBytes
	codexAggregateCapturedBodyMaxBytes = 32
	defer func() { codexAggregateCapturedBodyMaxBytes = originalLimit }()

	big := strings.Repeat("x", 200)
	// response.incomplete + large body to force capture truncation.
	sse := strings.Join([]string{
		"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_big\",\"filler\":\"" + big + "\"}}",
		"",
		"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_big\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}",
		"",
	}, "\n") + "\n"

	_, err := collectCodexResponseAggregate(bytes.NewBufferString(sse), true)
	if err != nil {
		t.Fatalf("collectCodexResponseAggregate error: %v", err)
	}
	snap := CodexMetrics()
	if snap.TerminalIncomplete != 1 {
		t.Errorf("TerminalIncomplete = %d, want 1", snap.TerminalIncomplete)
	}
	if snap.CaptureTruncated == 0 {
		t.Errorf("CaptureTruncated = 0, want >= 1 (fired by truncation branch)")
	}
	if snap.TerminalFailed != 0 {
		t.Errorf("TerminalFailed leaked to %d", snap.TerminalFailed)
	}
}
