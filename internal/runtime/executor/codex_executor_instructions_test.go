package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
			"type":"allowed_tools",
			"mode":"any",
			"cache_control":{"type":"ephemeral"},
			"tools":[
				{"type":"function","name":"Read","description":"Read files","strict":false,"parameters":{"type":"object"},"cache_control":{"type":"ephemeral"}},
				{"type":"mcp","server_label":"codex_apps","name":"calendar.create_event","authorization":"secret"},
				{"type":"web_search_20250305","name":"web_search","filters":{"allowed_domains":["example.com"]}},
				{"type":"image_generation","output_format":"png"},
				{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
				{"type":"computer_use","display_width":1024}
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
					"parameters":{"type":"object","properties":{}}
				}]
				},
				{"type":"tool_search","execution":"sync","description":"Search for tools.","parameters":{"type":"object","properties":{}},"cache_control":{"type":"ephemeral"}},
				{"type":"image_generation","output_format":"png","cache_control":{"type":"ephemeral"}},
				{"type":"custom","name":"apply_patch","description":"Apply patches.","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}
			],
			"tool_choice":{"type":"custom","name":"apply_patch"}
	}`)

	got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if gotCount := gjson.GetBytes(got, "tools.#").Int(); gotCount != 4 {
		t.Fatalf("tools length = %d, want 4; body=%s", gotCount, got)
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
	if gotType := gjson.GetBytes(got, "tools.1.type").String(); gotType != "tool_search" {
		t.Fatalf("tools.1.type = %q, want tool_search; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tools.1.cache_control").Exists() {
		t.Fatalf("tool_search cache_control should be removed; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tools.2.type").String(); gotType != "image_generation" {
		t.Fatalf("tools.2.type = %q, want image_generation; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tools.2.cache_control").Exists() {
		t.Fatalf("image_generation cache_control should be removed; body=%s", got)
	}
	if gotType := gjson.GetBytes(got, "tools.3.type").String(); gotType != "custom" {
		t.Fatalf("tools.3.type = %q, want custom; body=%s", gotType, got)
	}
	if gotSyntax := gjson.GetBytes(got, "tools.3.format.syntax").String(); gotSyntax != "lark" {
		t.Fatalf("custom format syntax = %q, want lark; body=%s", gotSyntax, got)
	}
	if gotType := gjson.GetBytes(got, "tool_choice.type").String(); gotType != "custom" {
		t.Fatalf("tool_choice.type = %q, want custom; body=%s", gotType, got)
	}
	if gjson.GetBytes(got, "tools.3.parameters").Exists() {
		t.Fatalf("custom tool should not be converted to function parameters; body=%s", got)
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
