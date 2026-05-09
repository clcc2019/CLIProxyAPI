package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// extractUsageFromSSE parses a "event: ...\ndata: {...}\n\n" frame and
// returns the decoded data map. Helper for the usage-emission tests.
func extractUsageFromSSE(t *testing.T, frame []byte) map[string]any {
	t.Helper()
	s := string(frame)
	idx := strings.Index(s, "data: ")
	if idx < 0 {
		t.Fatalf("SSE frame missing data line: %s", s)
	}
	line := s[idx+len("data: "):]
	if end := strings.Index(line, "\n"); end >= 0 {
		line = line[:end]
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("failed to decode SSE data: %v (raw: %s)", err, line)
	}
	return decoded
}

// nestedUsage walks the "message" -> "usage" path used by message_start,
// or the top-level "usage" used by message_delta / ping / non-stream response.
func nestedUsage(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	if msg, ok := decoded["message"].(map[string]any); ok {
		if u, ok := msg["usage"].(map[string]any); ok {
			return u
		}
	}
	if u, ok := decoded["usage"].(map[string]any); ok {
		return u
	}
	t.Fatalf("no usage in decoded frame: %+v", decoded)
	return nil
}

// TestBuildClaudeMessageStartEvent_EmitsFullUsage guards that
// BuildClaudeMessageStartEvent propagates every usage.Detail field Claude
// Code inspects — previously only input_tokens was emitted, hiding cache
// and reasoning counts from the client's live token counter.
func TestBuildClaudeMessageStartEvent_EmitsFullUsage(t *testing.T) {
	frame := BuildClaudeMessageStartEvent("claude-sonnet-4.5", usage.Detail{
		InputTokens:         123,
		OutputTokens:        45,
		CachedTokens:        20,
		CacheCreationTokens: 7,
		ReasoningTokens:     11,
	})
	u := nestedUsage(t, extractUsageFromSSE(t, frame))
	if got := toInt(u["input_tokens"]); got != 123 {
		t.Fatalf("input_tokens = %d, want 123", got)
	}
	if got := toInt(u["cache_read_input_tokens"]); got != 20 {
		t.Fatalf("cache_read_input_tokens = %d, want 20", got)
	}
	if got := toInt(u["cache_creation_input_tokens"]); got != 7 {
		t.Fatalf("cache_creation_input_tokens = %d, want 7", got)
	}
	if got := toInt(u["reasoning_tokens"]); got != 11 {
		t.Fatalf("reasoning_tokens = %d, want 11", got)
	}
}

// TestBuildClaudeMessageStartEvent_OmitsZeroFields verifies that cache and
// reasoning fields are not emitted when zero. Claude Code uses the presence
// of cache_read_input_tokens as the signal that prompt caching was probed;
// emitting a zero would confuse its "cache miss" heuristic.
func TestBuildClaudeMessageStartEvent_OmitsZeroFields(t *testing.T) {
	frame := BuildClaudeMessageStartEvent("claude-sonnet-4.5", usage.Detail{
		InputTokens: 10,
	})
	u := nestedUsage(t, extractUsageFromSSE(t, frame))
	if _, ok := u["cache_read_input_tokens"]; ok {
		t.Fatalf("cache_read_input_tokens must be omitted when zero; got %+v", u)
	}
	if _, ok := u["cache_creation_input_tokens"]; ok {
		t.Fatalf("cache_creation_input_tokens must be omitted when zero")
	}
	if _, ok := u["reasoning_tokens"]; ok {
		t.Fatalf("reasoning_tokens must be omitted when zero")
	}
}

// TestBuildClaudeMessageDeltaEvent_EmitsFullUsage guards the delta path,
// which is where most non-stream clients read the final token tally.
func TestBuildClaudeMessageDeltaEvent_EmitsFullUsage(t *testing.T) {
	frame := BuildClaudeMessageDeltaEvent("end_turn", usage.Detail{
		InputTokens:         500,
		OutputTokens:        100,
		CachedTokens:        300,
		CacheCreationTokens: 50,
		ReasoningTokens:     40,
	})
	u := nestedUsage(t, extractUsageFromSSE(t, frame))
	if got := toInt(u["cache_read_input_tokens"]); got != 300 {
		t.Fatalf("cache_read_input_tokens = %d, want 300", got)
	}
	if got := toInt(u["cache_creation_input_tokens"]); got != 50 {
		t.Fatalf("cache_creation_input_tokens = %d, want 50", got)
	}
	if got := toInt(u["reasoning_tokens"]); got != 40 {
		t.Fatalf("reasoning_tokens = %d, want 40", got)
	}
}

// TestBuildClaudeResponse_EmitsFullUsage covers the non-streaming path where
// the raw response is handed directly to the client. Previously only input
// and output were present, stripping cache / reasoning data entirely.
func TestBuildClaudeResponse_EmitsFullUsage(t *testing.T) {
	raw := BuildClaudeResponse("hi", nil, "claude-sonnet-4.5", usage.Detail{
		InputTokens:         80,
		OutputTokens:        12,
		CachedTokens:        60,
		CacheCreationTokens: 5,
		ReasoningTokens:     3,
	}, "end_turn")
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	u, ok := decoded["usage"].(map[string]any)
	if !ok {
		t.Fatalf("response missing usage: %s", string(raw))
	}
	if got := toInt(u["cache_read_input_tokens"]); got != 60 {
		t.Fatalf("cache_read_input_tokens = %d, want 60", got)
	}
	if got := toInt(u["cache_creation_input_tokens"]); got != 5 {
		t.Fatalf("cache_creation_input_tokens = %d, want 5", got)
	}
	if got := toInt(u["reasoning_tokens"]); got != 3 {
		t.Fatalf("reasoning_tokens = %d, want 3", got)
	}
}

func toInt(v any) int64 {
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
