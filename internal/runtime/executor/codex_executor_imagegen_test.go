package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnsureImageGenerationTool_NoTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	if !tools.IsArray() {
		t.Fatalf("expected tools array, got %v", tools.Type)
	}
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected type=image_generation, got %s", arr[0].Get("type").String())
	}
	if arr[0].Get("output_format").String() != "png" {
		t.Fatalf("expected output_format=png, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_ExistingToolsWithoutImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"get_weather","parameters":{}}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "function" {
		t.Fatalf("expected first tool type=function, got %s", arr[0].Get("type").String())
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", arr[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_AlreadyPresent(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","output_format":"webp"},{"type":"function","name":"f1"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools (no duplicate), got %d", len(arr))
	}
	if arr[0].Get("output_format").String() != "webp" {
		t.Fatalf("expected original output_format=webp preserved, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_RecognizesImageFunctionForms(t *testing.T) {
	for _, testCase := range []struct {
		name string
		tool string
	}{
		{name: "qualified function", tool: `{"type":"function","name":"image_gen.imagegen"}`},
		{name: "namespace function", tool: `{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4","tools":[` + testCase.tool + `]}`)
			result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)
			if tools := gjson.GetBytes(result, "tools").Array(); len(tools) != 1 {
				t.Fatalf("image tool was injected twice: %s", result)
			}
		})
	}
}

func TestEnsureImageGenerationTool_EmptyToolsArray(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected type=image_generation, got %s", arr[0].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_WebSearchAndImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"web_search"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "web_search" {
		t.Fatalf("expected first tool type=web_search, got %s", arr[0].Get("type").String())
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", arr[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_GPT53CodexSparkDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.3-codex-spark","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.3-codex-spark", nil, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for gpt-5.3-codex-spark, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestEnsureImageGenerationTool_FreeCodexAuthDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	freeAuth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"plan_type": "free"},
	}
	result := ensureImageGenerationTool(body, "gpt-5.4", freeAuth, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for free codex auth, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestEnsureImageGenerationTool_APIKeyAuthDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	apiKeyAuth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}

	result := ensureImageGenerationTool(body, "gpt-5.4", apiKeyAuth, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for API-key codex auth, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestEnsureImageGenerationTool_ResponsesLiteHeaderDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"unknown-responses-lite-model","input":"draw a cat"}`)
	headers := make(http.Header)
	headers.Set(codexHeaderOpenAIInternalCodexResponsesLite, "true")

	result := ensureImageGenerationTool(body, "unknown-responses-lite-model", nil, headers)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", result)
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for responses-lite header, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestEnsureImageGenerationTool_ResponsesLiteClientMetadataDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"unknown-responses-lite-model","input":"draw a cat","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true}}`)

	result := ensureImageGenerationTool(body, "unknown-responses-lite-model", nil, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", result)
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for responses-lite metadata, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestIsCodexResponsesLiteRequest_KnownAccountCapabilityOverridesRequestHints(t *testing.T) {
	const (
		authID  = "test-account-responses-lite-request-hints-disabled"
		modelID = "test-responses-lite-request-hints-disabled-model"
	)
	auth := &cliproxyauth.Auth{ID: authID, Provider: "codex"}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{
		ID: modelID,
		CodexCapabilities: &registry.CodexClientModelCapabilities{
			ModelSlug:        modelID,
			UseResponsesLite: false,
		},
	}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	body := []byte(`{"model":"test-responses-lite-request-hints-disabled-model","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true}}`)
	headers := make(http.Header)
	headers.Set(codexHeaderOpenAIInternalCodexResponsesLite, "true")

	if isCodexResponsesLiteRequest(body, modelID, auth, headers) {
		t.Fatal("known account capability disabled responses-lite but request hints enabled it")
	}
}
