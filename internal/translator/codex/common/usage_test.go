package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestCachedInputTokensRecognizesCanonicalAndFallbackFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
	}{
		{
			name: "chat details",
			raw:  `{"prompt_tokens_details":{"cached_tokens":11},"cached_input_tokens":22,"cache_read_input_tokens":33}`,
			want: 11,
		},
		{
			name: "responses details",
			raw:  `{"input_tokens_details":{"cached_tokens":22},"cached_input_tokens":33,"cache_read_input_tokens":44}`,
			want: 22,
		},
		{
			name: "official codex flat field",
			raw:  `{"cached_input_tokens":33,"cache_read_input_tokens":44}`,
			want: 33,
		},
		{
			name: "cache read fallback",
			raw:  `{"cache_read_input_tokens":44}`,
			want: 44,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CachedInputTokens(gjson.Parse(tc.raw))
			if !got.Exists() {
				t.Fatalf("CachedInputTokens() did not find a value")
			}
			if got.Int() != tc.want {
				t.Fatalf("CachedInputTokens() = %d, want %d", got.Int(), tc.want)
			}
		})
	}
}

func TestReasoningOutputTokensRecognizesCanonicalAndFallbackFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
	}{
		{
			name: "chat details",
			raw:  `{"completion_tokens_details":{"reasoning_tokens":5},"reasoning_output_tokens":6,"reasoning_tokens":7}`,
			want: 5,
		},
		{
			name: "responses details",
			raw:  `{"output_tokens_details":{"reasoning_tokens":6},"reasoning_output_tokens":7,"reasoning_tokens":8}`,
			want: 6,
		},
		{
			name: "official codex flat field",
			raw:  `{"reasoning_output_tokens":7,"reasoning_tokens":8}`,
			want: 7,
		},
		{
			name: "generic fallback",
			raw:  `{"reasoning_tokens":8}`,
			want: 8,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ReasoningOutputTokens(gjson.Parse(tc.raw))
			if !got.Exists() {
				t.Fatalf("ReasoningOutputTokens() did not find a value")
			}
			if got.Int() != tc.want {
				t.Fatalf("ReasoningOutputTokens() = %d, want %d", got.Int(), tc.want)
			}
		})
	}
}
