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
