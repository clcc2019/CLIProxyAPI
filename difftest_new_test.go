package main

import (
	"encoding/json"
	"math/rand"
	"os"
	"testing"

	cc "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/claude"
	rr "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func genClaudeReq(r *rand.Rand) map[string]any {
	roles := []string{"user", "assistant", "system", ""}
	n := r.Intn(6)
	msgs := make([]any, 0, n)
	for i := 0; i < n; i++ {
		role := roles[r.Intn(len(roles))]
		switch r.Intn(4) {
		case 0:
			msgs = append(msgs, map[string]any{"role": role, "content": "hello"})
		case 1:
			msgs = append(msgs, map[string]any{"role": role, "content": []any{}})
		case 2:
			parts := []any{}
			for j := 0; j < r.Intn(4); j++ {
				switch r.Intn(6) {
				case 0:
					parts = append(parts, map[string]any{"type": "text", "text": "t"})
				case 1:
					parts = append(parts, map[string]any{"type": "thinking", "signature": "gAAAAAB" + "x"})
				case 2:
					parts = append(parts, map[string]any{"type": "tool_use", "id": "toolu_1", "name": "fn", "input": map[string]any{"a": 1}})
				case 3:
					parts = append(parts, map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"})
				case 4:
					parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"data": "AAA", "media_type": "image/png"}})
				case 5:
					parts = append(parts, map[string]any{"type": "unknown"})
				}
			}
			msgs = append(msgs, map[string]any{"role": role, "content": parts})
		case 3:
			msgs = append(msgs, map[string]any{"role": role})
		}
	}
	req := map[string]any{"messages": msgs, "system": "sys"}
	if r.Intn(2) == 0 {
		tools := []any{}
		for j := 0; j < r.Intn(4); j++ {
			if r.Intn(3) == 0 {
				tools = append(tools, map[string]any{"type": "web_search_20250305", "name": "web_search"})
			} else {
				tools = append(tools, map[string]any{"name": "fn", "input_schema": map[string]any{"type": "object"}})
			}
		}
		req["tools"] = tools
	}
	return req
}

func genResponsesReq(r *rand.Rand) map[string]any {
	items := []any{}
	for i := 0; i < r.Intn(6); i++ {
		switch r.Intn(5) {
		case 0:
			items = append(items, map[string]any{"type": "message", "role": "system", "content": []any{map[string]any{"type": "input_text", "text": "x"}}})
		case 1:
			items = append(items, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "y"}}})
		case 2:
			items = append(items, map[string]any{"type": "reasoning", "id": "rs_" + string(make([]byte, 0)), "encrypted_content": "e"})
		case 3:
			items = append(items, map[string]any{"type": "function_call", "call_id": "c", "name": "n", "arguments": "{}"})
		case 4:
			items = append(items, map[string]any{"type": "message", "role": "developer"})
		}
	}
	req := map[string]any{"input": items, "model": "gpt-5"}
	if r.Intn(2) == 0 {
		req["tools"] = []any{
			map[string]any{"type": "web_search_preview"},
			map[string]any{"type": "function", "name": "f"},
			map[string]any{"type": "web_search_preview_2025_03_11"},
		}
	}
	if r.Intn(2) == 0 {
		req["tool_choice"] = map[string]any{"type": "web_search_preview", "tools": []any{map[string]any{"type": "web_search_preview"}}}
	}
	return req
}

func genSanitizeInput(r *rand.Rand) map[string]any {
	long := ""
	for i := 0; i < 80; i++ {
		long += "a"
	}
	items := []any{}
	for i := 0; i < r.Intn(6); i++ {
		switch r.Intn(5) {
		case 0:
			items = append(items, map[string]any{"type": "message", "id": "short"})
		case 1:
			items = append(items, map[string]any{"type": "message", "id": long})
		case 2:
			items = append(items, map[string]any{"type": "reasoning", "id": long, "encrypted_content": "e"})
		case 3:
			items = append(items, map[string]any{"type": "reasoning", "id": "short", "encrypted_content": "e"})
		case 4:
			items = append(items, map[string]any{"type": "message"})
		}
	}
	return map[string]any{"input": items}
}

func TestDumpNew(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	out := []map[string]string{}
	for i := 0; i < 400; i++ {
		cr, _ := json.Marshal(genClaudeReq(r))
		rrq, _ := json.Marshal(genResponsesReq(r))
		si, _ := json.Marshal(genSanitizeInput(r))
		out = append(out, map[string]string{
			"claude_in":   string(cr),
			"claude_out":  string(cc.ConvertClaudeRequestToCodex("m", cr, true)),
			"resp_in":     string(rrq),
			"resp_out":    string(rr.ConvertOpenAIResponsesRequestToCodex("m", rrq, true)),
			"san_in":      string(si),
			"san_out":     string(helps.SanitizeCodexInputItemIDs(si)),
		})
	}
	b, _ := json.Marshal(out)
	_ = os.WriteFile("/tmp/advrev_new.json", b, 0o644)
}
