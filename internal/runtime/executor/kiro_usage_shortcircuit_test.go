package executor

import (
	"testing"
)

// TestKiroEventHasUsageContainerPositive verifies the short-circuit returns
// true for every known container/flat-field shape; keeping this test
// comprehensive guards against a regression that would silently skip usage
// parsing for a new Kiro/Amazon Q field name.
func TestKiroEventHasUsageContainerPositive(t *testing.T) {
	cases := []map[string]any{
		{"input_tokens": float64(1)},
		{"inputTokens": float64(1)},
		{"output_tokens": float64(1)},
		{"outputTokens": float64(1)},
		{"prompt_tokens": float64(1)},
		{"completion_tokens": float64(1)},
		{"cacheReadInputTokenCount": float64(1)},
		{"cache_hit_input_tokens": float64(1)},
		{"cachedInputTokenCount": float64(1)},
		{"cacheWriteInputTokenCount": float64(1)},
		{"cache_creation_input_token_count": float64(1)},
		{"usage": map[string]any{"prompt_tokens": float64(1)}},
		{"tokenUsage": map[string]any{}},
		{"token_usage": map[string]any{}},
		{"messageMetadataEvent": map[string]any{}},
		{"metadataEvent": map[string]any{}},
		{"usageEvent": map[string]any{}},
		{"metadata": map[string]any{}},
		{"usage_metadata": map[string]any{}},
		{"response_metadata": map[string]any{}},
	}
	for i, tc := range cases {
		if !kiroEventHasUsageContainer(tc) {
			t.Errorf("case %d: expected true for %+v", i, tc)
		}
	}
}

// TestKiroEventHasUsageContainerNegative verifies that the common
// content/tool_use frames are correctly rejected. A false positive here
// would force the recursive walker to run for every stream frame.
func TestKiroEventHasUsageContainerNegative(t *testing.T) {
	cases := []map[string]any{
		nil,
		{},
		{"assistantResponseEvent": map[string]any{"content": "text"}},
		{"toolUseEvent": map[string]any{"toolUseId": "abc", "name": "sh"}},
		{"reasoningContentEvent": map[string]any{"content": "thinking"}},
		{"stopReason": "end_turn"},
		{"content": "some delta"},
	}
	for i, tc := range cases {
		if kiroEventHasUsageContainer(tc) {
			t.Errorf("case %d: expected false for %+v", i, tc)
		}
	}
}

// TestExtractKiroUsageShortCircuitReturnsZero confirms the short-circuit
// returns an all-zero usage.Detail when the event obviously lacks usage —
// the caller then knows to rely on the local estimator.
func TestExtractKiroUsageShortCircuitReturnsZero(t *testing.T) {
	event := map[string]any{
		"assistantResponseEvent": map[string]any{"content": "hello"},
	}
	detail := extractKiroUsage(event)
	if detail.InputTokens != 0 || detail.OutputTokens != 0 || detail.TotalTokens != 0 {
		t.Fatalf("expected all-zero detail for non-usage event, got %+v", detail)
	}
}
