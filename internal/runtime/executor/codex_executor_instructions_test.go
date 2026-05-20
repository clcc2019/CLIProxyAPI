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
				{"name":"Read","description":"Read files","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}},
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
