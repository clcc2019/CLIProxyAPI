package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeResponseInputItemsConvertsMCPStructuredContent(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_mcp_1","name":"tool","arguments":"{}"},
			{
				"type":"mcp_tool_call_output",
				"call_id":"call_mcp_1",
				"output":{
					"content":[{"type":"text","text":"fallback"}],
					"structuredContent":{"ok":true,"count":2},
					"isError":false
				}
			}
		]
	}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotType := gjson.GetBytes(got, "input.1.type").String(); gotType != "function_call_output" {
		t.Fatalf("type = %q, want function_call_output; body=%s", gotType, got)
	}
	if gotOutput := gjson.GetBytes(got, "input.1.output").String(); gotOutput != "Wall time: 0.0000 seconds\nOutput:\n"+`{"count":2,"ok":true}` {
		t.Fatalf("output = %q, want wall-time header plus compact structured content JSON; body=%s", gotOutput, got)
	}
}

func TestNormalizeResponseInputItemsUsesMCPWallTimeSeconds(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_mcp_1","name":"tool","arguments":"{}"},
			{
				"type":"mcp_tool_call_output",
				"call_id":"call_mcp_1",
				"wall_time_seconds":1.25,
				"output":{"content":[{"type":"text","text":"hello"}],"isError":false}
			}
		]
	}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotOutput := gjson.GetBytes(got, "input.1.output").String(); gotOutput != "Wall time: 1.2500 seconds\nOutput:\n"+`[{"text":"hello","type":"text"}]` {
		t.Fatalf("output = %q, want measured wall-time header plus serialized MCP content array; body=%s", gotOutput, got)
	}
}

func TestNormalizeResponseInputItemsConvertsMCPImageContentItems(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_mcp_1","name":"tool","arguments":"{}"},
			{
				"type":"mcp_tool_call_output",
				"call_id":"call_mcp_1",
				"output":{
					"content":[
						{"type":"text","text":"see image"},
						{"type":"image","data":"BASE64","mimeType":"image/png","_meta":{"codex/imageDetail":"original"}}
					],
					"isError":false
				}
			}
		]
	}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotType := gjson.GetBytes(got, "input.1.type").String(); gotType != "function_call_output" {
		t.Fatalf("type = %q, want function_call_output; body=%s", gotType, got)
	}
	if gotText := gjson.GetBytes(got, "input.1.output.0.text").String(); gotText != "Wall time: 0.0000 seconds\nOutput:" {
		t.Fatalf("output.0.text = %q, want wall-time header; body=%s", gotText, got)
	}
	if gotItemType := gjson.GetBytes(got, "input.1.output.1.type").String(); gotItemType != "input_text" {
		t.Fatalf("output.1.type = %q, want input_text; body=%s", gotItemType, got)
	}
	if gotItemType := gjson.GetBytes(got, "input.1.output.2.type").String(); gotItemType != "input_image" {
		t.Fatalf("output.2.type = %q, want input_image; body=%s", gotItemType, got)
	}
	if gotURL := gjson.GetBytes(got, "input.1.output.2.image_url").String(); gotURL != "data:image/png;base64,BASE64" {
		t.Fatalf("image_url = %q; body=%s", gotURL, got)
	}
	if gotDetail := gjson.GetBytes(got, "input.1.output.2.detail").String(); gotDetail != "original" {
		t.Fatalf("detail = %q, want original; body=%s", gotDetail, got)
	}
}

func TestNormalizeResponseInputItemsConvertsMCPPureTextContentToSerializedContent(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_mcp_1","name":"tool","arguments":"{}"},
			{
				"type":"mcp_tool_call_output",
				"call_id":"call_mcp_1",
				"output":{"content":[{"type":"text","text":"hello"}],"isError":false}
			}
		]
	}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotOutput := gjson.GetBytes(got, "input.1.output").String(); gotOutput != "Wall time: 0.0000 seconds\nOutput:\n"+`[{"text":"hello","type":"text"}]` {
		t.Fatalf("output = %q, want wall-time header plus serialized MCP content array; body=%s", gotOutput, got)
	}
}

func TestNormalizeResponseInputItemsPreservesToolPairingForIncrementalCallers(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)

	got := NormalizeResponseInputItems(body)

	if gotLen := gjson.GetBytes(got, "input.#").Int(); gotLen != 1 {
		t.Fatalf("input length = %d, want 1; body=%s", gotLen, got)
	}
	if gotOutput := gjson.GetBytes(got, "input.0.output").String(); gotOutput != "ok" {
		t.Fatalf("output = %q, want ok; body=%s", gotOutput, got)
	}
}

