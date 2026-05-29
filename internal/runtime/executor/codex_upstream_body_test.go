package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestNormalizeCodexFinalUpstreamBody_NormalizesOfficialInputItems(t *testing.T) {
	body := []byte(`{
		"model": "client-alias",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"function_call","call_id":"call_mcp_1","name":"tool","arguments":"{}"},
			{"type":"mcp_tool_call_output","call_id":"call_mcp_1","output":"{\"ok\":true}"},
			{"type":"function_call","call_id":"call_mcp_2","name":"tool","arguments":"{}"},
			{"type":"mcp_tool_call_output","call_id":"call_mcp_2","output":{"content":[{"type":"text","text":"fallback"}],"structuredContent":{"ok":true}}},
			{"type":"compaction_summary","encrypted_content":"enc-summary"},
			{"type":"context_compaction","encrypted_content":"enc-context"},
			{"type":"compaction_trigger","reason":"token_limit"},
			{"type":"compaction","encrypted_content":"enc-compaction"}
		]
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	input := gjson.GetBytes(gotBody, "input")
	if !input.IsArray() {
		t.Fatalf("input should remain an array: %s", string(gotBody))
	}
	items := input.Array()
	if len(items) != 8 {
		t.Fatalf("input length = %d, want 8 after filtering compaction_trigger: %s", len(items), string(gotBody))
	}
	if got := items[2].Get("type").String(); got != "function_call_output" {
		t.Fatalf("mcp_tool_call_output should map to function_call_output, got %q: %s", got, string(gotBody))
	}
	if got := items[2].Get("call_id").String(); got != "call_mcp_1" {
		t.Fatalf("call_id was not preserved, got %q: %s", got, string(gotBody))
	}
	if got := items[2].Get("output").String(); got != `{"ok":true}` {
		t.Fatalf("output was not preserved, got %q: %s", got, string(gotBody))
	}
	if got := items[4].Get("type").String(); got != "function_call_output" {
		t.Fatalf("object mcp_tool_call_output should map to function_call_output, got %q: %s", got, string(gotBody))
	}
	if got := items[4].Get("output").String(); got != "Wall time: 0.0000 seconds\nOutput:\n"+`{"ok":true}` {
		t.Fatalf("object mcp output was not converted to Responses output text, got %q: %s", got, string(gotBody))
	}
	if got := items[5].Get("type").String(); got != "compaction" {
		t.Fatalf("compaction_summary should map to compaction, got %q: %s", got, string(gotBody))
	}
	if got := items[5].Get("encrypted_content").String(); got != "enc-summary" {
		t.Fatalf("compaction_summary encrypted_content was not preserved, got %q", got)
	}
	if got := items[6].Get("type").String(); got != "context_compaction" {
		t.Fatalf("context_compaction should be preserved, got %q: %s", got, string(gotBody))
	}
	if got := items[7].Get("type").String(); got != "compaction" {
		t.Fatalf("existing compaction should be preserved, got %q: %s", got, string(gotBody))
	}
	for _, item := range items {
		if got := item.Get("type").String(); got == "compaction_trigger" || got == "mcp_tool_call_output" || got == "compaction_summary" {
			t.Fatalf("unsupported official input item type leaked upstream: %s", string(gotBody))
		}
	}
}

func TestNormalizeCodexFinalUpstreamBody_DefaultsMissingInputToArray(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias"}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	input := gjson.GetBytes(gotBody, "input")
	if !input.IsArray() || len(input.Array()) != 0 {
		t.Fatalf("missing input should default to an empty array; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "parallel_tool_calls"); got.Type != gjson.True {
		t.Fatalf("gpt-5.4 should default parallel_tool_calls to JSON true from Codex catalog; got %s body=%s", got.Raw, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_DefaultsNullResponsesInputToArray(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":null}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	input := gjson.GetBytes(gotBody, "input")
	if !input.IsArray() || len(input.Array()) != 0 {
		t.Fatalf("null responses input should default to an empty array; body=%s", gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_RepairsPrunedMissingContext(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","previous_response_id":""}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	input := gjson.GetBytes(gotBody, "input")
	if !input.IsArray() || len(input.Array()) != 0 {
		t.Fatalf("missing Responses context should be repaired with input=[]; body=%s", gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_PreservesIncrementalPreviousResponseInput(t *testing.T) {
	body := []byte(`{
		"model":"client-alias",
		"previous_response_id":"resp_1",
		"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		preservePreviousResponseID:  true,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("previous_response_id = %q, want resp_1; body=%s", got, gotBody)
	}
	if gotLen := gjson.GetBytes(gotBody, "input.#").Int(); gotLen != 1 {
		t.Fatalf("input length = %d, want 1; body=%s", gotLen, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.output").String(); got != "ok" {
		t.Fatalf("input.0.output = %q, want ok; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_DefaultsNullCompactInputToArray(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":null}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamCompact,
		streamMode:                  codexStreamFieldDelete,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	input := gjson.GetBytes(gotBody, "input")
	if !input.IsArray() || len(input.Array()) != 0 {
		t.Fatalf("null compact input should default to an empty array; body=%s", gotBody)
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("compact request should not include stream; body=%s", gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_ParsesParallelToolCallString(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","parallel_tool_calls":"false"}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "parallel_tool_calls"); got.Type != gjson.False {
		t.Fatalf("parallel_tool_calls string false should normalize to JSON false; got %s body=%s", got.Raw, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_DefaultsOfficialReasoningAndVerbosity(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[]}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "include").Array(); len(got) != 1 || got[0].String() != "reasoning.encrypted_content" {
		t.Fatalf("include should contain reasoning.encrypted_content; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text.verbosity").String(); got != "low" {
		t.Fatalf("text.verbosity = %q, want low; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_PreservesCallerReasoningAndVerbosity(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"reasoning":{"effort":"high","summary":"auto"},"text":{"verbosity":"high"}}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "reasoning.summary").String(); got != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text.verbosity").String(); got != "high" {
		t.Fatalf("text.verbosity = %q, want high; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_RemovesUnsupportedReasoningAndVerbosity(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"parallel_tool_calls":null,"reasoning":{"effort":"high"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"high"}}`), "unknown-model-for-codex", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "parallel_tool_calls"); got.Type != gjson.False {
		t.Fatalf("unknown model should default parallel_tool_calls to false; got %s body=%s", got.Raw, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "reasoning"); got.Exists() {
		t.Fatalf("unsupported reasoning should be removed; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "include").Array(); len(got) != 0 {
		t.Fatalf("reasoning include should be removed when reasoning is unsupported; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text"); got.Exists() {
		t.Fatalf("text with only unsupported verbosity should be removed; body=%s", gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_RemovesUnsupportedVerbosityButKeepsSchema(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"text":{"verbosity":"high","format":{"type":"json_schema","schema":{"type":"object"}}}}`), "unknown-model-for-codex", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "text.verbosity"); got.Exists() {
		t.Fatalf("unsupported verbosity should be removed; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text.format.name").String(); got != codexDefaultOutputSchemaTextFormatName {
		t.Fatalf("json schema format should be preserved and named, got %q; body=%s", got, gotBody)
	}
}
