package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecuteNormalizesNullInstructions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses")
	}
	if got := gjson.GetBytes(gotBody, "instructions").String(); got != "You are a helpful assistant." {
		t.Fatalf("instructions = %q, want default instructions; body=%s", got, gotBody)
	}
}

func TestCodexExecutorExecuteKeepsClaudeSystemAsInstructions(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"claude-sonnet-4",
			"system":[
				{"type":"text","text":"x-anthropic-billing-header: tenant-123"},
				{"type":"text","text":"Be helpful"}
			],
			"messages":[{"role":"user","content":"hello"}]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "instructions").String(); got != "Be helpful" {
		t.Fatalf("instructions = %q, want %q; body=%s", got, "Be helpful", gotBody)
	}
}

func TestCodexExecutorExecuteDerivesInstructionsFromDeveloperInput(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"instructions":"",
			"input":[
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"Use concise answers."}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
			]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "instructions").String(); got != "Use concise answers." {
		t.Fatalf("instructions = %q, want developer input instructions; body=%s", got, gotBody)
	}
}

func TestCodexExecutorExecuteNormalizesMissingToolTypes(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"instructions":"Be helpful.",
			"input":"hello",
			"tools":[
				{"name":"Read","description":"Read files","input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"file_path":{"type":"string"}}}},
				{"type":"None","name":"Write","input_schema":{"type":"object","properties":{"content":{"type":"string"}}}}
			],
			"tool_choice":{"type":"tool","name":"Read"}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "tools.0.type").String(); got != "function" {
		t.Fatalf("tools.0.type = %q, want function; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tools.1.type").String(); got != "function" {
		t.Fatalf("tools.1.type = %q, want function; body=%s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "tools.0.input_schema").Exists() {
		t.Fatalf("tools.0.input_schema should be removed; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tools.0.parameters.properties.file_path.type").String(); got != "string" {
		t.Fatalf("tools.0.parameters not copied from input_schema; got %q body=%s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "tools.0.parameters.$schema").Exists() {
		t.Fatalf("tools.0.parameters.$schema should be removed after input_schema copy; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tool_choice.name").String(); got != "Read" {
		t.Fatalf("tool_choice.name = %q, want Read; body=%s", got, gotBody)
	}
}

func TestCodexExecutorExecuteNormalizesToolChoiceAllowedTools(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"instructions":"Be helpful.",
			"input":"hello",
			"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["example.com"]}],
			"tool_choice":{"type":"allowed_tools","tools":[{"type":"None","name":"Read","input_schema":{"type":"object"}}]}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "tools.0.type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tools.0.filters.allowed_domains.0").String(); got != "example.com" {
		t.Fatalf("allowed_domains not moved to filters; got %q body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tools.0.external_web_access").Bool(); got {
		t.Fatalf("web_search external_web_access = true, want default cached false; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tool_choice.tools.0.type").String(); got != "function" {
		t.Fatalf("tool_choice.tools.0.type = %q, want function; body=%s", got, gotBody)
	}
}

func TestNormalizeCodexFinalUpstreamBodyNormalizesAllowedToolsChoiceRefs(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"instructions":"Be helpful.",
		"input":"hello",
		"tool_choice":{
			"type":"Allowed_Tools",
			"mode":"ANY",
			"cache_control":{"type":"ephemeral"},
			"tools":[
				{"type":"Function","name":"Read","description":"Read files","strict":false,"parameters":{"type":"object"},"cache_control":{"type":"ephemeral"}},
				{"type":"MCP","server_label":"codex_apps","name":"calendar.create_event","authorization":"secret"},
				{"type":"WEB_SEARCH_20250305","name":"web_search","filters":{"allowed_domains":["example.com"]}},
				{"type":"Image_Generation","output_format":"png"},
				{"type":"Custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
				{"type":"Computer_Use","display_width":1024}
			]
		}
	}`)

	got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if gotType := gjson.GetBytes(got, "tool_choice.type").String(); gotType != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q, want allowed_tools; body=%s", gotType, got)
	}
	if gotMode := gjson.GetBytes(got, "tool_choice.mode").String(); gotMode != "required" {
		t.Fatalf("tool_choice.mode = %q, want required; body=%s", gotMode, got)
	}
	if gotCount := gjson.GetBytes(got, "tool_choice.tools.#").Int(); gotCount != 6 {
		t.Fatalf("tool_choice.tools length = %d, want 6; body=%s", gotCount, got)
	}
	if gotName := gjson.GetBytes(got, "tool_choice.tools.0.name").String(); gotName != "Read" {
		t.Fatalf("function allowed tool name = %q, want Read; body=%s", gotName, got)
	}
	if gjson.GetBytes(got, "tool_choice.tools.0.parameters").Exists() || gjson.GetBytes(got, "tool_choice.tools.0.strict").Exists() {
		t.Fatalf("function allowed tool should be a reference, not a full definition; body=%s", got)
	}
	if gotServer := gjson.GetBytes(got, "tool_choice.tools.1.server_label").String(); gotServer != "codex_apps" {
		t.Fatalf("mcp allowed tool server_label = %q, want codex_apps; body=%s", gotServer, got)
	}
	if gjson.GetBytes(got, "tool_choice.tools.1.authorization").Exists() {
		t.Fatalf("mcp allowed tool should not keep authorization; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tool_choice.tools.2.type").String(); gotType != "web_search" {
		t.Fatalf("web_search allowed tool type = %q, want web_search; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tool_choice.tools.2.filters").Exists() {
		t.Fatalf("web_search allowed tool should not keep full filters; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tool_choice.tools.3.type").String(); gotType != "image_generation" {
		t.Fatalf("image_generation allowed tool type = %q; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tool_choice.tools.3.output_format").Exists() {
		t.Fatalf("image_generation allowed tool should not keep output_format; body=%s", got)
	}
	if gotName := gjson.GetBytes(got, "tool_choice.tools.4.name").String(); gotName != "apply_patch" {
		t.Fatalf("custom allowed tool name = %q, want apply_patch; body=%s", gotName, got)
	}
	if gjson.GetBytes(got, "tool_choice.tools.4.format").Exists() {
		t.Fatalf("custom allowed tool should not keep full format; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tool_choice.tools.5.type").String(); gotType != "computer_use" {
		t.Fatalf("computer allowed tool type = %q, want computer_use; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tool_choice.tools.5.display_width").Exists() {
		t.Fatalf("computer allowed tool should not keep full definition fields; body=%s", got)
	}
	if gjson.GetBytes(got, "tool_choice.cache_control").Exists() {
		t.Fatalf("allowed_tools choice should not keep cache_control; body=%s", got)
	}
}

func TestNormalizeCodexFinalUpstreamBodyPreservesOfficialCodexToolShapes(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"instructions":"Be helpful.",
		"input":"hello",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__codex_apps__calendar",
				"description":"Plan events",
				"tools":[{
					"type":"function",
					"name":"create_event",
					"description":"Create an event.",
					"strict":false,
					"defer_loading":true,
					"parameters":{"type":"object","properties":{}},
					"output_schema":{"type":"object"}
				}]
				},
				{"type":"function","name":"plain","description":"Plain.","strict":false,"defer_loading":false,"parameters":{"type":"object","properties":{}},"output_schema":{"type":"object"}},
				{"type":"tool_search","execution":"sync","description":"Search for tools.","parameters":{"type":"object","properties":{}},"cache_control":{"type":"ephemeral"}},
				{"type":"image_generation","output_format":"png","cache_control":{"type":"ephemeral"}},
				{"type":"custom","name":"apply_patch","description":"Apply patches.","defer_loading":false,"output_schema":{"type":"object"},"format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}
			],
			"tool_choice":{"type":"custom","name":"apply_patch"}
	}`)

	got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if gotCount := gjson.GetBytes(got, "tools.#").Int(); gotCount != 5 {
		t.Fatalf("tools length = %d, want 5; body=%s", gotCount, got)
	}
	if gotType := gjson.GetBytes(got, "tools.0.type").String(); gotType != "namespace" {
		t.Fatalf("tools.0.type = %q, want namespace; body=%s", gotType, got)
	}
	if gotName := gjson.GetBytes(got, "tools.0.name").String(); gotName != "mcp__codex_apps__calendar" {
		t.Fatalf("namespace name = %q; body=%s", gotName, got)
	}
	if gotType := gjson.GetBytes(got, "tools.0.tools.0.type").String(); gotType != "function" {
		t.Fatalf("namespace child type = %q, want function; body=%s", gotType, got)
	}
	if gotDeferred := gjson.GetBytes(got, "tools.0.tools.0.defer_loading").Bool(); !gotDeferred {
		t.Fatalf("namespace child defer_loading not preserved; body=%s", got)
	}
	if gjson.GetBytes(got, "tools.0.tools.0.output_schema").Exists() {
		t.Fatalf("namespace child output_schema should not be serialized upstream; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tools.1.type").String(); gotType != "function" {
		t.Fatalf("tools.1.type = %q, want function; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tools.1.defer_loading").Exists() || gjson.GetBytes(got, "tools.1.output_schema").Exists() {
		t.Fatalf("function tool should omit false defer_loading and output_schema; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tools.2.type").String(); gotType != "tool_search" {
		t.Fatalf("tools.1.type = %q, want tool_search; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tools.2.cache_control").Exists() {
		t.Fatalf("tool_search cache_control should be removed; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tools.3.type").String(); gotType != "image_generation" {
		t.Fatalf("tools.2.type = %q, want image_generation; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tools.3.cache_control").Exists() {
		t.Fatalf("image_generation cache_control should be removed; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tools.4.type").String(); gotType != "custom" {
		t.Fatalf("tools.3.type = %q, want custom; body=%s", gotType, got)
	}
	if gotSyntax := gjson.GetBytes(got, "tools.4.format.syntax").String(); gotSyntax != "lark" {
		t.Fatalf("custom format syntax = %q, want lark; body=%s", gotSyntax, got)
	}
	if gjson.GetBytes(got, "tools.4.defer_loading").Exists() || gjson.GetBytes(got, "tools.4.output_schema").Exists() {
		t.Fatalf("custom tool should omit non-freeform fields; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tool_choice.type").String(); gotType != "custom" {
		t.Fatalf("tool_choice.type = %q, want custom; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tools.4.parameters").Exists() {
		t.Fatalf("custom tool should not be converted to function parameters; body=%s", got)
	}
}

func TestNormalizeCodexFinalUpstreamBodyCompletesHostedCodexToolDefaults(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"instructions":"Be helpful.",
		"input":"hello",
		"tools":[
			{"type":"tool_search","execution":"","description":"","parameters":null,"output_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}},
			{"type":"tool_search","execution":"client","description":"Search.","parameters":{"type":"object","properties":{}}},
			{"type":"image_generation","output_format":"","output_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}},
			{"type":"web_search","name":"web_search","enabled":true,"max_uses":5,"output_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}
		]
	}`)

	got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if gotExecution := gjson.GetBytes(got, "tools.0.execution").String(); gotExecution != "client" {
		t.Fatalf("tool_search execution = %q, want client; body=%s", gotExecution, got)
	}
	if gotDescription := gjson.GetBytes(got, "tools.0.description").String(); !strings.Contains(gotDescription, "Tool discovery") {
		t.Fatalf("tool_search description was not defaulted; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tools.0.parameters.type").String(); gotType != "object" {
		t.Fatalf("tool_search parameters.type = %q, want object; body=%s", gotType, got)
	}
	if gotRequired := gjson.GetBytes(got, `tools.0.parameters.required.0`).String(); gotRequired != "query" {
		t.Fatalf("tool_search required[0] = %q, want query; body=%s", gotRequired, got)
	}
	if gotLimit := gjson.GetBytes(got, "tools.1.parameters.properties.limit.type").String(); gotLimit != "number" {
		t.Fatalf("tool_search malformed parameters were not repaired; body=%s", got)
	}
	if gotAdditional := gjson.GetBytes(got, "tools.1.parameters.additionalProperties"); gotAdditional.Type != gjson.False {
		t.Fatalf("tool_search additionalProperties = %s, want false; body=%s", gotAdditional.Raw, got)
	}
	if gotFormat := gjson.GetBytes(got, "tools.2.output_format").String(); gotFormat != "png" {
		t.Fatalf("image_generation output_format = %q, want png; body=%s", gotFormat, got)
	}
	if gotExternal := gjson.GetBytes(got, "tools.3.external_web_access"); !gotExternal.Exists() || gotExternal.Bool() {
		t.Fatalf("web_search external_web_access = %s, want false; body=%s", gotExternal.Raw, got)
	}
	if gjson.GetBytes(got, "tools.3.name").Exists() || gjson.GetBytes(got, "tools.3.enabled").Exists() || gjson.GetBytes(got, "tools.3.max_uses").Exists() {
		t.Fatalf("web_search kept compatibility-only fields; body=%s", got)
	}
	if gjson.GetBytes(got, "tools.0.cache_control").Exists() || gjson.GetBytes(got, "tools.2.cache_control").Exists() || gjson.GetBytes(got, "tools.3.cache_control").Exists() {
		t.Fatalf("hosted tool cache_control should be removed; body=%s", got)
	}
	if gjson.GetBytes(got, "tools.0.output_schema").Exists() || gjson.GetBytes(got, "tools.2.output_schema").Exists() || gjson.GetBytes(got, "tools.3.output_schema").Exists() {
		t.Fatalf("hosted tool output_schema should be removed; body=%s", got)
	}
}

func TestNormalizeCodexFinalUpstreamBodyCoalescesNamespaceToolsLikeOfficialCodex(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"instructions":"Be helpful.",
		"input":"hello",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__sample",
				"description":"",
				"tools":[
					{"type":"function","name":"z_last","parameters":{"type":"object","properties":{}}},
					{"type":"function","name":"a_first","parameters":{"type":"object","properties":{}}}
				]
			},
			{"type":"function","name":"plain","parameters":{"type":"object","properties":{}}},
			{
				"type":"namespace",
				"name":"mcp__sample",
				"description":"Sample tools.",
				"tools":[{"type":"function","name":"middle","parameters":{"type":"object","properties":{}}}]
			},
			{"type":"namespace","name":"mcp__empty","tools":null}
		]
	}`)

	got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if gotCount := gjson.GetBytes(got, "tools.#").Int(); gotCount != 3 {
		t.Fatalf("tools length = %d, want merged namespace + function + empty namespace; body=%s", gotCount, got)
	}
	if gotDescription := gjson.GetBytes(got, "tools.0.description").String(); gotDescription != "Sample tools." {
		t.Fatalf("merged namespace description = %q; body=%s", gotDescription, got)
	}
	for index, wantName := range []string{"a_first", "middle", "z_last"} {
		path := "tools.0.tools." + strconv.Itoa(index) + ".name"
		if gotName := gjson.GetBytes(got, path).String(); gotName != wantName {
			t.Fatalf("%s = %q, want %q; body=%s", path, gotName, wantName, got)
		}
	}
	if gotDescription := gjson.GetBytes(got, "tools.2.description").String(); gotDescription != "Tools in the mcp__empty namespace." {
		t.Fatalf("empty namespace description = %q; body=%s", gotDescription, got)
	}
	if gotCount := gjson.GetBytes(got, "tools.2.tools.#").Int(); gotCount != 0 {
		t.Fatalf("empty namespace tool count = %d, want 0; body=%s", gotCount, got)
	}
}

func TestNormalizeCodexFinalUpstreamBodyPreservesExplicitWebSearchModeAndConfig(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"instructions":"Be helpful.",
		"input":"hello",
		"tools":[{
			"type":"web_search",
			"external_web_access":true,
			"filters":{"allowed_domains":["example.com"]},
			"user_location":{"type":"approximate","country":"US","region":"California","city":"San Francisco","timezone":"America/Los_Angeles"},
			"search_context_size":"high",
			"search_content_types":["text","image"]
		}]
	}`)

	got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if gotExternal := gjson.GetBytes(got, "tools.0.external_web_access"); !gotExternal.Bool() {
		t.Fatalf("web_search external_web_access = %s, want true; body=%s", gotExternal.Raw, got)
	}
	if gotDomain := gjson.GetBytes(got, "tools.0.filters.allowed_domains.0").String(); gotDomain != "example.com" {
		t.Fatalf("web_search allowed domain = %q; body=%s", gotDomain, got)
	}
	if gotCity := gjson.GetBytes(got, "tools.0.user_location.city").String(); gotCity != "San Francisco" {
		t.Fatalf("web_search user_location.city = %q; body=%s", gotCity, got)
	}
	if gotSize := gjson.GetBytes(got, "tools.0.search_context_size").String(); gotSize != "high" {
		t.Fatalf("web_search search_context_size = %q; body=%s", gotSize, got)
	}
	if gotContentType := gjson.GetBytes(got, "tools.0.search_content_types.1").String(); gotContentType != "image" {
		t.Fatalf("web_search search_content_types missing image; body=%s", got)
	}
}

func TestNormalizeCodexFinalUpstreamBodyDropsNonFunctionNamespaceChildren(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"instructions":"Be helpful.",
		"input":"hello",
		"tools":[{
			"type":"namespace",
			"name":"mcp__sample",
			"description":"Sample namespace.",
			"tools":[
				{"type":"function","name":"keep_me","parameters":{"type":"object","properties":{}}},
				{"type":"custom","name":"drop_custom","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
				{"type":"web_search","filters":{"allowed_domains":["example.com"]}}
			]
		}]
	}`)

	got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if gotCount := gjson.GetBytes(got, "tools.0.tools.#").Int(); gotCount != 1 {
		t.Fatalf("namespace child count = %d, want 1; body=%s", gotCount, got)
	}
	if gotName := gjson.GetBytes(got, "tools.0.tools.0.name").String(); gotName != "keep_me" {
		t.Fatalf("namespace child name = %q, want keep_me; body=%s", gotName, got)
	}
	if gotType := gjson.GetBytes(got, "tools.0.tools.0.type").String(); gotType != "function" {
		t.Fatalf("namespace child type = %q, want function; body=%s", gotType, got)
	}
}

func TestNormalizeCodexFinalUpstreamBodyUnwrapsNestedCustomTools(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"instructions":"Be helpful.",
		"input":"hello",
		"tools":[{
			"type":"custom",
			"custom":{
				"name":"apply_patch",
				"description":"Apply patches.",
				"format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}
			},
			"parameters":{"type":"object"},
			"cache_control":{"type":"ephemeral"}
		}],
		"tool_choice":{
			"type":"allowed_tools",
			"mode":"any",
			"tools":[{
				"type":"custom",
				"custom":{"name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}
			}]
		}
	}`)

	got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if gotType := gjson.GetBytes(got, "tools.0.type").String(); gotType != "custom" {
		t.Fatalf("tools.0.type = %q, want custom; body=%s", gotType, got)
	}
	if gotName := gjson.GetBytes(got, "tools.0.name").String(); gotName != "apply_patch" {
		t.Fatalf("tools.0.name = %q, want apply_patch; body=%s", gotName, got)
	}
	if gotSyntax := gjson.GetBytes(got, "tools.0.format.syntax").String(); gotSyntax != "lark" {
		t.Fatalf("custom format syntax = %q, want lark; body=%s", gotSyntax, got)
	}
	for _, path := range []string{"tools.0.custom", "tools.0.parameters", "tools.0.cache_control"} {
		if gjson.GetBytes(got, path).Exists() {
			t.Fatalf("%s should be removed from custom tool; body=%s", path, got)
		}
	}
	if gotType := gjson.GetBytes(got, "tool_choice.tools.0.type").String(); gotType != "custom" {
		t.Fatalf("allowed custom type = %q, want custom; body=%s", gotType, got)
	}
	if gotName := gjson.GetBytes(got, "tool_choice.tools.0.name").String(); gotName != "apply_patch" {
		t.Fatalf("allowed custom name = %q, want apply_patch; body=%s", gotName, got)
	}
	if gjson.GetBytes(got, "tool_choice.tools.0.custom").Exists() {
		t.Fatalf("allowed custom tool should be a reference; body=%s", got)
	}
}

func TestCodexExecutorExecuteStreamNormalizesNullInstructions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses")
	}
	if got := gjson.GetBytes(gotBody, "instructions").String(); got != "You are a helpful assistant." {
		t.Fatalf("instructions = %q, want default instructions; body=%s", got, gotBody)
	}
}

func TestCodexExecutorCountTokensTreatsNullInstructionsAsEmpty(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})

	nullResp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("CountTokens(null) error: %v", err)
	}

	emptyResp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":"","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("CountTokens(empty) error: %v", err)
	}

	if string(nullResp.Payload) != string(emptyResp.Payload) {
		t.Fatalf("token count payload mismatch:\nnull=%s\nempty=%s", string(nullResp.Payload), string(emptyResp.Payload))
	}
}

func TestBuildCodexTokenCountTextCollectsRelevantSegments(t *testing.T) {
	body := []byte(`{
		"instructions":"be helpful",
		"input":[
			{"type":"message","content":[{"text":"hello"},{"text":" world "}]},
			{"type":"function_call","name":"tool","arguments":"{\"x\":1}"},
			{"type":"function_call_output","output":"ok"},
			{"type":"unknown","text":"fallback"}
		],
		"tools":[
			{"name":"tool","description":"desc","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}
		],
		"text":{"format":{"name":"schema_name","schema":{"type":"object"}}}
	}`)

	got := buildCodexTokenCountText(gjson.ParseBytes(body), len(body))
	want := "be helpful\nhello\nworld\ntool\n{\"x\":1}\nok\nfallback\ntool\ndesc\n{\"type\":\"object\",\"properties\":{\"x\":{\"type\":\"string\"}}}\nschema_name\n{\"type\":\"object\"}"
	if got != want {
		t.Fatalf("token count text mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestCodexTokenizerKeyNormalizesModelFamilies(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{model: "", want: "cl100k_base"},
		{model: "gpt-5.4-mini", want: "gpt-5"},
		{model: "GPT-5.3-CODEX", want: "gpt-5"},
		{model: " GPT-4.1-MINI ", want: "gpt-4.1"},
		{model: "gpt-4.1-mini", want: "gpt-4.1"},
		{model: "gpt-4o-mini", want: "gpt-4o"},
		{model: "gpt-4-turbo", want: "gpt-4"},
		{model: "gpt-3.5-turbo", want: "gpt-3.5"},
		{model: "unknown-model-for-codex", want: "cl100k_base"},
	}

	for _, tt := range cases {
		t.Run(tt.model, func(t *testing.T) {
			if got := codexTokenizerKey(tt.model); got != tt.want {
				t.Fatalf("codexTokenizerKey(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func BenchmarkCodexTokenizerKey(b *testing.B) {
	for b.Loop() {
		if got := codexTokenizerKey(" GPT-4.1-MINI "); got != "gpt-4.1" {
			b.Fatalf("codexTokenizerKey() = %q", got)
		}
	}
}
