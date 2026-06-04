package executor

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func resetCodexWindowStateStore() {
	globalCodexWindowStateStore.reset()
}

func TestCodexApplyHTTPClientMetadataIncludesAPIKeyDefault(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyHTTPClientMetadata(body, req, auth, nil)

	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id == "" {
		t.Fatalf("API-key request should include client_metadata.x-codex-installation-id, got %s", got)
	}
}

func TestCodexApplyHTTPClientMetadataKeepsOAuthDefault(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}

	got := codexApplyHTTPClientMetadata(body, req, auth, nil)

	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id == "" {
		t.Fatalf("OAuth request should include client_metadata.x-codex-installation-id, got %s", got)
	}
}

func TestCodexApplyHTTPClientMetadataHonorsExistingAPIKeyClientMetadata(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[],"client_metadata":{}}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyHTTPClientMetadata(body, req, auth, nil)

	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id == "" {
		t.Fatalf("existing API-key client_metadata should be enriched, got %s", got)
	}
}

func TestCodexApplyHTTPClientMetadataKeepsOnlyStringMetadata(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[],"client_metadata":{"keep":"value","drop_number":123,"drop_object":{"x":"y"},"drop_null":null}}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyHTTPClientMetadata(body, req, auth, nil)

	if value := gjson.GetBytes(got, "client_metadata.keep").String(); value != "value" {
		t.Fatalf("client_metadata.keep = %q, want value; body=%s", value, got)
	}
	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id == "" {
		t.Fatalf("client_metadata.x-codex-installation-id missing; body=%s", got)
	}
	for _, field := range []string{"drop_number", "drop_object", "drop_null"} {
		if gjson.GetBytes(got, "client_metadata."+field).Exists() {
			t.Fatalf("client_metadata.%s should be removed from string-only metadata map; body=%s", field, got)
		}
	}
}

func TestCodexApplyHTTPClientMetadataOverwritesReservedInstallationID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[],"client_metadata":{"x-codex-installation-id":"stale-install","keep":"value"}}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set(codexHeaderInstallationID, "current-install")
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyHTTPClientMetadata(body, req, auth, nil)

	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id != "current-install" {
		t.Fatalf("client_metadata.x-codex-installation-id = %q, want current-install; body=%s", id, got)
	}
	if value := gjson.GetBytes(got, "client_metadata.keep").String(); value != "value" {
		t.Fatalf("client_metadata.keep = %q, want value; body=%s", value, got)
	}
}

func TestCodexApplyHTTPClientMetadataUsesPinnedAuthInstallationID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Metadata:   map[string]any{"access_token": "oauth-token"},
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	firstHeaders := http.Header{}
	firstHeaders.Set(codexHeaderInstallationID, "first-install")
	codexPinClientProfileFromFirstRequest(context.Background(), auth, nil, firstHeaders, nil)

	secondHeaders := http.Header{}
	secondHeaders.Set(codexHeaderInstallationID, "second-install")
	got := codexApplyHTTPClientMetadataWithSource(body, nil, codexClientProfileSourceHeaders(auth, secondHeaders), auth, nil)

	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id != "first-install" {
		t.Fatalf("client_metadata.x-codex-installation-id = %q, want first-install; body=%s", id, got)
	}
}

func TestCodexApplyWebsocketClientMetadataIncludesAPIKeyDefault(t *testing.T) {
	resetCodexWindowStateStore()
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	headers := http.Header{}
	headers.Set("Session_id", "session-1")
	codexEnsureResponsesIdentityHeaders(headers, nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyWebsocketClientMetadata(context.Background(), body, headers, auth, nil)

	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id == "" {
		t.Fatalf("API-key websocket body should include installation metadata, got %s", got)
	}
	if windowID := gjson.GetBytes(got, "client_metadata.x-codex-window-id").String(); windowID != "session-1:0" {
		t.Fatalf("client_metadata.x-codex-window-id = %q, want session-1:0; body=%s", windowID, got)
	}
}

func TestCodexEnsureResponsesIdentityHeadersTracksWindowGenerationBySession(t *testing.T) {
	resetCodexWindowStateStore()

	first := http.Header{}
	first.Set("Session_id", "session-1")
	codexEnsureResponsesIdentityHeaders(first, nil)
	if got := first.Get(codexHeaderWindowID); got != "session-1:0" {
		t.Fatalf("%s = %q, want %q", codexHeaderWindowID, got, "session-1:0")
	}

	codexAdvanceWindowGeneration("session-1")

	second := http.Header{}
	second.Set("Session_id", "session-1")
	codexEnsureResponsesIdentityHeaders(second, nil)
	if got := second.Get(codexHeaderWindowID); got != "session-1:1" {
		t.Fatalf("%s = %q, want %q", codexHeaderWindowID, got, "session-1:1")
	}
}

func TestCodexApplyWebsocketClientMetadataHonorsExplicitAPIKeyHeaders(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	headers := http.Header{}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}
	ctx := contextWithGinHeaders(map[string]string{"X-Codex-Window-Id": "window-1"})

	got := codexApplyWebsocketClientMetadata(ctx, body, headers, auth, nil)

	if windowID := gjson.GetBytes(got, "client_metadata.x-codex-window-id").String(); windowID != "window-1" {
		t.Fatalf("client_metadata.x-codex-window-id = %q, want window-1; body=%s", windowID, got)
	}
}

