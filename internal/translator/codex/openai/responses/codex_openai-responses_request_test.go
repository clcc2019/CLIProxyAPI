package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertSystemRoleToDeveloper_BasicConversion tests the basic system -> developer role conversion
func TestConvertSystemRoleToDeveloper_BasicConversion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are a pirate."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Say hello."}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that system role was converted to developer
	firstItemRole := gjson.Get(outputStr, "input.0.role")
	if firstItemRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstItemRole.String())
	}

	// Check that user role remains unchanged
	secondItemRole := gjson.Get(outputStr, "input.1.role")
	if secondItemRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", secondItemRole.String())
	}

	// Check content is preserved
	firstItemContent := gjson.Get(outputStr, "input.0.content.0.text")
	if firstItemContent.String() != "You are a pirate." {
		t.Errorf("Expected content 'You are a pirate.', got '%s'", firstItemContent.String())
	}
}

func TestConvertOpenAIResponsesRequestToCodex_UsesResolvedModelName(t *testing.T) {
	inputJSON := []byte(`{
		"model": "client-alias/gpt-5.2",
		"input": "hello"
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	if got := gjson.GetBytes(output, "model").String(); got != "gpt-5.2" {
		t.Fatalf("model = %q, want resolved model gpt-5.2; output=%s", got, string(output))
	}
}

func TestConvertOpenAIResponsesRequestToCodex_ReasoningNullDoesNotRequestReasoningInclude(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"reasoning": null,
		"input": "hello"
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	include := gjson.GetBytes(output, "include")
	if !include.IsArray() || len(include.Array()) != 0 {
		t.Fatalf("include = %s, want empty array for reasoning:null; output=%s", include.Raw, string(output))
	}
}

// TestConvertSystemRoleToDeveloper_MultipleSystemMessages tests conversion with multiple system messages
func TestConvertSystemRoleToDeveloper_MultipleSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "Be concise."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that both system roles were converted
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "developer" {
		t.Errorf("Expected second role 'developer', got '%s'", secondRole.String())
	}

	// Check that user role is unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "user" {
		t.Errorf("Expected third role 'user', got '%s'", thirdRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_NoSystemMessages tests that requests without system messages are unchanged
func TestConvertSystemRoleToDeveloper_NoSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi there!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that user and assistant roles are unchanged
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "assistant" {
		t.Errorf("Expected role 'assistant', got '%s'", secondRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_EmptyInput tests that empty input arrays are handled correctly
func TestConvertSystemRoleToDeveloper_EmptyInput(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": []
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that input is still an empty array
	inputArray := gjson.Get(outputStr, "input")
	if !inputArray.IsArray() {
		t.Error("Input should still be an array")
	}
	if len(inputArray.Array()) != 0 {
		t.Errorf("Expected empty array, got %d items", len(inputArray.Array()))
	}
}

// TestConvertSystemRoleToDeveloper_NoInputField tests that requests without input field are unchanged
func TestConvertSystemRoleToDeveloper_NoInputField(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that other fields are still set correctly
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true by conversion")
	}

	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be set to false by conversion")
	}
}

// TestConvertOpenAIResponsesRequestToCodex_OriginalIssue tests the exact issue reported by the user
func TestConvertOpenAIResponsesRequestToCodex_OriginalIssue(t *testing.T) {
	// This is the exact input that was failing with "System messages are not allowed"
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": "You are a pirate. Always respond in pirate speak."
			},
			{
				"type": "message",
				"role": "user",
				"content": "Say hello."
			}
		],
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify system role was converted to developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstRole.String())
	}

	// Verify stream was set to true (as required by Codex)
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true")
	}

	// Verify other required fields for Codex
	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be false")
	}

	parallelCalls := gjson.Get(outputStr, "parallel_tool_calls")
	if !parallelCalls.Bool() {
		t.Error("parallel_tool_calls should be true")
	}

	// Align with codex-rs: without a reasoning block, include is an empty array.
	include := gjson.Get(outputStr, "include")
	if !include.IsArray() || len(include.Array()) != 0 {
		t.Errorf("include should be an empty array when reasoning is absent, got: %s", include.Raw)
	}
}

// TestConvertOpenAIResponsesRequestToCodex_IncludeOnlyWithReasoning verifies
// that include=["reasoning.encrypted_content"] is injected when the request
// carries a reasoning block and include=[] otherwise.
func TestConvertOpenAIResponsesRequestToCodex_IncludeOnlyWithReasoning(t *testing.T) {
	withReasoning := []byte(`{
		"model": "gpt-5.2",
		"reasoning": {"effort": "high"},
		"input": [{"role":"user","content":"hi"}]
	}`)
	out := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", withReasoning, false)
	include := gjson.GetBytes(out, "include")
	if !include.IsArray() || len(include.Array()) != 1 || include.Array()[0].String() != "reasoning.encrypted_content" {
		t.Fatalf("expected include=[reasoning.encrypted_content] when reasoning present, got: %s", include.Raw)
	}

	withoutReasoning := []byte(`{
		"model": "gpt-5.2",
		"input": [{"role":"user","content":"hi"}]
	}`)
	out = ConvertOpenAIResponsesRequestToCodex("gpt-5.2", withoutReasoning, false)
	include = gjson.GetBytes(out, "include")
	if !include.IsArray() || len(include.Array()) != 0 {
		t.Fatalf("include should be empty when reasoning is missing, got: %s", string(out))
	}

	// Caller-provided include must be preserved verbatim when reasoning exists.
	callerInclude := []byte(`{
		"model": "gpt-5.2",
		"reasoning": {"effort": "high"},
		"include": ["reasoning.encrypted_content","other.field"],
		"input": [{"role":"user","content":"hi"}]
	}`)
	out = ConvertOpenAIResponsesRequestToCodex("gpt-5.2", callerInclude, false)
	arr := gjson.GetBytes(out, "include").Array()
	if len(arr) != 2 || arr[1].String() != "other.field" {
		t.Fatalf("caller-supplied include should be preserved, got: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToCodex_ServiceTierPreservesNonEmptyStrings(t *testing.T) {
	cases := []struct {
		name       string
		rawTier    string
		wantExists bool
		want       string
	}{
		{name: "auto", rawTier: `"auto"`, wantExists: true, want: "auto"},
		{name: "default", rawTier: `"default"`, wantExists: true, want: "default"},
		{name: "flex", rawTier: `"flex"`, wantExists: true, want: "flex"},
		{name: "priority", rawTier: `"priority"`, wantExists: true, want: "priority"},
		{name: "empty", rawTier: `""`},
		{name: "number", rawTier: `123`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := []byte(`{
				"model": "gpt-5.2",
				"service_tier": ` + tt.rawTier + `,
				"input": [{"role":"user","content":"hi"}]
			}`)
			out := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
			got := gjson.GetBytes(out, "service_tier")
			if got.Exists() != tt.wantExists {
				t.Fatalf("service_tier exists=%v, want %v; output=%s", got.Exists(), tt.wantExists, string(out))
			}
			if tt.wantExists && got.String() != tt.want {
				t.Fatalf("service_tier was changed to %q", got.String())
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToCodex_MapsResponseFormatToTextFormat(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [{"role":"user","content":"hi"}],
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"strict": true,
				"schema": {
					"type": "object",
					"properties": {"answer": {"type": "string"}},
					"required": ["answer"]
				}
			}
		},
		"text": {"verbosity": "low"}
	}`)

	out := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)

	if gjson.GetBytes(out, "response_format").Exists() {
		t.Fatalf("response_format should be removed after compatibility mapping: %s", string(out))
	}
	if got := gjson.GetBytes(out, "text.format.type").String(); got != "json_schema" {
		t.Fatalf("text.format.type = %q, want json_schema; output=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "text.format.name").String(); got != "codex_output_schema" {
		t.Fatalf("text.format.name = %q, want codex_output_schema; output=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "text.format.strict").Bool(); !got {
		t.Fatalf("text.format.strict = false, want true; output=%s", string(out))
	}
	if got := gjson.GetBytes(out, "text.format.schema.properties.answer.type").String(); got != "string" {
		t.Fatalf("text.format.schema not preserved; output=%s", string(out))
	}
	if got := gjson.GetBytes(out, "text.verbosity").String(); got != "low" {
		t.Fatalf("text.verbosity = %q, want low; output=%s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToCodex_TextFormatWinsOverResponseFormat(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [{"role":"user","content":"hi"}],
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"name": "legacy",
				"schema": {"type": "object"}
			}
		},
		"text": {
			"format": {
				"type": "json_schema",
				"name": "native",
				"schema": {"type": "object", "properties": {"ok": {"type": "boolean"}}}
			}
		}
	}`)

	out := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)

	if gjson.GetBytes(out, "response_format").Exists() {
		t.Fatalf("response_format should be removed when native text.format exists: %s", string(out))
	}
	if got := gjson.GetBytes(out, "text.format.name").String(); got != "native" {
		t.Fatalf("text.format.name = %q, want native; output=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "text.format.schema.properties.ok.type").String(); got != "boolean" {
		t.Fatalf("native text.format schema should win; output=%s", string(out))
	}
}

// TestConvertSystemRoleToDeveloper_AssistantRole tests that assistant role is preserved
func TestConvertSystemRoleToDeveloper_AssistantRole(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check system -> developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	// Check user unchanged
	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "user" {
		t.Errorf("Expected second role 'user', got '%s'", secondRole.String())
	}

	// Check assistant unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "assistant" {
		t.Errorf("Expected third role 'assistant', got '%s'", thirdRole.String())
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesWebSearchPreview(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4-mini",
		"input": "find latest OpenAI model news",
		"tools": [
			{"type": "web_search_preview_2025_03_11"}
		],
		"tool_choice": {
			"type": "allowed_tools",
			"tools": [
				{"type": "web_search_preview"},
				{"type": "web_search_preview_2025_03_11"}
			]
		}
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4-mini", inputJSON, false)

	if got := gjson.GetBytes(output, "tools.0.type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want %q: %s", got, "web_search", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q, want %q: %s", got, "allowed_tools", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.tools.0.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.tools.0.type = %q, want %q: %s", got, "web_search", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.tools.1.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.tools.1.type = %q, want %q: %s", got, "web_search", string(output))
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesTopLevelToolChoicePreviewAlias(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4-mini",
		"input": "find latest OpenAI model news",
		"tool_choice": {"type": "web_search_preview_2025_03_11"}
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4-mini", inputJSON, false)

	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.type = %q, want %q: %s", got, "web_search", string(output))
	}
}

func TestUserFieldDeletion(t *testing.T) {
	inputJSON := []byte(`{  
		"model": "gpt-5.2",  
		"user": "test-user",  
		"input": [{"role": "user", "content": "Hello"}]  
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify user field is deleted
	userField := gjson.Get(outputStr, "user")
	if userField.Exists() {
		t.Errorf("user field should be deleted, but it was found with value: %s", userField.Raw)
	}
}

func TestContextManagementCompactionCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"context_management": [
			{
				"type": "compaction",
				"compact_threshold": 12000
			}
		],
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "context_management").Exists() {
		t.Fatalf("context_management should be removed for Codex compatibility")
	}
	if gjson.Get(outputStr, "truncation").Exists() {
		t.Fatalf("truncation should be removed for Codex compatibility")
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesOfficialInputItems(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"mcp_tool_call_output","call_id":"call_mcp_1","output":"{\"ok\":true}"},
			{"type":"mcp_tool_call_output","call_id":"call_mcp_2","output":{"content":[{"type":"text","text":"fallback"}],"structuredContent":{"ok":true}}},
			{"type":"compaction_summary","encrypted_content":"enc-summary"},
			{"type":"context_compaction","encrypted_content":"enc-context"},
			{"type":"compaction_trigger","reason":"token_limit"},
			{"type":"compaction","encrypted_content":"enc-compaction"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	input := gjson.GetBytes(output, "input")
	if !input.IsArray() {
		t.Fatalf("input should remain an array: %s", string(output))
	}
	items := input.Array()
	if len(items) != 6 {
		t.Fatalf("input length = %d, want 6 after filtering compaction_trigger: %s", len(items), string(output))
	}
	if got := items[1].Get("type").String(); got != "function_call_output" {
		t.Fatalf("mcp_tool_call_output should map to function_call_output, got %q: %s", got, string(output))
	}
	if got := items[1].Get("call_id").String(); got != "call_mcp_1" {
		t.Fatalf("call_id was not preserved, got %q: %s", got, string(output))
	}
	if got := items[1].Get("output").String(); got != `{"ok":true}` {
		t.Fatalf("output was not preserved, got %q: %s", got, string(output))
	}
	if got := items[2].Get("type").String(); got != "function_call_output" {
		t.Fatalf("object mcp_tool_call_output should map to function_call_output, got %q: %s", got, string(output))
	}
	if got := items[2].Get("output").String(); got != "Wall time: 0.0000 seconds\nOutput:\n"+`{"ok":true}` {
		t.Fatalf("object mcp output was not converted to Responses output text, got %q: %s", got, string(output))
	}
	if got := items[3].Get("type").String(); got != "compaction" {
		t.Fatalf("compaction_summary should map to compaction, got %q: %s", got, string(output))
	}
	if got := items[3].Get("encrypted_content").String(); got != "enc-summary" {
		t.Fatalf("compaction_summary encrypted_content was not preserved, got %q", got)
	}
	if got := items[4].Get("type").String(); got != "context_compaction" {
		t.Fatalf("context_compaction should be preserved, got %q: %s", got, string(output))
	}
	if got := items[5].Get("type").String(); got != "compaction" {
		t.Fatalf("existing compaction should be preserved, got %q: %s", got, string(output))
	}
	for _, item := range items {
		if got := item.Get("type").String(); got == "compaction_trigger" || got == "mcp_tool_call_output" || got == "compaction_summary" {
			t.Fatalf("unsupported official input item type leaked upstream: %s", string(output))
		}
	}
}

func TestTruncationRemovedForCodexCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"truncation": "disabled",
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "truncation").Exists() {
		t.Fatalf("truncation should be removed for Codex compatibility")
	}
}
