package openai

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func newTestStreamState(t *testing.T) *OpenAIStreamState {
	t.Helper()
	s := NewOpenAIStreamState("test-model")
	// Stabilize fields the wire-format tests inspect.
	s.ResponseID = "chatcmpl-fixed-id"
	s.Created = 1700000000
	return s
}

func TestBuildOpenAISSETextDelta_WireFormat(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		isFirst bool
	}{
		{"plain_first", "hello", true},
		{"plain_subsequent", " world", false},
		{"with_quote_and_backslash", `say "hi"\n nope`, false},
		{"unicode", "你好 🌏", false},
		{"control_chars", "line1\nline2\ttab", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newTestStreamState(t)
			state.HasSentFirstChunk = !tc.isFirst

			payload := BuildOpenAISSETextDelta(state, tc.text)

			var obj map[string]any
			if err := json.Unmarshal([]byte(payload), &obj); err != nil {
				t.Fatalf("not valid JSON: %v\npayload=%q", err, payload)
			}
			if obj["object"] != "chat.completion.chunk" {
				t.Fatalf("object=%v", obj["object"])
			}
			if obj["model"] != "test-model" {
				t.Fatalf("model=%v", obj["model"])
			}
			if obj["id"] != "chatcmpl-fixed-id" {
				t.Fatalf("id=%v", obj["id"])
			}
			if int64(obj["created"].(float64)) != 1700000000 {
				t.Fatalf("created=%v", obj["created"])
			}

			choices := obj["choices"].([]any)
			if len(choices) != 1 {
				t.Fatalf("choices len=%d", len(choices))
			}
			choice := choices[0].(map[string]any)
			if choice["finish_reason"] != nil {
				t.Fatalf("finish_reason=%v, want nil", choice["finish_reason"])
			}
			delta := choice["delta"].(map[string]any)
			if delta["content"] != tc.text {
				t.Fatalf("content=%q, want %q", delta["content"], tc.text)
			}
			if tc.isFirst {
				if delta["role"] != "assistant" {
					t.Fatalf("first-chunk role=%v, want assistant", delta["role"])
				}
				if !state.HasSentFirstChunk {
					t.Fatal("HasSentFirstChunk should flip to true")
				}
			} else {
				if _, ok := delta["role"]; ok {
					t.Fatal("non-first chunk should not include role")
				}
			}
		})
	}
}

func TestBuildOpenAISSEReasoningDelta_WireFormat(t *testing.T) {
	state := newTestStreamState(t)
	state.HasSentFirstChunk = true
	payload := BuildOpenAISSEReasoningDelta(state, "thinking step 1")

	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	choice := obj["choices"].([]any)[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	if delta["reasoning_content"] != "thinking step 1" {
		t.Fatalf("reasoning_content=%q", delta["reasoning_content"])
	}
	if _, ok := delta["content"]; ok {
		t.Fatal("reasoning chunk should not include content field")
	}
}

func TestBuildOpenAISSEFinish_WireFormat(t *testing.T) {
	t.Run("with_reason", func(t *testing.T) {
		state := newTestStreamState(t)
		payload := BuildOpenAISSEFinish(state, "stop")

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		choice := obj["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "stop" {
			t.Fatalf("finish_reason=%v", choice["finish_reason"])
		}
		delta := choice["delta"].(map[string]any)
		if len(delta) != 0 {
			t.Fatalf("delta should be empty, got %v", delta)
		}
	})
	t.Run("empty_reason_emits_null", func(t *testing.T) {
		state := newTestStreamState(t)
		payload := BuildOpenAISSEFinish(state, "")

		// Must contain literal `"finish_reason":null` per OpenAI streaming
		// contract; reflection-marshal would have produced the same.
		if !strings.Contains(payload, `"finish_reason":null`) {
			t.Fatalf("expected explicit null finish_reason; payload=%s", payload)
		}
	})
}

func TestBuildOpenAISSEFirstChunk_WireFormat(t *testing.T) {
	state := newTestStreamState(t)
	if state.HasSentFirstChunk {
		t.Fatal("fresh state should have HasSentFirstChunk == false")
	}
	payload := BuildOpenAISSEFirstChunk(state)

	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	choice := obj["choices"].([]any)[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	if delta["role"] != "assistant" {
		t.Fatalf("role=%v", delta["role"])
	}
	if delta["content"] != "" {
		t.Fatalf("content=%q, want empty", delta["content"])
	}
	if !state.HasSentFirstChunk {
		t.Fatal("HasSentFirstChunk must flip to true")
	}
}

func TestBuildOpenAISSETextDelta_PreservesUTF8(t *testing.T) {
	state := newTestStreamState(t)
	state.HasSentFirstChunk = true
	payload := BuildOpenAISSETextDelta(state, "漢字 αβγ 🚀")
	if !utf8.ValidString(payload) {
		t.Fatal("payload is not valid UTF-8")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	delta := obj["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["content"] != "漢字 αβγ 🚀" {
		t.Fatalf("content=%q", delta["content"])
	}
}

func TestBuildOpenAISSETextDelta_PooledBufferIsolated(t *testing.T) {
	state := newTestStreamState(t)
	state.HasSentFirstChunk = true

	first := BuildOpenAISSETextDelta(state, "aaaa")
	second := BuildOpenAISSETextDelta(state, "bbbbbbbbbbbbbbbbbb")

	var obj1, obj2 map[string]any
	_ = json.Unmarshal([]byte(first), &obj1)
	_ = json.Unmarshal([]byte(second), &obj2)
	delta1 := obj1["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	delta2 := obj2["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta1["content"] != "aaaa" {
		t.Fatalf("first content=%q (clobbered?)", delta1["content"])
	}
	if delta2["content"] != "bbbbbbbbbbbbbbbbbb" {
		t.Fatalf("second content=%q", delta2["content"])
	}
}

// Benchmarks: run with `-bench=BenchmarkBuildOpenAISSE -benchmem` to compare
// against pre-optimization baseline.

func BenchmarkBuildOpenAISSETextDelta(b *testing.B) {
	state := NewOpenAIStreamState("gpt-5")
	state.HasSentFirstChunk = true
	const sample = "this is a representative streaming text chunk with some words"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildOpenAISSETextDelta(state, sample)
	}
}

func BenchmarkBuildOpenAISSEFinish(b *testing.B) {
	state := NewOpenAIStreamState("gpt-5")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildOpenAISSEFinish(state, "stop")
	}
}
