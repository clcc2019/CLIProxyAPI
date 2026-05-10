package executor

import (
	"context"
	"strings"
	"testing"

	kiroclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/claude"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// buildKiroStreamStateForTest constructs a state with minimal wiring for
// direct testing of the finalize / estimateMissingUsage paths. The output
// channel is large enough that emit won't block.
func buildKiroStreamStateForTest(t *testing.T, kiroReq []byte) (*kiroStreamState, chan cliproxyexecutor.StreamChunk) {
	t.Helper()
	out := make(chan cliproxyexecutor.StreamChunk, 64)
	state := newKiroStreamState(
		context.Background(),
		NewKiroExecutor(nil),
		out,
		sdktranslator.FormatClaude,
		"claude-sonnet-4.5",
		[]byte(`{"stream":true}`),
		kiroReq,
		nil, // reporter — nil to avoid coupling the usage manager
	)
	return state, out
}

// TestKiroStreamStateEstimateFillsMissingUsage exercises the finalize path
// where upstream supplied no tokens at all. The estimator must populate
// both input (from kiroReq) and output (from accumulated content) so the
// emitted message_delta carries non-zero counts — this is the primary
// behaviour change introduced by Batch 1.
func TestKiroStreamStateEstimateFillsMissingUsage(t *testing.T) {
	kiroReq := []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"Tell me about Go interfaces in detail."}}}}`)
	state, _ := buildKiroStreamStateForTest(t, kiroReq)
	state.accumulatedContent.WriteString("An interface in Go is a set of method signatures.")

	state.estimateMissingUsage()

	if state.totalUsage.InputTokens <= 0 {
		t.Fatalf("InputTokens should be estimated from request: %+v", state.totalUsage)
	}
	if state.totalUsage.OutputTokens <= 0 {
		t.Fatalf("OutputTokens should be estimated from accumulated content: %+v", state.totalUsage)
	}
	if state.totalUsage.TotalTokens != state.totalUsage.InputTokens+state.totalUsage.OutputTokens+state.totalUsage.ReasoningTokens {
		t.Fatalf("TotalTokens mismatch: %+v", state.totalUsage)
	}
}

func TestKiroStreamStateEstimateOutputKeepsReasoningInTotal(t *testing.T) {
	state, _ := buildKiroStreamStateForTest(t, []byte(`{}`))
	state.totalUsage = usage.Detail{InputTokens: 10, ReasoningTokens: 7, TotalTokens: 10}
	state.accumulatedContent.WriteString("streamed response text")

	state.estimateMissingUsage()

	componentTotal := state.totalUsage.InputTokens + state.totalUsage.OutputTokens + state.totalUsage.ReasoningTokens
	if state.totalUsage.OutputTokens <= 0 {
		t.Fatalf("OutputTokens should be estimated: %+v", state.totalUsage)
	}
	if state.totalUsage.TotalTokens != componentTotal {
		t.Fatalf("TotalTokens should include reasoning tokens: want %d, got %+v", componentTotal, state.totalUsage)
	}
}

// TestKiroStreamStateEstimateSkipsWhenUpstreamSupplied verifies the
// estimator does not clobber values already supplied by the upstream —
// otherwise a billing-correct count could be replaced by an estimate.
func TestKiroStreamStateEstimateSkipsWhenUpstreamSupplied(t *testing.T) {
	state, _ := buildKiroStreamStateForTest(t, []byte(`{}`))
	state.totalUsage = usage.Detail{InputTokens: 500, OutputTokens: 250, TotalTokens: 750}
	state.accumulatedContent.WriteString("response text")
	state.estimateMissingUsage()
	if state.totalUsage.InputTokens != 500 || state.totalUsage.OutputTokens != 250 {
		t.Fatalf("upstream values overwritten: %+v", state.totalUsage)
	}
}

// TestKiroStreamStateFinalizeSkipsOnFailure covers the guard that prevents
// finalize() from emitting content_block_stop / message_delta /
// message_stop when the stream failed — emitting them would confuse
// clients that saw the error chunk.
func TestKiroStreamStateFinalizeSkipsOnFailure(t *testing.T) {
	state, out := buildKiroStreamStateForTest(t, []byte(`{}`))
	state.streamFailed = true
	state.finalize()
	close(out)
	for chunk := range out {
		if chunk.Err != nil {
			continue
		}
		if len(chunk.Payload) > 0 {
			t.Fatalf("finalize() must not emit payloads after failure: %s", chunk.Payload)
		}
	}
}

// TestKiroStreamStateProcessEventRoutesContent exercises the happy path:
// an assistantResponseEvent with content delta opens a text block, emits
// a content_block_delta, and updates the accumulator.
func TestKiroStreamStateProcessEventRoutesContent(t *testing.T) {
	state, out := buildKiroStreamStateForTest(t, []byte(`{}`))
	state.ensureMessageStart()
	state.processEvent(parsedKiroEvent{content: "Hello world."})
	if !state.textBlockOpen {
		t.Fatalf("text block should be open after content event")
	}
	if state.accumulatedContent.String() != "Hello world." {
		t.Fatalf("accumulator mismatch: %q", state.accumulatedContent.String())
	}
	close(out)
	saw := false
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("unexpected error: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "Hello world.") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("stream did not emit content delta")
	}
}

// TestKiroStreamStateToolUseDeduplicated verifies processed IDs aren't
// re-emitted when the upstream retries a tool_use (occasionally happens
// when CodeWhisperer replays an event after a connection blip).
func TestKiroStreamStateToolUseDeduplicated(t *testing.T) {
	state, out := buildKiroStreamStateForTest(t, []byte(`{}`))
	state.ensureMessageStart()
	tu := kiroclaude.KiroToolUse{ToolUseID: "toolu_abc", Name: "shell", Input: map[string]any{"command": "ls"}}
	state.emitToolUseBlock(tu, true)
	before := len(state.emittedToolUses)
	state.emitToolUseBlock(tu, true) // duplicate, should be dropped
	if len(state.emittedToolUses) != before {
		t.Fatalf("duplicate tool_use was emitted: before=%d after=%d", before, len(state.emittedToolUses))
	}
	close(out)
}

// TestKiroStreamStateFinalizeInfersStopReason confirms the "no stop_reason
// from upstream" path defaults to "tool_use" when tools were emitted, or
// "end_turn" otherwise. Clients parse stop_reason to decide next-turn
// behaviour, so the inference must not drift.
func TestKiroStreamStateFinalizeInfersStopReason(t *testing.T) {
	// Case 1: tool_use present → stop_reason=tool_use
	state, out := buildKiroStreamStateForTest(t, []byte(`{}`))
	state.ensureMessageStart()
	state.hasToolUses = true
	state.finalize()
	close(out)
	if state.stopReason != "tool_use" {
		t.Fatalf("want stop_reason=tool_use, got %q", state.stopReason)
	}

	// Case 2: no tool_use → stop_reason=end_turn
	state2, out2 := buildKiroStreamStateForTest(t, []byte(`{}`))
	state2.ensureMessageStart()
	state2.finalize()
	close(out2)
	if state2.stopReason != "end_turn" {
		t.Fatalf("want stop_reason=end_turn, got %q", state2.stopReason)
	}
}

// TestExtractKiroUsageUnchangedByFlatten pins the flattening refactor: the
// output for a representative nested metadata payload must match what the
// old closure-based implementation produced. Stored values come from a
// hand-computed expected result for the known input.
func TestExtractKiroUsageUnchangedByFlatten(t *testing.T) {
	event := map[string]any{
		"messageMetadataEvent": map[string]any{
			"tokenUsage": map[string]any{
				"uncachedInputTokens":   float64(120),
				"cacheReadInputTokens":  float64(30),
				"cacheWriteInputTokens": float64(10),
				"outputTokens":          float64(45),
				"totalTokens":           float64(205),
			},
		},
	}
	detail := extractKiroUsage(event)
	if detail.InputTokens != 160 {
		t.Fatalf("InputTokens: want 160 (uncached+read+write), got %d", detail.InputTokens)
	}
	if detail.CachedTokens != 30 {
		t.Fatalf("CachedTokens: want 30, got %d", detail.CachedTokens)
	}
	if detail.CacheCreationTokens != 10 {
		t.Fatalf("CacheCreationTokens: want 10, got %d", detail.CacheCreationTokens)
	}
	if detail.OutputTokens != 45 {
		t.Fatalf("OutputTokens: want 45, got %d", detail.OutputTokens)
	}
	if detail.TotalTokens != 205 {
		t.Fatalf("TotalTokens: want 205, got %d", detail.TotalTokens)
	}
}

func TestExtractKiroUsageParsesTokenCountCacheAliases(t *testing.T) {
	event := map[string]any{
		"messageMetadataEvent": map[string]any{
			"tokenUsage": map[string]any{
				"uncachedInputTokenCount":   float64(100),
				"cacheReadInputTokenCount":  float64(55),
				"cacheWriteInputTokenCount": float64(12),
				"outputTokenCount":          float64(33),
				"totalTokenCount":           float64(200),
			},
		},
	}
	detail := extractKiroUsage(event)
	if detail.InputTokens != 167 {
		t.Fatalf("InputTokens: want 167 (uncached+read+write), got %d", detail.InputTokens)
	}
	if detail.CachedTokens != 55 {
		t.Fatalf("CachedTokens: want 55, got %d", detail.CachedTokens)
	}
	if detail.CacheCreationTokens != 12 {
		t.Fatalf("CacheCreationTokens: want 12, got %d", detail.CacheCreationTokens)
	}
	if detail.OutputTokens != 33 {
		t.Fatalf("OutputTokens: want 33, got %d", detail.OutputTokens)
	}
	if detail.TotalTokens != 200 {
		t.Fatalf("TotalTokens: want 200, got %d", detail.TotalTokens)
	}
}

func TestExtractKiroUsageParsesNestedTokenDetailsAliases(t *testing.T) {
	event := map[string]any{
		"usage": map[string]any{
			"inputTokenCount":  float64(150),
			"outputTokenCount": float64(20),
			"inputTokenDetails": map[string]any{
				"cachedInputTokenCount": float64(70),
			},
			"outputTokenDetails": map[string]any{
				"reasoningTokenCount": float64(9),
			},
		},
	}
	detail := extractKiroUsage(event)
	if detail.InputTokens != 150 {
		t.Fatalf("InputTokens: want 150, got %d", detail.InputTokens)
	}
	if detail.CachedTokens != 70 {
		t.Fatalf("CachedTokens: want 70, got %d", detail.CachedTokens)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("ReasoningTokens: want 9, got %d", detail.ReasoningTokens)
	}
	if detail.TotalTokens != 179 {
		t.Fatalf("TotalTokens: want 179, got %d", detail.TotalTokens)
	}
}