func TestNormalizeResponseInputItemsInsertsMissingFunctionCallOutput(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}"}]}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotType := gjson.GetBytes(got, "input.1.type").String(); gotType != "function_call_output" {
		t.Fatalf("inserted type = %q, want function_call_output; body=%s", gotType, got)
	}
	if gotOutput := gjson.GetBytes(got, "input.1.output").String(); gotOutput != "aborted" {
		t.Fatalf("inserted output = %q, want aborted; body=%s", gotOutput, got)
	}
	if gotCallID := gjson.GetBytes(got, "input.1.call_id").String(); gotCallID != "call_1" {
		t.Fatalf("inserted call_id = %q, want call_1; body=%s", gotCallID, got)
	}
}

func TestNormalizeResponseInputItemsInsertsMissingLocalShellOutput(t *testing.T) {
	body := []byte(`{"input":[{"type":"local_shell_call","call_id":"call_shell"}]}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotType := gjson.GetBytes(got, "input.1.type").String(); gotType != "function_call_output" {
		t.Fatalf("inserted type = %q, want function_call_output; body=%s", gotType, got)
	}
	if gotCallID := gjson.GetBytes(got, "input.1.call_id").String(); gotCallID != "call_shell" {
		t.Fatalf("inserted call_id = %q, want call_shell; body=%s", gotCallID, got)
	}
}

func TestNormalizeResponseInputItemsInsertsMissingCustomToolOutput(t *testing.T) {
	body := []byte(`{"input":[{"type":"custom_tool_call","call_id":"call_custom","name":"tool","input":"{}"}]}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotType := gjson.GetBytes(got, "input.1.type").String(); gotType != "custom_tool_call_output" {
		t.Fatalf("inserted type = %q, want custom_tool_call_output; body=%s", gotType, got)
	}
	if gotOutput := gjson.GetBytes(got, "input.1.output").String(); gotOutput != "aborted" {
		t.Fatalf("inserted output = %q, want aborted; body=%s", gotOutput, got)
	}
}

func TestNormalizeResponseInputItemsInsertsMissingToolSearchOutput(t *testing.T) {
	body := []byte(`{"input":[{"type":"tool_search_call","call_id":"call_search","query":"abc"}]}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotType := gjson.GetBytes(got, "input.1.type").String(); gotType != "tool_search_output" {
		t.Fatalf("inserted type = %q, want tool_search_output; body=%s", gotType, got)
	}
	if gotStatus := gjson.GetBytes(got, "input.1.status").String(); gotStatus != "completed" {
		t.Fatalf("inserted status = %q, want completed; body=%s", gotStatus, got)
	}
	if gotExecution := gjson.GetBytes(got, "input.1.execution").String(); gotExecution != "client" {
		t.Fatalf("inserted execution = %q, want client; body=%s", gotExecution, got)
	}
	if gotTools := gjson.GetBytes(got, "input.1.tools"); !gotTools.IsArray() || len(gotTools.Array()) != 0 {
		t.Fatalf("inserted tools = %s, want empty array; body=%s", gotTools.Raw, got)
	}
}

func TestNormalizeResponseInputItemsDoesNotInsertOutputForServerToolSearchCall(t *testing.T) {
	body := []byte(`{"input":[{"type":"tool_search_call","call_id":"server_search","execution":"server","status":"completed","arguments":{"paths":["crm"]}}]}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotLen := gjson.GetBytes(got, "input.#").Int(); gotLen != 1 {
		t.Fatalf("input length = %d, want 1; body=%s", gotLen, got)
	}
	if gotType := gjson.GetBytes(got, "input.0.type").String(); gotType != "tool_search_call" {
		t.Fatalf("remaining type = %q, want tool_search_call; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "input.1").Exists() {
		t.Fatalf("server tool_search_call should not get synthetic output; body=%s", got)
	}
}

func TestNormalizeResponseInputItemsRemovesOrphanOutputs(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call_output","call_id":"missing_fn","output":"ok"},
		{"type":"custom_tool_call_output","call_id":"missing_custom","output":"ok"},
		{"type":"tool_search_output","call_id":"missing_search","status":"completed","execution":"client","tools":[]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotLen := gjson.GetBytes(got, "input.#").Int(); gotLen != 1 {
		t.Fatalf("input length = %d, want 1; body=%s", gotLen, got)
	}
	if gotType := gjson.GetBytes(got, "input.0.type").String(); gotType != "message" {
		t.Fatalf("remaining type = %q, want message; body=%s", gotType, got)
	}
}

func TestNormalizeResponseInputItemsPreservesStandaloneToolSearchServerOutputs(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"tool_search_output","status":"completed","execution":"server","tools":[]},
		{"type":"tool_search_output","status":"completed","execution":"client","tools":[]}
	]}`)

	got := NormalizeFullTranscriptResponseInputItems(body)

	if gotLen := gjson.GetBytes(got, "input.#").Int(); gotLen != 2 {
		t.Fatalf("input length = %d, want 2; body=%s", gotLen, got)
	}
}
