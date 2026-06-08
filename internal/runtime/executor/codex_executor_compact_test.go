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

func TestCodexExecutorCompactAddsDefaultInstructions(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing instructions",
			payload: `{"model":"gpt-5.4","input":"hello"}`,
		},
		{
			name:    "null instructions",
			payload: `{"model":"gpt-5.4","instructions":null,"input":"hello"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  "test",
			}}

			resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(tc.payload),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Alt:          "responses/compact",
				Stream:       false,
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if gotPath != "/responses/compact" {
				t.Fatalf("path = %q, want %q", gotPath, "/responses/compact")
			}
			if got := gjson.GetBytes(gotBody, "instructions").String(); got != "You are a helpful assistant." {
				t.Fatalf("instructions = %q, want default instructions; body=%s", got, string(gotBody))
			}
			if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
				t.Fatalf("payload = %s", string(resp.Payload))
			}
		})
	}
}

func TestCodexExecutorCompactUsesCompactOnlyBodyFields(t *testing.T) {
	resetCodexWindowStateStore()
	var gotBody []byte
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[]}`))
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
			"input":"hello",
			"store":true,
			"stream":true,
			"tool_choice":"required",
			"include":["reasoning.encrypted_content"],
			"service_tier":"priority",
			"prompt_cache_key":"pc-1",
			"previous_response_id":"resp_1",
			"client_metadata":{"x-codex-installation-id":"install-1"}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	for _, field := range []string{"store", "stream", "tool_choice", "include", "client_metadata"} {
		if gjson.GetBytes(gotBody, field).Exists() {
			t.Fatalf("%s should not be sent to responses/compact: %s", field, gotBody)
		}
	}
	if got := gjson.GetBytes(gotBody, "prompt_cache_key").String(); got != "pc-1" {
		t.Fatalf("prompt_cache_key = %q, want pc-1; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "service_tier"); got.Exists() {
		t.Fatalf("service_tier should be omitted for API-key responses/compact requests; got %s body=%s", got.Raw, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "tools").IsArray(); !got {
		t.Fatalf("tools should default to an empty array for compact: %s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "parallel_tool_calls").Bool(); !got {
		t.Fatalf("parallel_tool_calls = false, want true; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "previous_response_id"); got.Exists() {
		t.Fatalf("previous_response_id should not be sent to responses/compact: %s", gotBody)
	}
	assertCodexTurnMetadataString(t, gotHeaders.Get(codexHeaderTurnMetadata), "request_kind", codexCompactionRequestKind)
	assertCodexTurnMetadataString(t, gotHeaders.Get(codexHeaderTurnMetadata), "window_id", gotHeaders.Get(codexHeaderWindowID))
	assertCodexTurnMetadataString(t, gotHeaders.Get(codexHeaderTurnMetadata), "compaction.implementation", codexDefaultCompactionImplementation)
	assertCodexTurnMetadataString(t, gotHeaders.Get(codexHeaderTurnMetadata), "compaction.strategy", codexDefaultCompactionStrategy)
	if got := gotHeaders.Get(codexHeaderTurnState); got != "" {
		t.Fatalf("%s should not be sent by default to responses/compact: %q", codexHeaderTurnState, got)
	}
	if got := gotHeaders.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id should not be sent by default to responses/compact: %q", got)
	}
	if got := gotHeaders.Get(codexHeaderSessionID); got == "" {
		t.Fatalf("%s should be present on responses/compact", codexHeaderSessionID)
	}
	if got := gotHeaders.Get(codexHeaderInstallationID); got == "" {
		t.Fatalf("%s should be present on responses/compact", codexHeaderInstallationID)
	}
}

func TestCodexExecutorCompactPrunesOldContextAfterContextLengthError(t *testing.T) {
	var gotBodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "application/json")
		if len(gotBodies) <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`Your input exceeds the context window of this model. Please adjust your input and try again.`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"old-1"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old-2"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"old-3"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old-4"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"middle-5"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"middle-6"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"latest-7"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"latest-8"}]}
			]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(gotBodies) != 3 {
		t.Fatalf("request count = %d, want 3", len(gotBodies))
	}
	if got := gjson.GetBytes(gotBodies[0], "input.#").Int(); got != 8 {
		t.Fatalf("first request input length = %d, want 8; body=%s", got, gotBodies[0])
	}
	if got := gjson.GetBytes(gotBodies[1], "input.#").Int(); got != 4 {
		t.Fatalf("second request input length = %d, want 4; body=%s", got, gotBodies[1])
	}
	if got := gjson.GetBytes(gotBodies[2], "input.#").Int(); got != 2 {
		t.Fatalf("third request input length = %d, want 2; body=%s", got, gotBodies[2])
	}
	if got := gjson.GetBytes(gotBodies[2], "input.0.content.0.text").String(); got != "latest-7" {
		t.Fatalf("third request first kept text = %q, want latest-7; body=%s", got, gotBodies[2])
	}
	if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestCodexExecutorCompactPrunesSingleMessageTextAfterContextLengthError(t *testing.T) {
	var gotBodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "application/json")
		if len(gotBodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction"}`))
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
			"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"oldernewer"}]}]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(gotBodies) != 2 {
		t.Fatalf("request count = %d, want 2", len(gotBodies))
	}
	if got := gjson.GetBytes(gotBodies[1], "input.0.content.0.text").String(); got != "newer" {
		t.Fatalf("second request text = %q, want newer; body=%s", got, gotBodies[1])
	}
}

