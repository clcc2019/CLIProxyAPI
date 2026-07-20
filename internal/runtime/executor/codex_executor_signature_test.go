package executor

import (
	"context"
	"encoding/base64"
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

func validCodexReasoningEncryptedContentForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func newCodexSignatureTestAuth(serverURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": serverURL,
		"api_key":  "test",
	}}
}

func TestCodexExecutorPreservesGPT56SolReasoningForUpstream(t *testing.T) {
	for _, tt := range []struct {
		effort string
		want   string
	}{
		{effort: "xhigh", want: "xhigh"},
		{effort: "max", want: "max"},
		{effort: "Ultra", want: "ultra"},
	} {
		t.Run(tt.effort, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				gotBody = body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
				Model:   "gpt-5.6-sol(" + tt.effort + ")",
				Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Stream:       false,
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}

			if got := gjson.GetBytes(gotBody, "model").String(); got != "gpt-5.6-sol" {
				t.Fatalf("upstream model = %q, want gpt-5.6-sol; body=%s", got, string(gotBody))
			}
			if got := gjson.GetBytes(gotBody, "reasoning.effort").String(); got != tt.want {
				t.Fatalf("upstream reasoning.effort = %q, want %q; body=%s", got, tt.want, string(gotBody))
			}
		})
	}
}

func TestCodexExecutorDropsInvalidReasoningEncryptedContentFromFinalRequest(t *testing.T) {
	validEncryptedContent := validCodexReasoningEncryptedContentForTest()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[` +
			`{"id":"rs_bad","type":"reasoning","encrypted_content":"gAAAAABqFTIa\u2026abc","summary":[]},` +
			`{"id":"rs_non_string","type":"reasoning","encrypted_content":123,"summary":[]},` +
			`{"id":"rs_good","type":"reasoning","encrypted_content":"` + validEncryptedContent + `","summary":[]},` +
			`{"role":"user","content":"hello","encrypted_content":"leave-message-alone"}` +
			`]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid reasoning encrypted_content exists, want removed; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.1.encrypted_content").Exists() {
		t.Fatalf("non-string reasoning encrypted_content exists, want removed; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.0.id").Exists() || gjson.GetBytes(gotBody, "input.1.id").Exists() {
		t.Fatalf("invalid reasoning items retained orphan IDs with store disabled; body=%s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.2.encrypted_content").String(); got != validEncryptedContent {
		t.Fatalf("valid reasoning encrypted_content = %q, want preserved", got)
	}
	if got := gjson.GetBytes(gotBody, "input.3.encrypted_content").String(); got != "leave-message-alone" {
		t.Fatalf("non-reasoning encrypted_content = %q, want untouched", got)
	}
}

func TestSanitizeOpenAIResponsesReasoningDropsOrphanIDOnlyWhenStoreDisabled(t *testing.T) {
	body := []byte(`{"store":false,"input":[{"type":"reasoning","id":"rs-orphan","summary":[]},{"type":"message","id":"msg-1"}]}`)
	got := sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "test", body)
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("orphan reasoning ID was retained: %s", got)
	}
	if gjson.GetBytes(got, "input.1.id").String() != "msg-1" {
		t.Fatalf("non-reasoning ID changed: %s", got)
	}

	stored := []byte(`{"store":true,"input":[{"type":"reasoning","id":"rs-stored","summary":[]}]}`)
	got = sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "test", stored)
	if gjson.GetBytes(got, "input.0.id").String() != "rs-stored" {
		t.Fatalf("store=true reasoning ID was removed: %s", got)
	}
}

func TestCodexExecutorRetriesWithoutClientReasoningEncryptedContentAfterHTTP400(t *testing.T) {
	validEncryptedContent := validCodexReasoningEncryptedContentForTest()
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The encrypted content gAAA...ZF5s could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","code":"invalid_encrypted_content"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[` +
			`{"id":"rs_foreign","type":"reasoning","encrypted_content":"` + validEncryptedContent + `","summary":[]},` +
			`{"role":"user","content":"hello","encrypted_content":"leave-message-alone"}` +
			`]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute should retry without client encrypted_content: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream request count = %d, want 2", len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "input.0.encrypted_content").String(); got != validEncryptedContent {
		t.Fatalf("first request reasoning encrypted_content = %q, want preserved; body=%s", got, string(bodies[0]))
	}
	if gjson.GetBytes(bodies[1], "input.0.encrypted_content").Exists() {
		t.Fatalf("retry request reasoning encrypted_content exists, want removed; body=%s", string(bodies[1]))
	}
	if got := gjson.GetBytes(bodies[1], "input.1.encrypted_content").String(); got != "leave-message-alone" {
		t.Fatalf("retry request non-reasoning encrypted_content = %q, want untouched; body=%s", got, string(bodies[1]))
	}
}

func TestCodexExecutorExecuteStreamDropsInvalidReasoningEncryptedContentFromFinalRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","stream":true,"input":[{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}
	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid stream reasoning encrypted_content exists, want removed; body=%s", string(gotBody))
	}
}

func TestCodexExecutorExecuteStreamRetriesWithoutClientReasoningEncryptedContentAfterResponseFailed(t *testing.T) {
	validEncryptedContent := validCodexReasoningEncryptedContentForTest()
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"The encrypted content gAAA...ZF5s could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","code":"invalid_encrypted_content"}}}` + "\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","stream":true,"input":[{"id":"rs_foreign","type":"reasoning","encrypted_content":"` + validEncryptedContent + `","summary":[]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream setup error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream should retry without client encrypted_content, got chunk error: %v", chunk.Err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream request count = %d, want 2", len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "input.0.encrypted_content").String(); got != validEncryptedContent {
		t.Fatalf("first stream request reasoning encrypted_content = %q, want preserved; body=%s", got, string(bodies[0]))
	}
	if gjson.GetBytes(bodies[1], "input.0.encrypted_content").Exists() {
		t.Fatalf("retry stream request reasoning encrypted_content exists, want removed; body=%s", string(bodies[1]))
	}
}

func TestCodexExecutorCompactDropsInvalidReasoningEncryptedContentFromFinalRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute compact error: %v", err)
	}
	if gjson.GetBytes(gotBody, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid compact reasoning encrypted_content exists, want removed; body=%s", string(gotBody))
	}
}

func TestCodexExecutorCompactRetriesWithoutClientReasoningEncryptedContentAfterHTTP400(t *testing.T) {
	validEncryptedContent := validCodexReasoningEncryptedContentForTest()
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The encrypted content gAAA...ZF5s could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","code":"invalid_encrypted_content"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_2","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"id":"rs_foreign","type":"reasoning","encrypted_content":"` + validEncryptedContent + `","summary":[]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute compact should retry without client encrypted_content: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream request count = %d, want 2", len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "input.0.encrypted_content").String(); got != validEncryptedContent {
		t.Fatalf("first compact request reasoning encrypted_content = %q, want preserved; body=%s", got, string(bodies[0]))
	}
	if gjson.GetBytes(bodies[1], "input.0.encrypted_content").Exists() {
		t.Fatalf("retry compact request reasoning encrypted_content exists, want removed; body=%s", string(bodies[1]))
	}
}