func TestCodexApplyWebsocketClientMetadataOverwritesReservedFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[],"client_metadata":{"x-codex-installation-id":"stale-install","x-codex-window-id":"stale-window","x-openai-subagent":"stale-subagent","x-codex-parent-thread-id":"stale-parent","x-codex-turn-metadata":"stale-turn","ws_request_header_traceparent":"stale-traceparent","ws_request_header_tracestate":"stale-tracestate","keep":"value"}}`)
	headers := http.Header{}
	headers.Set(codexHeaderInstallationID, "current-install")
	headers.Set(codexHeaderWindowID, "current-window")
	headers.Set("X-OpenAI-Subagent", "review")
	headers.Set(codexHeaderParentThreadID, "parent-1")
	headers.Set(codexHeaderTurnMetadata, `{"turn_id":"turn-1"}`)
	headers.Set("Traceparent", "00-current")
	headers.Set("Tracestate", "state-current")
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyWebsocketClientMetadata(context.Background(), body, headers, auth, nil)

	assertMetadata := func(path string, want string) {
		t.Helper()
		if gotValue := gjson.GetBytes(got, "client_metadata."+path).String(); gotValue != want {
			t.Fatalf("client_metadata.%s = %q, want %q; body=%s", path, gotValue, want, got)
		}
	}
	assertMetadata("x-codex-installation-id", "current-install")
	assertMetadata("x-codex-window-id", "current-window")
	assertMetadata("x-openai-subagent", "review")
	assertMetadata("x-codex-parent-thread-id", "parent-1")
	assertMetadata("x-codex-turn-metadata", `{"turn_id":"turn-1"}`)
	assertMetadata("ws_request_header_traceparent", "00-current")
	assertMetadata("ws_request_header_tracestate", "state-current")
	assertMetadata("keep", "value")
}

func TestCodexApplyWebsocketClientMetadataReplacesNonObjectMetadata(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[],"client_metadata":"invalid"}`)
	headers := http.Header{}
	headers.Set(codexHeaderInstallationID, "current-install")
	headers.Set(codexHeaderWindowID, "current-window")
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyWebsocketClientMetadata(context.Background(), body, headers, auth, nil)

	if !gjson.GetBytes(got, "client_metadata").IsObject() {
		t.Fatalf("client_metadata should be replaced with object; body=%s", got)
	}
	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id != "current-install" {
		t.Fatalf("client_metadata.x-codex-installation-id = %q, want current-install; body=%s", id, got)
	}
}

func BenchmarkCodexApplyWebsocketClientMetadataNoExistingMetadata(b *testing.B) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"hello"}],"tools":[],"stream":true}`)
	headers := http.Header{}
	headers.Set(codexHeaderInstallationID, "install-1")
	headers.Set(codexHeaderWindowID, "window-1")
	headers.Set(codexHeaderTurnMetadata, `{"turn_id":"turn-1"}`)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		got := codexApplyWebsocketClientMetadata(context.Background(), body, headers, auth, nil)
		if len(got) == 0 {
			b.Fatal("empty body")
		}
	}
}

func BenchmarkCodexApplyWebsocketClientMetadataExistingMetadata(b *testing.B) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"hello"}],"client_metadata":{"x-codex-installation-id":"stale-install","x-codex-window-id":"stale-window","x-codex-turn-metadata":"stale-turn","keep":"value"},"tools":[],"stream":true}`)
	headers := http.Header{}
	headers.Set(codexHeaderInstallationID, "install-1")
	headers.Set(codexHeaderWindowID, "window-1")
	headers.Set(codexHeaderTurnMetadata, `{"turn_id":"turn-1"}`)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		got := codexApplyWebsocketClientMetadata(context.Background(), body, headers, auth, nil)
		if len(got) == 0 {
			b.Fatal("empty body")
		}
	}
}
