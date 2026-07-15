package executor

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
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

func TestCodexMatchesAzureResponsesBaseURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "openai azure", url: " https://Example.OpenAI.Azure.com/openai/responses ", want: true},
		{name: "cognitive services", url: "https://example.CognitiveServices.Azure.com/openai/responses", want: true},
		{name: "windows openai path", url: "https://example.Windows.net/OpenAI/responses", want: true},
		{name: "non azure", url: "https://api.openai.com/v1/responses", want: false},
		{name: "empty", url: "  ", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexMatchesAzureResponsesBaseURL(tt.url); got != tt.want {
				t.Fatalf("codexMatchesAzureResponsesBaseURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
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

func TestNormalizeCodexFinalUpstreamBody_StripsInputItemIDsUnlessStoreEnabled(t *testing.T) {
	body := []byte(`{
		"model":"client-alias",
		"input":[
			{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"function_call","id":"call_item_1","call_id":"call_1","name":"tool","arguments":"{}"},
			{"type":"function_call_output","id":"out_1","call_id":"call_1","output":"ok"}
		]
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "input.0.id"); got.Exists() {
		t.Fatalf("input.0.id should be stripped when store=false; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.1.id"); got.Exists() {
		t.Fatalf("input.1.id should be stripped when store=false; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.2.id"); got.Exists() {
		t.Fatalf("input.2.id should be stripped when store=false; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.2.call_id").String(); got != "call_1" {
		t.Fatalf("call_id = %q, want call_1; body=%s", got, gotBody)
	}

	gotStoreBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "azure"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       true,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotStoreBody, "input.0.id").String(); got != "msg_1" {
		t.Fatalf("input.0.id = %q, want msg_1 when store=true; body=%s", got, gotStoreBody)
	}
	if got := gjson.GetBytes(gotStoreBody, "input.1.id").String(); got != "call_item_1" {
		t.Fatalf("input.1.id = %q, want call_item_1 when store=true; body=%s", got, gotStoreBody)
	}
	if got := gjson.GetBytes(gotStoreBody, "input.2.id").String(); got != "out_1" {
		t.Fatalf("input.2.id = %q, want out_1 when store=true; body=%s", got, gotStoreBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_StripsItemPassthroughMetadataForNonOpenAIProviders(t *testing.T) {
	body := []byte(`{
		"model":"client-alias",
		"input":[
			{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"hi"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn_1"}},
			{"type":"function_call","id":"call_item_1","call_id":"call_1","name":"tool","arguments":"{}","internal_chat_message_metadata_passthrough":{"turn_id":"turn_1"}}
		]
	}`)

	gotCodexBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})
	if got := gjson.GetBytes(gotCodexBody, "input.0.internal_chat_message_metadata_passthrough.turn_id").String(); got != "turn_1" {
		t.Fatalf("codex provider should preserve item passthrough metadata, got %q; body=%s", got, gotCodexBody)
	}

	gotOpenAIBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "openai"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})
	if got := gjson.GetBytes(gotOpenAIBody, "input.1.internal_chat_message_metadata_passthrough.turn_id").String(); got != "turn_1" {
		t.Fatalf("openai provider should preserve item passthrough metadata, got %q; body=%s", got, gotOpenAIBody)
	}

	gotOtherBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "openai-compatibility"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})
	if got := gjson.GetBytes(gotOtherBody, "input.0.internal_chat_message_metadata_passthrough"); got.Exists() {
		t.Fatalf("non-openai provider should strip item passthrough metadata; body=%s", gotOtherBody)
	}
	if got := gjson.GetBytes(gotOtherBody, "input.1.internal_chat_message_metadata_passthrough"); got.Exists() {
		t.Fatalf("non-openai provider should strip function call passthrough metadata; body=%s", gotOtherBody)
	}

	gotAzureBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "azure"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       true,
		suppressDefaultInstructions: true,
	})
	if got := gjson.GetBytes(gotAzureBody, "input.0.id").String(); got != "msg_1" {
		t.Fatalf("azure store=true should preserve item id, got %q; body=%s", got, gotAzureBody)
	}
	if got := gjson.GetBytes(gotAzureBody, "input.0.internal_chat_message_metadata_passthrough"); got.Exists() {
		t.Fatalf("azure provider should strip item passthrough metadata; body=%s", gotAzureBody)
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
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","parallel_tool_calls":" FALSE "}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "parallel_tool_calls"); got.Type != gjson.False {
		t.Fatalf("parallel_tool_calls string false should normalize to JSON false; got %s body=%s", got.Raw, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_NormalizesOfficialServiceTier(t *testing.T) {
	t.Run("fast alias", func(t *testing.T) {
		gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"service_tier":"fast"}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
			requestKind:                 codexFinalUpstreamResponses,
			streamMode:                  codexStreamFieldTrue,
			store:                       false,
			suppressDefaultInstructions: true,
		})

		if got := gjson.GetBytes(gotBody, "service_tier").String(); got != "priority" {
			t.Fatalf("service_tier = %q, want priority; body=%s", got, gotBody)
		}
	})

	t.Run("default sentinel", func(t *testing.T) {
		gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"service_tier":"default"}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
			requestKind:                 codexFinalUpstreamResponses,
			streamMode:                  codexStreamFieldTrue,
			store:                       false,
			suppressDefaultInstructions: true,
		})

		if got := gjson.GetBytes(gotBody, "service_tier"); got.Exists() {
			t.Fatalf("default service_tier should be omitted; body=%s", gotBody)
		}
	})

	t.Run("unsupported known model tier", func(t *testing.T) {
		gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"service_tier":"flex"}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
			requestKind:                 codexFinalUpstreamResponses,
			streamMode:                  codexStreamFieldTrue,
			store:                       false,
			suppressDefaultInstructions: true,
		})

		if got := gjson.GetBytes(gotBody, "service_tier"); got.Exists() {
			t.Fatalf("unsupported service_tier should be omitted for known model; body=%s", gotBody)
		}
	})

	t.Run("unknown model explicit passthrough", func(t *testing.T) {
		gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"service_tier":"flex"}`), "custom-model", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
			requestKind:                 codexFinalUpstreamResponses,
			streamMode:                  codexStreamFieldTrue,
			store:                       false,
			suppressDefaultInstructions: true,
		})

		if got := gjson.GetBytes(gotBody, "service_tier").String(); got != "flex" {
			t.Fatalf("unknown model service_tier = %q, want flex passthrough; body=%s", got, gotBody)
		}
	})
}

