package openairesponses

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

func TestPruneOldestInputContextPrunesArrayByOldestItems(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":"old"},{"type":"message","role":"assistant","content":"old-answer"},{"type":"message","role":"user","content":"new"},{"type":"message","role":"assistant","content":"new-answer"}]}`)

	result, ok := PruneOldestInputContext(body)
	if !ok {
		t.Fatal("PruneOldestInputContext() ok = false")
	}
	if result.Kind != "input_array" {
		t.Fatalf("kind = %q, want input_array", result.Kind)
	}
	if result.OldItems != 4 || result.NewItems != 2 {
		t.Fatalf("items = %d->%d, want 4->2", result.OldItems, result.NewItems)
	}
	if got := gjson.GetBytes(result.Body, "input.0.content").String(); got != "new" {
		t.Fatalf("first kept content = %q, want new; body=%s", got, result.Body)
	}
}

func TestPruneOldestInputContextPrunesSingleStringInput(t *testing.T) {
	body := []byte(`{"input":"oldernewer"}`)

	result, ok := PruneOldestInputContext(body)
	if !ok {
		t.Fatal("PruneOldestInputContext() ok = false")
	}
	if result.Kind != "input_string" {
		t.Fatalf("kind = %q, want input_string", result.Kind)
	}
	if got := gjson.GetBytes(result.Body, "input").String(); got != "newer" {
		t.Fatalf("input = %q, want newer", got)
	}
}

func TestPruneOldestInputContextPrunesSingleMessageText(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"oldernewer"}]}]}`)

	result, ok := PruneOldestInputContext(body)
	if !ok {
		t.Fatal("PruneOldestInputContext() ok = false")
	}
	if result.Kind != "input_item_text" {
		t.Fatalf("kind = %q, want input_item_text", result.Kind)
	}
	if got := gjson.GetBytes(result.Body, "input.0.content.0.text").String(); got != "newer" {
		t.Fatalf("text = %q, want newer; body=%s", got, result.Body)
	}
}

func TestPruneOldestInputContextKeepsUTF8Boundary(t *testing.T) {
	body := []byte(`{"input":"你好你好最新上下文"}`)

	result, ok := PruneOldestInputContext(body)
	if !ok {
		t.Fatal("PruneOldestInputContext() ok = false")
	}
	got := gjson.GetBytes(result.Body, "input").String()
	if !utf8.ValidString(got) || !strings.Contains(got, "上下文") {
		t.Fatalf("input = %q, want valid UTF-8 suffix with context", got)
	}
}
