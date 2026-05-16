package common

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClaudeTextDeltaJSON_WireFormat(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		index int
	}{
		{"plain", "hello", 0},
		{"with_quote", `say "hi"`, 5},
		{"backslash", `path\to\file`, 1},
		{"unicode", "你好 🌏", 2},
		{"control_chars", "line1\nline2\ttab", 1},
		{"empty", "", 0},
		{"large_index", "x", 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := ClaudeTextDeltaJSON(tc.index, tc.text)
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("not valid JSON: %v\nraw=%s", err, raw)
			}
			if obj["type"] != "content_block_delta" {
				t.Fatalf("type=%v", obj["type"])
			}
			if int(obj["index"].(float64)) != tc.index {
				t.Fatalf("index=%v", obj["index"])
			}
			delta := obj["delta"].(map[string]any)
			if delta["type"] != "text_delta" {
				t.Fatalf("delta.type=%v", delta["type"])
			}
			if delta["text"] != tc.text {
				t.Fatalf("delta.text=%q, want %q", delta["text"], tc.text)
			}
		})
	}
}

func TestClaudeThinkingDeltaJSON_WireFormat(t *testing.T) {
	raw := ClaudeThinkingDeltaJSON(7, `step "1"`)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	delta := obj["delta"].(map[string]any)
	if delta["type"] != "thinking_delta" {
		t.Fatalf("delta.type=%v", delta["type"])
	}
	if delta["thinking"] != `step "1"` {
		t.Fatalf("delta.thinking=%q", delta["thinking"])
	}
}

func TestClaudeInputJSONDeltaJSON_WireFormat(t *testing.T) {
	raw := ClaudeInputJSONDeltaJSON(3, `{"k":"v"}`)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	delta := obj["delta"].(map[string]any)
	if delta["type"] != "input_json_delta" {
		t.Fatalf("delta.type=%v", delta["type"])
	}
	if delta["partial_json"] != `{"k":"v"}` {
		t.Fatalf("partial_json=%q", delta["partial_json"])
	}
}

func TestClaudeContentBlockStopJSON_WireFormat(t *testing.T) {
	raw := ClaudeContentBlockStopJSON(2)
	if !strings.Contains(string(raw), `"index":2`) {
		t.Fatalf("missing index: %s", raw)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if obj["type"] != "content_block_stop" {
		t.Fatalf("type=%v", obj["type"])
	}
	if int(obj["index"].(float64)) != 2 {
		t.Fatal("index mismatch")
	}
}

func TestClaudeContentBlockStartTextJSON_WireFormat(t *testing.T) {
	raw := ClaudeContentBlockStartTextJSON(0)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	cb := obj["content_block"].(map[string]any)
	if cb["type"] != "text" {
		t.Fatalf("content_block.type=%v", cb["type"])
	}
	if cb["text"] != "" {
		t.Fatalf("content_block.text=%v, want empty", cb["text"])
	}
}

func TestClaudeContentBlockStartThinkingJSON_WireFormat(t *testing.T) {
	raw := ClaudeContentBlockStartThinkingJSON(0)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	cb := obj["content_block"].(map[string]any)
	if cb["type"] != "thinking" {
		t.Fatalf("content_block.type=%v", cb["type"])
	}
	if cb["thinking"] != "" {
		t.Fatalf("content_block.thinking=%v, want empty", cb["thinking"])
	}
}

func TestClaudeTextDeltaJSON_PreservesUTF8(t *testing.T) {
	raw := ClaudeTextDeltaJSON(0, "漢字 αβγ 🚀")
	if !utf8.Valid(raw) {
		t.Fatal("raw is not valid UTF-8")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	delta := obj["delta"].(map[string]any)
	if delta["text"] != "漢字 αβγ 🚀" {
		t.Fatalf("text mismatch")
	}
}

func TestClaudeTextDeltaJSON_PooledBufferIsolation(t *testing.T) {
	first := ClaudeTextDeltaJSON(0, "aaaaaa")
	second := ClaudeTextDeltaJSON(1, "bbbbbbbbbbbbbbb")
	if !bytes.Contains(first, []byte(`"text":"aaaaaa"`)) {
		t.Fatalf("first event clobbered: %s", first)
	}
	if !bytes.Contains(second, []byte(`"text":"bbbbbbbbbbbbbbb"`)) {
		t.Fatalf("second event clobbered: %s", second)
	}
}

func BenchmarkClaudeTextDeltaJSON(b *testing.B) {
	const sample = "this is a representative streaming text chunk with some words"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ClaudeTextDeltaJSON(i&0xff, sample)
	}
}

func BenchmarkClaudeContentBlockStopJSON(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ClaudeContentBlockStopJSON(i & 0xff)
	}
}
