package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexEncryptedRetryPreservesStoredToolAnchor(t *testing.T) {
	body := []byte(`{"store":true,"previous_response_id":"resp_anchor","input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":"encrypted_content","encrypted_content":"ciphertext"}]}]}`)
	got, changed := codexRetryBodyWithoutClientReasoningEncryptedContent(context.Background(), body, []byte(`{"error":{"code":"invalid_encrypted_content"}}`))
	if !changed || gjson.GetBytes(got, "input.0.output.0.type").String() != "input_text" {
		t.Fatalf("encrypted tool output was not sanitized: %s", got)
	}
	if gjson.GetBytes(got, "previous_response_id").String() != "resp_anchor" {
		t.Fatalf("retry removed the only anchor for the tool result: %s", got)
	}
}

func TestCodexToolOutputSanitizerPreservesPlainText(t *testing.T) {
	for _, output := range []string{
		"The encryptedContent field is documented here; preserve this tool result.",
		`{"description":"encrypted_content is an API field","count":3}`,
	} {
		body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": output}}})
		got, changed := dropOpenAIResponsesFunctionOutputEncryptedContent(context.Background(), "test", body, "test")
		if changed || string(got) != string(body) {
			t.Fatalf("ordinary tool output was changed: %s", got)
		}
	}
}

func TestCodexToolOutputSanitizerPreservesSerializedJSONContent(t *testing.T) {
	output := `{"content":[{"type":"text","text":"readable result"},{"type":"text","text":"ciphertext","_meta":{"codex/encryptedContent":true}}],"isError":false}`
	body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": output}}})
	got, changed := dropOpenAIResponsesFunctionOutputEncryptedContent(context.Background(), "test", body, "test")
	result := gjson.GetBytes(got, "input.0.output")
	if !changed || result.Type != gjson.String || !gjson.Valid(result.String()) {
		t.Fatalf("serialized tool result must remain a JSON string: %s", got)
	}
	if gjson.Get(result.String(), "content.0.text").String() != "readable result" ||
		gjson.Get(result.String(), "content.1.text").String() != openAIResponsesFunctionOutputEncryptedContentPlaceholder {
		t.Fatalf("sanitization lost readable content or retained ciphertext: %s", got)
	}
}

func TestCodexAzureDetectionUsesHost(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://resource.openai.azure.com/openai/v1", true},
		{"http://resource.openai.azure.com:8080/openai/v1", true},
		{"https://RESOURCE.OPENAI.AZURE.COM./openai/v1", true},
		{"https://resource.aoai.azure.com/openai/v1", true},
		{"https://resource.aoai.azure.us/openai/v1", true},
		{"https://resource.aoai.azure.cn/openai/v1", true},
		{"https://resource.aoai.azure.com.attacker.test/openai/v1", false},
		{"ftp://resource.openai.azure.com/openai/v1", false},
		{"https://resource.openai.azure.com@example.test/v1", false},
		{"https://resource.openai.azure.us/openai/v1", true},
		{"https://resource.cognitiveservices.azure.cn/openai/v1", true},
		{"https://resource.services.ai.azure.com/openai/v1", true},
		{"https://gateway.azure-api.net/openai/v1", true},
		{"https://resource.openai.azure.com.attacker.test/openai/v1", false},
		{"https://example.test/openai.azure.com/responses", false},
		{"https://example.test/v1?target=ai.azure.com", false},
		{"https://notai.azure.com/v1", false},
	} {
		if got := codexMatchesAzureResponsesBaseURL(tc.url); got != tc.want {
			t.Errorf("Azure detection for %s = %t, want %t", tc.url, got, tc.want)
		}
	}
}

func TestCodexBodyMemoSeparatesUpstreamPolicies(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"call_memo_scope","name":"tool","arguments":"{}"},{"type":"function_call_output","call_id":"call_memo_scope","output":[{"type":"encrypted_content","encrypted_content":"ciphertext"}]}]}`)
	opts := codexFinalUpstreamBodyOptions{requestKind: codexFinalUpstreamResponses, suppressDefaultInstructions: true}
	for _, tc := range []struct{ baseURL, want string }{
		{"https://chatgpt.com/backend-api/codex", "encrypted_content"},
		{"https://resource.services.ai.azure.com/openai/v1", "input_text"},
		{"https://chatgpt.com/backend-api/codex", "encrypted_content"},
	} {
		auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": tc.baseURL}}
		got := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", auth, opts)
		if result := gjson.GetBytes(got, `input.#(type=="function_call_output").output.0.type`).String(); result != tc.want {
			t.Fatalf("cached body used wrong policy for %s: %s", tc.baseURL, got)
		}
	}
}