func TestNormalizeCodexFinalUpstreamBody_SanitizesUnsupportedOriginalToolOutputImageDetail(t *testing.T) {
	body := []byte(`{
		"model":"client-alias",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,user","detail":"original"}]},
			{"type":"function_call","call_id":"function-call","name":"tool","arguments":"{}"},
			{"type":"function_call_output","call_id":"function-call","output":[
				{"type":"input_image","image_url":"data:image/png;base64,function","detail":"original"},
				{"type":"input_image","image_url":"data:image/png;base64,function-low","detail":"low"}
			]},
			{"type":"custom_tool_call","call_id":"custom-call","name":"custom","input":"{}"},
			{"type":"custom_tool_call_output","call_id":"custom-call","output":[
				{"type":"input_image","image_url":"data:image/png;base64,custom","detail":"Original"}
			]}
		]
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.2", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "input.0.content.0.detail").String(); got != "original" {
		t.Fatalf("user image detail = %q, want original; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.2.output.0.detail").String(); got != "high" {
		t.Fatalf("function output image detail = %q, want high; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.2.output.1.detail").String(); got != "low" {
		t.Fatalf("function low image detail = %q, want low; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.4.output.0.detail").String(); got != "high" {
		t.Fatalf("custom output image detail = %q, want high; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_PreservesSupportedOriginalToolOutputImageDetail(t *testing.T) {
	body := []byte(`{
		"model":"client-alias",
		"input":[
			{"type":"function_call","call_id":"function-call","name":"tool","arguments":"{}"},
			{"type":"function_call_output","call_id":"function-call","output":[{"type":"input_image","image_url":"data:image/png;base64,function","detail":"original"}]}
		]
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "input.1.output.0.detail").String(); got != "original" {
		t.Fatalf("supported model image detail = %q, want original; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_DefaultsOfficialReasoningAndVerbosity(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[]}`), "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "medium" {
		t.Fatalf("reasoning.effort = %q, want medium; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "include").Array(); len(got) != 1 || got[0].String() != "reasoning.encrypted_content" {
		t.Fatalf("include should contain reasoning.encrypted_content; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text.verbosity").String(); got != "low" {
		t.Fatalf("text.verbosity = %q, want low; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_LooksUpModelCapabilitiesOnce(t *testing.T) {
	originalCapabilitiesForModel := codexClientModelCapabilitiesForModel
	calls := 0
	codexClientModelCapabilitiesForModel = func(modelID string) (registry.CodexClientModelCapabilities, bool) {
		calls++
		return registry.CodexClientModelCapabilities{
			SupportsReasoningSummaryParameter: true,
			SupportsVerbosity:                 true,
			DefaultVerbosity:                  "low",
			SupportsImageDetailOriginal:       true,
			ServiceTiers:                      []string{"priority"},
		}, true
	}
	defer func() {
		codexClientModelCapabilitiesForModel = originalCapabilitiesForModel
	}()

	normalizeCodexFinalUpstreamBodyUncached(
		[]byte(`{"model":"client-alias","input":[],"reasoning":{"effort":"high"},"service_tier":"priority"}`),
		"test-model",
		&cliproxyauth.Auth{Provider: "codex"},
		codexFinalUpstreamBodyOptions{
			requestKind:                 codexFinalUpstreamResponses,
			streamMode:                  codexStreamFieldTrue,
			suppressDefaultInstructions: true,
		},
	)

	if calls != 1 {
		t.Fatalf("model capability lookups = %d, want 1", calls)
	}
}

func TestNormalizeCodexFinalUpstreamBody_DowngradesGPT55XHighReasoningForUpstream(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"reasoning":{"effort":"xhigh"}}`), "gpt-5.5", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_PreservesGPT56SolReasoningForUpstream(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		effort     string
		wantEffort string
	}{
		{name: "sol xhigh", model: "gpt-5.6-sol", effort: "xhigh", wantEffort: "xhigh"},
		{name: "sol max", model: "gpt-5.6-sol", effort: "max", wantEffort: "max"},
		{name: "sol high unchanged", model: "gpt-5.6-sol", effort: "high", wantEffort: "high"},
		{name: "sol medium unchanged", model: "gpt-5.6-sol", effort: "medium", wantEffort: "medium"},
		{name: "sol low unchanged", model: "gpt-5.6-sol", effort: "low", wantEffort: "low"},
		{name: "sol minimal unchanged", model: "gpt-5.6-sol", effort: "minimal", wantEffort: "minimal"},
		{name: "sol none unchanged", model: "gpt-5.6-sol", effort: "none", wantEffort: "none"},
		{name: "sol Ultra", model: "gpt-5.6-sol", effort: "Ultra", wantEffort: "Ultra"},
		{name: "sol uppercase model and effort", model: "GPT-5.6-SOL", effort: "XHIGH", wantEffort: "XHIGH"},
		{name: "terra max unchanged", model: "gpt-5.6-terra", effort: "max", wantEffort: "max"},
		{name: "luna xhigh unchanged", model: "gpt-5.6-luna", effort: "xhigh", wantEffort: "xhigh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"client-alias","input":[],"reasoning":{"effort":"` + tt.effort + `"}}`)
			gotBody := normalizeCodexFinalUpstreamBody(body, tt.model, &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
				requestKind:                 codexFinalUpstreamResponses,
				streamMode:                  codexStreamFieldTrue,
				store:                       false,
				suppressDefaultInstructions: true,
			})

			if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != tt.wantEffort {
				t.Fatalf("reasoning.effort = %q, want %q; body=%s", got, tt.wantEffort, gotBody)
			}
			if got := gjson.GetBytes(gotBody, "model").String(); got != tt.model {
				t.Fatalf("model = %q, want %q; body=%s", got, tt.model, gotBody)
			}
		})
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

func TestNormalizeCodexFinalUpstreamReasoning_MapsUltraToOfficialRequestEffort(t *testing.T) {
	capabilities := registry.CodexClientModelCapabilities{
		SupportsReasoningSummaries:        true,
		SupportsReasoningSummaryParameter: true,
		DefaultReasoningLevel:             "ultra",
	}

	t.Run("caller effort", func(t *testing.T) {
		gotBody := normalizeCodexFinalUpstreamReasoning([]byte(`{"reasoning":{"effort":"ultra","summary":"auto"}}`), &capabilities, false)
		if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "max" {
			t.Fatalf("reasoning.effort = %q, want max; body=%s", got, gotBody)
		}
		if got := gjson.GetBytes(gotBody, "reasoning.summary").String(); got != "auto" {
			t.Fatalf("reasoning.summary = %q, want auto; body=%s", got, gotBody)
		}
	})

	t.Run("default effort", func(t *testing.T) {
		gotBody := normalizeCodexFinalUpstreamReasoning([]byte(`{}`), &capabilities, false)
		if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "max" {
			t.Fatalf("reasoning.effort = %q, want max; body=%s", got, gotBody)
		}
	})

	t.Run("blank effort uses default", func(t *testing.T) {
		gotBody := normalizeCodexFinalUpstreamReasoning([]byte(`{"reasoning":{"effort":"  "}}`), &capabilities, false)
		if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "max" {
			t.Fatalf("reasoning.effort = %q, want max; body=%s", got, gotBody)
		}
	})
}

func TestNormalizeCodexFinalUpstreamBodyPreservesCodexReasoningSummaryDelivery(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[],
		"stream_options":{
			"reasoning_summary_delivery":"sequential_cutoff",
			"include_usage":true
		}
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})
	if got := gjson.GetBytes(gotBody, "stream_options.reasoning_summary_delivery").String(); got != "sequential_cutoff" {
		t.Fatalf("reasoning_summary_delivery = %q, want sequential_cutoff; body=%s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "stream_options.include_usage").Exists() {
		t.Fatalf("generic include_usage should be removed; body=%s", gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamResponsesLiteWithCapabilities(t *testing.T) {
	body := []byte(`{
		"model":"lite-model",
		"instructions":"test instructions",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"tool","parameters":{"type":"object"}}],
		"parallel_tool_calls":true,
		"reasoning":{"effort":"medium"}
	}`)

	gotBody := normalizeCodexFinalUpstreamResponsesLiteWithCapabilities(body, registry.CodexClientModelCapabilities{
		UseResponsesLite: true,
	})

	if gjson.GetBytes(gotBody, "instructions").Exists() {
		t.Fatalf("instructions should move into input for responses_lite; body=%s", gotBody)
	}
	if gjson.GetBytes(gotBody, "tools").Exists() {
		t.Fatalf("tools should move into additional_tools for responses_lite; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "parallel_tool_calls"); got.Type != gjson.False {
		t.Fatalf("parallel_tool_calls = %s, want false; body=%s", got.Raw, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "reasoning.context").String(); got != "all_turns" {
		t.Fatalf("reasoning.context = %q, want all_turns; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.type").String(); got != "additional_tools" {
		t.Fatalf("input.0.type = %q, want additional_tools; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.role").String(); got != "developer" {
		t.Fatalf("input.0.role = %q, want developer; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.tools.0.name").String(); got != "tool" {
		t.Fatalf("input.0.tools.0.name = %q, want tool; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.1.role").String(); got != "developer" {
		t.Fatalf("input.1.role = %q, want developer instruction message; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.1.content.0.text").String(); got != "test instructions" {
		t.Fatalf("instruction text = %q, want test instructions; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.2.role").String(); got != "user" {
		t.Fatalf("input.2.role = %q, want original user message; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_ResponsesLiteMovesDefaultInstructionsAfterDefaults(t *testing.T) {
	originalCapabilitiesForModel := codexClientModelCapabilitiesForModel
	codexClientModelCapabilitiesForModel = func(modelID string) (registry.CodexClientModelCapabilities, bool) {
		if modelID == "lite-model" {
			return registry.CodexClientModelCapabilities{
				UseResponsesLite:                  true,
				SupportsReasoningSummaries:        true,
				SupportsReasoningSummaryParameter: true,
				DefaultReasoningLevel:             "medium",
			}, true
		}
		return originalCapabilitiesForModel(modelID)
	}
	defer func() {
		codexClientModelCapabilitiesForModel = originalCapabilitiesForModel
	}()

	body := []byte(`{
		"model":"client-alias",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"tool","parameters":{"type":"object"}}]
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "lite-model", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
		store:       false,
	})

	if got := gjson.GetBytes(gotBody, "instructions"); got.Exists() {
		t.Fatalf("responses_lite should not keep top-level instructions after defaulting; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tools"); got.Exists() {
		t.Fatalf("responses_lite should move tools into input; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.type").String(); got != "additional_tools" {
		t.Fatalf("input.0.type = %q, want additional_tools; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.1.role").String(); got != "developer" {
		t.Fatalf("input.1.role = %q, want developer default instructions; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.1.content.0.text").String(); got != "You are a helpful assistant." {
		t.Fatalf("default instruction text = %q, want helpful assistant default; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.2.role").String(); got != "user" {
		t.Fatalf("input.2.role = %q, want original user message; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamResponsesLiteStripsInputImageDetails(t *testing.T) {
	body := []byte(`{
		"model":"lite-model",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,user","detail":"high"}]},
			{"type":"function_call_output","call_id":"function-call","output":[{"type":"input_image","image_url":"data:image/png;base64,function","detail":"low"}]},
			{"type":"custom_tool_call_output","call_id":"custom-call","output":[{"type":"input_image","image_url":"data:image/png;base64,custom","detail":"auto"}]}
		]
	}`)

	gotBody := normalizeCodexFinalUpstreamResponsesLiteWithCapabilities(body, registry.CodexClientModelCapabilities{
		UseResponsesLite: true,
	})

	for _, path := range []string{
		"input.0.content.0.detail",
		"input.1.output.0.detail",
		"input.2.output.0.detail",
	} {
		if got := gjson.GetBytes(gotBody, path); got.Exists() {
			t.Fatalf("%s should be stripped for responses_lite; body=%s", path, gotBody)
		}
	}
	for path, want := range map[string]string{
		"input.0.content.0.image_url": "data:image/png;base64,user",
		"input.1.output.0.image_url":  "data:image/png;base64,function",
		"input.2.output.0.image_url":  "data:image/png;base64,custom",
	} {
		if got := gjson.GetBytes(gotBody, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, gotBody)
		}
	}
}

func TestNormalizeCodexFinalUpstreamResponsesLiteReplacesRemoteInputImages(t *testing.T) {
	body := []byte(`{
		"model":"lite-model",
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_image","image_url":"data:image/png;base64,local","detail":"high"},
				{"type":"input_image","image_url":"https://example.com/image.png","detail":"high"}
			]}
		]
	}`)

	gotBody := normalizeCodexFinalUpstreamResponsesLiteWithCapabilities(body, registry.CodexClientModelCapabilities{
		UseResponsesLite: true,
	})

	if got := gjson.GetBytes(gotBody, "input.0.content.0.type").String(); got != "input_image" {
		t.Fatalf("local image type = %q, want input_image; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.content.0.detail"); got.Exists() {
		t.Fatalf("local image detail should be stripped; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.content.0.image_url").String(); got != "data:image/png;base64,local" {
		t.Fatalf("local image_url = %q, want data URL; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.content.1.type").String(); got != "input_text" {
		t.Fatalf("remote image replacement type = %q, want input_text; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.content.1.text").String(); got != codexResponsesLiteRemoteImageOmittedText {
		t.Fatalf("remote image replacement text = %q, want %q; body=%s", got, codexResponsesLiteRemoteImageOmittedText, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.content.1.image_url"); got.Exists() {
		t.Fatalf("remote image_url should be omitted; body=%s", gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBody_UnknownModelKeepsReasoningAndRemovesUnsupportedVerbosity(t *testing.T) {
	gotBody := normalizeCodexFinalUpstreamBody([]byte(`{"model":"client-alias","input":[],"parallel_tool_calls":null,"reasoning":{"effort":"high"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"high"}}`), "unknown-model-for-codex", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind:                 codexFinalUpstreamResponses,
		streamMode:                  codexStreamFieldTrue,
		store:                       false,
		suppressDefaultInstructions: true,
	})

	if got := gjson.GetBytes(gotBody, "parallel_tool_calls"); got.Type != gjson.False {
		t.Fatalf("unknown model should default parallel_tool_calls to false; got %s body=%s", got.Raw, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "high" {
		t.Fatalf("unknown model reasoning effort = %q, want high; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, `include.#(=="reasoning.encrypted_content")`).String(); got != "reasoning.encrypted_content" {
		t.Fatalf("unknown model should include reasoning encrypted content; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text"); got.Exists() {
		t.Fatalf("text with only unsupported verbosity should be removed; body=%s", gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamReasoningDropsUnsupportedSummaryParameterOnly(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"high","summary":"auto"}}`)
	capabilities := registry.CodexClientModelCapabilities{
		SupportsReasoningSummaryParameter: false,
	}
	gotBody := normalizeCodexFinalUpstreamReasoning(body, &capabilities, false)
	if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "reasoning.summary").Exists() {
		t.Fatalf("unsupported reasoning.summary should be removed; body=%s", gotBody)
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

func BenchmarkNormalizeCodexFinalUpstreamToolType(b *testing.B) {
	b.Run("known", func(b *testing.B) {
		toolTypes := []string{
			" Function ",
			"WEB_SEARCH_20250305",
			"Image_Generation",
			"Computer_Use",
			"Apply_Patch",
		}

		for b.Loop() {
			for _, toolType := range toolTypes {
				if normalizeCodexFinalUpstreamToolType(toolType) == "" {
					b.Fatal("unexpected empty tool type")
				}
			}
		}
	})

	b.Run("unknown", func(b *testing.B) {
		toolTypes := []string{
			"Unknown_Custom_Type",
		}

		for b.Loop() {
			for _, toolType := range toolTypes {
				if normalizeCodexFinalUpstreamToolType(toolType) == "" {
					b.Fatal("unexpected empty tool type")
				}
			}
		}
	})
}

func BenchmarkCodexMatchesAzureResponsesBaseURL(b *testing.B) {
	url := " https://Example.OpenAI.Azure.com/openai/responses "
	for b.Loop() {
		if !codexMatchesAzureResponsesBaseURL(url) {
			b.Fatal("expected azure responses base URL")
		}
	}
}

func BenchmarkNormalizeCodexFinalUpstreamToolChoice(b *testing.B) {
	choices := []gjson.Result{
		gjson.Parse(`" ANY "`),
		gjson.Parse(`{"type":"Allowed_Tools","mode":"ANY","tools":[{"type":"Function","name":"Read"},{"type":"WEB_SEARCH_20250305"},{"type":"Image_Generation"},{"type":"Computer_Use"}]}`),
		gjson.Parse(`{"type":"Custom","name":"apply_patch"}`),
	}

	for b.Loop() {
		for _, choice := range choices {
			if _, ok := normalizeCodexFinalUpstreamToolChoice(choice); !ok {
				b.Fatal("unexpected dropped tool choice")
			}
		}
	}
}

func TestNormalizeCodexFinalUpstreamToolChoiceEscapesNames(t *testing.T) {
	name := "read \"quoted\"\\path\nnext"
	nameJSON, err := json.Marshal(name)
	if err != nil {
		t.Fatalf("marshal name: %v", err)
	}
	tests := []struct {
		name string
		raw  string
		path string
	}{
		{name: "function", raw: `{"type":"function","name":` + string(nameJSON) + `}`, path: "name"},
		{name: "custom", raw: `{"type":"custom","name":` + string(nameJSON) + `}`, path: "name"},
		{name: "allowed function", raw: `{"type":"allowed_tools","tools":[{"type":"function","name":` + string(nameJSON) + `}]}`, path: "tools.0.name"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, ok := normalizeCodexFinalUpstreamToolChoice(gjson.Parse(test.raw))
			if !ok {
				t.Fatal("tool choice was dropped")
			}
			if !gjson.ValidBytes(raw) {
				t.Fatalf("tool choice is invalid JSON: %q", raw)
			}
			if got := gjson.GetBytes(raw, test.path).String(); got != name {
				t.Fatalf("normalized name = %q, want %q; body=%s", got, name, raw)
			}
		})
	}
}

func BenchmarkNormalizeCodexFinalUpstreamToolTypeLegacyMixed(b *testing.B) {
	toolTypes := []string{
		" Function ",
		"WEB_SEARCH_20250305",
		"Image_Generation",
		"Computer_Use",
		"Apply_Patch",
		"Unknown_Custom_Type",
	}

	for b.Loop() {
		for _, toolType := range toolTypes {
			if normalizeCodexFinalUpstreamToolType(toolType) == "" && toolType != "" {
				b.Fatal("unexpected empty tool type")
			}
		}
	}
}
