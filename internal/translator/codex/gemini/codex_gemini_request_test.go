package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToCodex_RestoresCodexNativeToolHistory(t *testing.T) {
	input := []byte(`{
		"model": "gemini-2.5-pro",
		"contents": [
			{"role": "user", "parts": [{"text": "use tools"}]},
			{"role": "model", "parts": [
				{"functionCall": {"name": "apply_patch", "args": {"input": "*** Begin Patch\n*** End Patch"}}},
				{"functionCall": {"name": "local_shell", "args": {"type": "exec", "command": ["pwd"]}}},
				{"functionCall": {"name": "tool_search", "args": {"query": "calendar", "limit": 1}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "apply_patch", "response": {"result": "patched"}}},
				{"functionResponse": {"name": "local_shell", "response": {"result": "/tmp"}}},
				{"functionResponse": {"name": "tool_search", "response": {"tools": [{"name": "calendar.create_event"}]}}}
			]}
		]
	}`)

	out := ConvertGeminiRequestToCodex("gpt-5.4", input, false)
	byType := map[string][]gjson.Result{}
	for _, item := range gjson.GetBytes(out, "input").Array() {
		itemType := item.Get("type").String()
		if itemType != "" {
			byType[itemType] = append(byType[itemType], item)
		}
	}

	if len(byType["custom_tool_call"]) != 1 {
		t.Fatalf("custom_tool_call count = %d, want 1. Output: %s", len(byType["custom_tool_call"]), string(out))
	}
	if got := byType["custom_tool_call"][0].Get("name").String(); got != "apply_patch" {
		t.Fatalf("custom tool name = %q, want apply_patch. Output: %s", got, string(out))
	}
	if got := byType["custom_tool_call"][0].Get("input").String(); got != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom tool input = %q. Output: %s", got, string(out))
	}
	if len(byType["custom_tool_call_output"]) != 1 {
		t.Fatalf("custom_tool_call_output count = %d, want 1. Output: %s", len(byType["custom_tool_call_output"]), string(out))
	}
	if got := byType["custom_tool_call_output"][0].Get("call_id").String(); got == "" || got != byType["custom_tool_call"][0].Get("call_id").String() {
		t.Fatalf("custom call/output call_id mismatch. Output: %s", string(out))
	}
	if len(byType["local_shell_call"]) != 1 {
		t.Fatalf("local_shell_call count = %d, want 1. Output: %s", len(byType["local_shell_call"]), string(out))
	}
	if got := byType["local_shell_call"][0].Get("action.command.0").String(); got != "pwd" {
		t.Fatalf("local_shell command = %q, want pwd. Output: %s", got, string(out))
	}
	if len(byType["tool_search_call"]) != 1 {
		t.Fatalf("tool_search_call count = %d, want 1. Output: %s", len(byType["tool_search_call"]), string(out))
	}
	if got := byType["tool_search_call"][0].Get("arguments.query").String(); got != "calendar" {
		t.Fatalf("tool_search query = %q, want calendar. Output: %s", got, string(out))
	}
	if len(byType["tool_search_output"]) != 1 {
		t.Fatalf("tool_search_output count = %d, want 1. Output: %s", len(byType["tool_search_output"]), string(out))
	}
	if got := byType["tool_search_output"][0].Get("tools.0.name").String(); got != "calendar.create_event" {
		t.Fatalf("tool_search output tool name = %q. Output: %s", got, string(out))
	}
	if got := byType["tool_search_output"][0].Get("call_id").String(); got == "" || got != byType["tool_search_call"][0].Get("call_id").String() {
		t.Fatalf("tool_search call/output call_id mismatch. Output: %s", string(out))
	}
}