func TestCodexAzureHTTPDoesNotForwardCodexTurnState(t *testing.T) {
	for _, endpoint := range []string{"/responses", "/responses/compact"} {
		t.Run(endpoint, func(t *testing.T) {
			g, _ := gin.CreateTestContext(httptest.NewRecorder())
			g.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			g.Request.Header.Set(codexHeaderTurnState, "foreign-codex-state")
			ctx := context.WithValue(context.Background(), "gin", g)
			auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{
				"base_url": "https://resource.openai.azure.com/openai/v1", "api_key": "test",
			}}
			body := []byte(`{"instructions":"Keep the current system instructions.","previous_response_id":"resp_1","input":[{"role":"user","content":"continue"}]}`)
			e := NewCodexExecutor(&config.Config{})
			call, err := e.prepareCodexHTTPCall(ctx, auth, sdktranslator.FromString("openai-response"), "session", auth.Attributes["base_url"]+endpoint, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: body}, body, "test", true)
			if err != nil {
				t.Fatal(err)
			}
			if got := call.prepared.httpReq.Header.Get(codexHeaderTurnState); got != "" {
				t.Fatalf("Azure received Codex turn state: %q", got)
			}
			if gjson.GetBytes(call.prepared.body, "instructions").String() != "Keep the current system instructions." {
				t.Fatalf("Azure continuation lost explicit instructions: %s", call.prepared.body)
			}

		})
	}
}

func TestCodexHTTP500EncryptedOutputUsesSingleSanitizedRetry(t *testing.T) {
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if gjson.GetBytes(body, `input.#(type=="function_call_output").output.0.type`).String() == "encrypted_content" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_encrypted_content","message":"Encrypted function output content could not be decrypted or decoded."}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()
	auth := newCodexSignatureTestAuth(server.URL)
	auth.Provider = "codex"
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":[{"type":"encrypted_content","encrypted_content":"ciphertext"}]}]}`)
	_, err := NewCodexExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("deterministic ciphertext error retried unchanged: got %d requests, want 2", len(bodies))
	}
}

func TestCodexAuthFileCredentialsReachUpstream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     string
		apiKey   bool
	}{
		{"api_key", map[string]any{"api_key": " file-key ", "auth_kind": "api_key", "access_token": "inactive-token"}, "file-key", true},
		{"access_token_mode", map[string]any{"api_key": "inactive-key", "access_token": "active-token", "auth_mode": "access_token"}, "active-token", false},
		{"accessToken_mode", map[string]any{"api_key": "inactive-key", "accessToken": "active-token", "authMode": "accessToken"}, "active-token", false},
		{"oauth", map[string]any{"access_token": "file-token"}, "file-token", false},
		{"oauth_over_stale_mirror", map[string]any{"api_key": "old-token", "accessToken": "fresh-token", "auth_kind": "oauth"}, "fresh-token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/custom/v1/responses" {
					t.Errorf("path = %q", r.URL.Path)
				}
				received <- r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_file\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer server.Close()
			tc.metadata["type"] = "codex"
			tc.metadata["base_url"] = " " + server.URL + "/custom/v1 "
			data, err := json.Marshal(tc.metadata)
			if err != nil {
				t.Fatal(err)
			}
			auth, err := cliproxyauth.NewAuthFromAuthFileData(data, cliproxyauth.AuthFileProjectionOptions{ID: t.Name() + ".json"})
			if err != nil {
				t.Fatal(err)
			}
			if got := codexIsAPIKeyAuth(auth); got != tc.apiKey {
				t.Fatalf("API key classification = %t, want %t", got, tc.apiKey)
			}
			body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
			_, err = NewCodexExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-received:
				if got != "Bearer "+tc.want {
					t.Fatalf("Authorization = %q, want Bearer %s", got, tc.want)
				}
			default:
				t.Fatal("configured upstream received no request")
			}
		})
	}
}

func TestCodexToolOutputSanitizerHandlesSerializedSnakeCaseCiphertext(t *testing.T) {
	output := `{"content":[{"type":"text","text":"readable result"},{"type":"encrypted_content","encrypted_content":"foreign-ciphertext"}]}`
	body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_json", "output": output}}})
	if err != nil {
		t.Fatal(err)
	}
	got, changed := dropOpenAIResponsesFunctionOutputEncryptedContent(context.Background(), "test", body, "test")
	result := gjson.GetBytes(got, "input.0.output").String()
	if !changed || gjson.Get(result, "content.0.text").String() != "readable result" || gjson.Get(result, "content.1.text").String() != openAIResponsesFunctionOutputEncryptedContentPlaceholder {
		t.Fatalf("serialized encrypted tool result not sanitized: %s", got)
	}
}
