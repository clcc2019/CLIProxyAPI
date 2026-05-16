package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// parseSSEFrame splits an SSE frame ("event: name\ndata: {...}\n\n") into the
// event name and a parsed JSON object.
func parseSSEFrame(t *testing.T, raw []byte) (string, map[string]any) {
	t.Helper()
	s := string(raw)
	if !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("frame missing trailing \\n\\n: %q", s)
	}
	s = strings.TrimSuffix(s, "\n\n")
	lines := strings.SplitN(s, "\n", 2)
	if len(lines) != 2 {
		t.Fatalf("frame missing data line: %q", s)
	}
	if !strings.HasPrefix(lines[0], "event: ") {
		t.Fatalf("frame missing event prefix: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "data: ") {
		t.Fatalf("frame missing data prefix: %q", lines[1])
	}
	event := strings.TrimPrefix(lines[0], "event: ")
	dataPayload := strings.TrimPrefix(lines[1], "data: ")
	var obj map[string]any
	if err := json.Unmarshal([]byte(dataPayload), &obj); err != nil {
		t.Fatalf("data is not valid JSON: %v\npayload=%q", err, dataPayload)
	}
	return event, obj
}

func TestBuildClaudeStreamEvent_WireFormat(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		index int
	}{
		{"plain", "hello world", 0},
		{"with_quote_and_backslash", `say "hi"\n nope`, 5},
		{"unicode", "你好，世界 🌏", 2},
		{"control_chars", "line1\nline2\ttab", 1},
		{"empty", "", 0},
		{"large_index", "x", 999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := BuildClaudeStreamEvent(tc.text, tc.index)
			event, obj := parseSSEFrame(t, raw)
			if event != "content_block_delta" {
				t.Fatalf("event=%q", event)
			}
			if obj["type"] != "content_block_delta" {
				t.Fatalf("type=%v", obj["type"])
			}
			// JSON numbers decode as float64 by default.
			if got := int(obj["index"].(float64)); got != tc.index {
				t.Fatalf("index=%d, want %d", got, tc.index)
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

func TestBuildClaudeThinkingDeltaEvent_WireFormat(t *testing.T) {
	raw := BuildClaudeThinkingDeltaEvent(`I "think", therefore I am.`, 7)
	event, obj := parseSSEFrame(t, raw)
	if event != "content_block_delta" {
		t.Fatalf("event=%q", event)
	}
	if int(obj["index"].(float64)) != 7 {
		t.Fatal("index mismatch")
	}
	delta := obj["delta"].(map[string]any)
	if delta["type"] != "thinking_delta" {
		t.Fatalf("delta.type=%v", delta["type"])
	}
	if delta["thinking"] != `I "think", therefore I am.` {
		t.Fatalf("delta.thinking=%q", delta["thinking"])
	}
}

func TestBuildClaudeInputJsonDeltaEvent_WireFormat(t *testing.T) {
	raw := BuildClaudeInputJsonDeltaEvent(`{"k":"v"}`, 3)
	event, obj := parseSSEFrame(t, raw)
	if event != "content_block_delta" {
		t.Fatalf("event=%q", event)
	}
	if int(obj["index"].(float64)) != 3 {
		t.Fatal("index mismatch")
	}
	delta := obj["delta"].(map[string]any)
	if delta["type"] != "input_json_delta" {
		t.Fatalf("delta.type=%v", delta["type"])
	}
	// partial_json is itself a string carrying JSON characters; the SSE
	// builder must escape it as a JSON string, not embed it raw.
	if delta["partial_json"] != `{"k":"v"}` {
		t.Fatalf("partial_json=%q", delta["partial_json"])
	}
}

func TestBuildClaudeContentBlockStopEvent_WireFormat(t *testing.T) {
	raw := BuildClaudeContentBlockStopEvent(2)
	event, obj := parseSSEFrame(t, raw)
	if event != "content_block_stop" {
		t.Fatalf("event=%q", event)
	}
	if obj["type"] != "content_block_stop" {
		t.Fatalf("type=%v", obj["type"])
	}
	if int(obj["index"].(float64)) != 2 {
		t.Fatal("index mismatch")
	}
	if _, hasDelta := obj["delta"]; hasDelta {
		t.Fatal("content_block_stop must not include a delta field")
	}
}

func TestBuildClaudeThinkingBlockStopEvent_WireFormat(t *testing.T) {
	a := BuildClaudeThinkingBlockStopEvent(4)
	b := BuildClaudeContentBlockStopEvent(4)
	if !bytes.Equal(a, b) {
		t.Fatalf("thinking-stop and content-stop must be byte-identical at the same index\na=%q\nb=%q", a, b)
	}
}

// TestBuildClaudeStreamEvent_PooledBufferIsolated guards against accidental
// buffer sharing across calls. If the returned slice were a view onto the
// pooled buffer, a follow-up call would clobber it.
func TestBuildClaudeStreamEvent_PooledBufferIsolated(t *testing.T) {
	first := BuildClaudeStreamEvent("aaaaaaaa", 0)
	second := BuildClaudeStreamEvent("bbbbbbbbbbbbbbbbbbbbbbbbb", 1)

	// Re-parse first; it must still contain "aaaaaaaa" not "bbbbb…".
	_, obj := parseSSEFrame(t, first)
	delta := obj["delta"].(map[string]any)
	if delta["text"] != "aaaaaaaa" {
		t.Fatalf("first event was clobbered: text=%q", delta["text"])
	}
	_, obj2 := parseSSEFrame(t, second)
	delta2 := obj2["delta"].(map[string]any)
	if delta2["text"] != "bbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("second event mismatch: text=%q", delta2["text"])
	}
}

// TestBuildClaudeStreamEvent_PreservesUTF8 sanity-checks that the hand-built
// JSON path produces valid UTF-8 even with multibyte input. This is a
// regression guard against a future "optimization" that reaches for
// strconv.AppendQuote (which produces \xNN escapes that aren't valid JSON).
func TestBuildClaudeStreamEvent_PreservesUTF8(t *testing.T) {
	raw := BuildClaudeStreamEvent("漢字 αβγ 🚀", 0)
	if !utf8.Valid(raw) {
		t.Fatal("frame is not valid UTF-8")
	}
	_, obj := parseSSEFrame(t, raw)
	delta := obj["delta"].(map[string]any)
	if delta["text"] != "漢字 αβγ 🚀" {
		t.Fatalf("text mismatch: %q", delta["text"])
	}
}

// BenchmarkBuildClaudeStreamEvent quantifies the streaming hot path. Run
// `go test -bench=BenchmarkBuildClaudeStreamEvent -benchmem
//   ./internal/translator/kiro/claude/`
// to see allocation count per chunk; the optimized version should report
// ~1 alloc/op (the returned []byte) versus ~6 allocs/op for the prior
// reflection-marshal implementation.
func BenchmarkBuildClaudeStreamEvent(b *testing.B) {
	const sample = "this is a representative streaming text chunk with some words"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildClaudeStreamEvent(sample, i&0xff)
	}
}

func BenchmarkBuildClaudeContentBlockStopEvent(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildClaudeContentBlockStopEvent(i & 0xff)
	}
}
