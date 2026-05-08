package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
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
	if gjson.GetBytes(gotBody, "instructions").Exists() {
		t.Fatalf("instructions should be omitted when empty, got %s", gotBody)
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
	if gjson.GetBytes(gotBody, "instructions").Exists() {
		t.Fatalf("instructions should be omitted when empty, got %s", gotBody)
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
