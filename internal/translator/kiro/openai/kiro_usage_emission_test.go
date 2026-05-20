package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// TestBuildOpenAISSEUsage_EmitsCachedAndReasoning verifies that the
// streaming OpenAI usage frame surfaces prompt_tokens_details.cached_tokens
// and completion_tokens_details.reasoning_tokens when upstream populated
// those buckets. Before this fix, only prompt / completion / total were
// emitted, hiding Kiro cache hits from OpenAI-surface clients.
func TestBuildOpenAISSEUsage_EmitsCachedAndReasoning(t *testing.T) {
	state := &OpenAIStreamState{
		ResponseID: "chatcmpl-test",
		Created:    1,
		Model:      "claude-sonnet-4.5",
	}
	sse := BuildOpenAISSEUsage(state, usage.Detail{
		InputTokens:     200,
		OutputTokens:    50,
		CachedTokens:    80,
		ReasoningTokens: 12,
		TotalTokens:     262,
	})
	// Strip the "data: " prefix + trailing "\n\n" to decode.
	payload := strings.TrimPrefix(sse, "data: ")
	if idx := strings.Index(payload, "\n"); idx >= 0 {
		payload = payload[:idx]
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode SSE: %v (raw: %s)", err, payload)
	}
	u, ok := decoded["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage: %+v", decoded)
	}
	if got := intFromAny(u["prompt_tokens"]); got != 200 {
		t.Fatalf("prompt_tokens = %d, want 200", got)
	}
	if got := intFromAny(u["completion_tokens"]); got != 50 {
		t.Fatalf("completion_tokens = %d, want 50", got)
	}
	if got := intFromAny(u["total_tokens"]); got != 262 {
		t.Fatalf("total_tokens = %d, want 262", got)
	}
	// Nested details (OpenAI standard shape).
	details, ok := u["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_tokens_details missing")
	}
	if got := intFromAny(details["cached_tokens"]); got != 80 {
		t.Fatalf("cached_tokens = %d, want 80", got)
	}
	out, ok := u["completion_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("completion_tokens_details missing")
	}
	if got := intFromAny(out["reasoning_tokens"]); got != 12 {
		t.Fatalf("reasoning_tokens = %d, want 12", got)
	}
}

// TestBuildOpenAISSEUsage_OmitsDetailsWhenZero guards that the nested
// details objects are not emitted when upstream reported no cache / no
// reasoning tokens — clients should see "no details" rather than zeroed
// buckets that can be misinterpreted.
func TestBuildOpenAISSEUsage_OmitsDetailsWhenZero(t *testing.T) {
	state := &OpenAIStreamState{ResponseID: "id", Model: "m"}
	sse := BuildOpenAISSEUsage(state, usage.Detail{
		InputTokens:  10,
		OutputTokens: 5,
	})
	payload := strings.TrimPrefix(sse, "data: ")
	if idx := strings.Index(payload, "\n"); idx >= 0 {
		payload = payload[:idx]
	}
	var decoded map[string]any
	_ = json.Unmarshal([]byte(payload), &decoded)
	u, _ := decoded["usage"].(map[string]any)
	if _, ok := u["prompt_tokens_details"]; ok {
		t.Fatalf("prompt_tokens_details should be omitted when zero")
	}
	if _, ok := u["completion_tokens_details"]; ok {
		t.Fatalf("completion_tokens_details should be omitted when zero")
	}
}

// TestKiroUsageFromClaudeJSON_ParsesFullShape guards the single helper that
// the Kiro→OpenAI translator uses to read Claude-shaped usage. It must
// pick up every bucket we plumb through the executor (input / output /
// cache_read / cache_creation / reasoning) and synthesize total_tokens
// when upstream omits it.
func TestKiroUsageFromClaudeJSON_ParsesFullShape(t *testing.T) {
	usageJSON := gjson.Parse(`{
		"input_tokens": 100,
		"output_tokens": 25,
		"cache_read_input_tokens": 40,
		"cache_creation_input_tokens": 10,
		"reasoning_tokens": 6
	}`)
	detail := kiroUsageFromClaudeJSON(usageJSON)
	if detail.InputTokens != 100 {
		t.Fatalf("input = %d, want 100", detail.InputTokens)
	}
	if detail.OutputTokens != 25 {
		t.Fatalf("output = %d, want 25", detail.OutputTokens)
	}
	if detail.CachedTokens != 40 {
		t.Fatalf("cached = %d, want 40", detail.CachedTokens)
	}
	if detail.CacheCreationTokens != 10 {
		t.Fatalf("cache_creation = %d, want 10", detail.CacheCreationTokens)
	}
	if detail.ReasoningTokens != 6 {
		t.Fatalf("reasoning = %d, want 6", detail.ReasoningTokens)
	}
	// 100 + 25 + 6 = 131; upstream didn't supply total_tokens.
	if detail.TotalTokens != 131 {
		t.Fatalf("total = %d, want 131 (synthesized)", detail.TotalTokens)
	}
}

// TestKiroUsageFromClaudeJSON_RespectsUpstreamTotal verifies we trust a
// non-zero upstream total_tokens value over the synthesized sum — some
// providers bill differently than a naive add-up implies.
func TestKiroUsageFromClaudeJSON_RespectsUpstreamTotal(t *testing.T) {
	usageJSON := gjson.Parse(`{"input_tokens": 10, "output_tokens": 5, "total_tokens": 999}`)
	detail := kiroUsageFromClaudeJSON(usageJSON)
	if detail.TotalTokens != 999 {
		t.Fatalf("total = %d, want 999 (upstream-provided)", detail.TotalTokens)
	}
}

func intFromAny(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}