func TestCodexExecutorCompactPreservesServiceTierForOptedInChatGPTAuth(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[]}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "oauth-token",
		},
		Metadata: map[string]any{
			"access_token": "oauth-token",
			"account_id":   "account-1",
			cliproxyauth.AuthFileServiceTierPassthroughKey: true,
		},
	}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"input":"hello",
			"service_tier":"priority"
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority for opted-in ChatGPT auth; body=%s", got, gotBody)
	}
}

func TestCodexExecutorCompactAdvancesWindowGenerationForSession(t *testing.T) {
	resetCodexWindowStateStore()

	firstReq, err := http.NewRequestWithContext(
		contextWithGinHeaders(map[string]string{codexHeaderSessionID: "conv-1"}),
		http.MethodPost,
		"https://example.com/responses",
		nil,
	)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error: %v", err)
	}
	applyCodexHeaders(firstReq, nil, "oauth-token", true, nil)
	if got := firstReq.Header.Get(codexHeaderWindowID); got != "conv-1:0" {
		t.Fatalf("initial %s = %q, want %q", codexHeaderWindowID, got, "conv-1:0")
	}

	var compactWindowID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compactWindowID = r.Header.Get(codexHeaderWindowID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderSessionID: "conv-1",
	})

	request := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}

	if _, err := executor.Execute(ctx, auth, request, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	}); err != nil {
		t.Fatalf("compact Execute error: %v", err)
	}

	if compactWindowID != "conv-1:0" {
		t.Fatalf("compact %s = %q, want %q", codexHeaderWindowID, compactWindowID, "conv-1:0")
	}

	secondReq, err := http.NewRequestWithContext(
		contextWithGinHeaders(map[string]string{codexHeaderSessionID: "conv-1"}),
		http.MethodPost,
		"https://example.com/responses",
		nil,
	)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error: %v", err)
	}
	applyCodexHeaders(secondReq, nil, "oauth-token", true, nil)
	if got := secondReq.Header.Get(codexHeaderWindowID); got != "conv-1:1" {
		t.Fatalf("post-compact %s = %q, want %q", codexHeaderWindowID, got, "conv-1:1")
	}
}

func TestCodexExecutorCompactUsesTurnMetadataSessionIDWhenHeaderMissing(t *testing.T) {
	resetCodexWindowStateStore()

	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderTurnMetadata: `{"session_id":"turn-session-1","turn_id":"turn-1","sandbox":"none"}`,
	})

	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("compact Execute error: %v", err)
	}

	if got := gotHeaders.Get(codexHeaderSessionID); got != "turn-session-1" {
		t.Fatalf("%s = %q, want %q", codexHeaderSessionID, got, "turn-session-1")
	}
	if got := gotHeaders.Get(codexHeaderWindowID); got != "turn-session-1:0" {
		t.Fatalf("%s = %q, want %q", codexHeaderWindowID, got, "turn-session-1:0")
	}
	assertCodexTurnMetadataString(t, gotHeaders.Get(codexHeaderTurnMetadata), "session_id", "turn-session-1")
	assertCodexTurnMetadataString(t, gotHeaders.Get(codexHeaderTurnMetadata), "turn_id", "turn-1")
	assertCodexTurnMetadataString(t, gotHeaders.Get(codexHeaderTurnMetadata), "request_kind", codexCompactionRequestKind)
	assertCodexTurnMetadataString(t, gotHeaders.Get(codexHeaderTurnMetadata), "window_id", gotHeaders.Get(codexHeaderWindowID))

	nextReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://example.com/responses",
		nil,
	)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error: %v", err)
	}
	applyCodexHeaders(nextReq, nil, "oauth-token", true, nil)
	if got := nextReq.Header.Get(codexHeaderSessionID); got != "turn-session-1" {
		t.Fatalf("next %s = %q, want %q", codexHeaderSessionID, got, "turn-session-1")
	}
	if got := nextReq.Header.Get(codexHeaderWindowID); got != "turn-session-1:1" {
		t.Fatalf("next %s = %q, want %q", codexHeaderWindowID, got, "turn-session-1:1")
	}
}
