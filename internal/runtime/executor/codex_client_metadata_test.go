package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
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

func TestCodexApplyWebsocketClientMetadataIncludesResponsesLiteHeader(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	headers := http.Header{
		codexWireHeaderOpenAIInternalCodexResponsesLite: []string{"true"},
	}

	got := codexApplyWebsocketClientMetadataWithStreamStartMS(context.Background(), body, headers, nil, nil, "123")

	if value := gjson.GetBytes(got, "client_metadata."+codexWSClientMetadataResponsesLite).String(); value != "true" {
		t.Fatalf("%s = %q, want true; body=%s", codexWSClientMetadataResponsesLite, value, got)
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

func TestCodexApplyHTTPClientMetadataIncludesOfficialIdentityProjection(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[],"client_metadata":{"session_id":"stale-session","thread_id":"stale-thread","turn_id":"stale-turn","x-codex-window-id":"stale-window","keep":"value"}}`)
	headers := http.Header{}
	headers.Set(codexHeaderInstallationID, "current-install")
	headers.Set(codexHeaderSessionID, "session-1")
	headers.Set(codexHeaderThreadID, "thread-1")
	headers.Set(codexHeaderWindowID, "thread-1:0")
	headers.Set(codexHeaderTurnMetadata, `{"session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","window_id":"thread-1:0"}`)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyHTTPClientMetadataWithSource(body, headers, nil, auth, nil)

	assertMetadata := func(path string, want string) {
		t.Helper()
		if gotValue := gjson.GetBytes(got, "client_metadata."+path).String(); gotValue != want {
			t.Fatalf("client_metadata.%s = %q, want %q; body=%s", path, gotValue, want, got)
		}
	}
	assertMetadata("x-codex-installation-id", "current-install")
	assertMetadata("session_id", "session-1")
	assertMetadata("thread_id", "thread-1")
	assertMetadata("turn_id", "turn-1")
	assertMetadata("x-codex-window-id", "thread-1:0")
	assertMetadata("x-codex-turn-metadata", `{"session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","window_id":"thread-1:0"}`)
	assertMetadata("keep", "value")
}

func TestCodexAppendJSONStringEscapesJSONStrings(t *testing.T) {
	for _, value := range []string{
		"plain-ascii",
		`quote"backslash\`,
		"line\nfeed",
		"nul\x00byte",
		"unicode-雪",
	} {
		raw := codexAppendJSONString(nil, value)
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("json.Unmarshal(%q) error = %v", string(raw), err)
		}
		if decoded != value {
			t.Fatalf("decoded = %q, want %q; raw=%s", decoded, value, raw)
		}
	}
}

func TestPrepareCodexHTTPCallProjectsGeneratedIdentityIntoClientMetadata(t *testing.T) {
	resetCodexWindowStateStore()
	executor := NewCodexExecutor(nil)
	payload := []byte(`{"model":"gpt-5-codex","input":[],"client_metadata":{"fiber_run_id":"fiber-1"}}`)
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: payload}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderSessionID: "session-1",
		codexHeaderThreadID:  "thread-1",
	})

	call, err := executor.prepareCodexHTTPCall(ctx, auth, sdktranslator.FromString("codex"), "", "https://example.com/responses", req, payload, "sk-test", true)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall error: %v", err)
	}
	body := call.prepared.body
	headers := call.prepared.httpReq.Header

	assertMetadata := func(path string, want string) {
		t.Helper()
		if gotValue := gjson.GetBytes(body, "client_metadata."+path).String(); gotValue != want {
			t.Fatalf("client_metadata.%s = %q, want %q; body=%s", path, gotValue, want, body)
		}
	}
	assertMetadata("session_id", "session-1")
	assertMetadata("thread_id", "thread-1")
	assertMetadata("x-codex-window-id", headers.Get(codexHeaderWindowID))
	assertMetadata("x-codex-turn-metadata", headers.Get(codexHeaderTurnMetadata))
	assertMetadata("fiber_run_id", "fiber-1")
	if turnID := gjson.GetBytes(body, "client_metadata.turn_id").String(); turnID == "" {
		t.Fatalf("client_metadata.turn_id should be generated; body=%s", body)
	}
	bodyReader, err := call.prepared.httpReq.GetBody()
	if err != nil {
		t.Fatalf("GetBody error: %v", err)
	}
	gotBody, err := io.ReadAll(bodyReader)
	if err != nil {
		t.Fatalf("ReadAll request body error: %v", err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("request body was not reset after client_metadata projection: req=%s prepared=%s", gotBody, body)
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

func TestCodexApplyWebsocketClientMetadataWithResponseCreateTypeAppendsType(t *testing.T) {
	resetCodexWindowStateStore()
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	headers := http.Header{}
	headers.Set(codexHeaderInstallationID, "install-1")
	headers.Set(codexHeaderWindowID, "window-1")
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test"}}

	got := codexApplyWebsocketClientMetadataWithResponseCreateType(context.Background(), body, headers, auth, nil, "1234")

	if typ := gjson.GetBytes(got, "type").String(); typ != "response.create" {
		t.Fatalf("type = %q, want response.create; body=%s", typ, got)
	}
	if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id != "install-1" {
		t.Fatalf("client_metadata.x-codex-installation-id = %q, want install-1; body=%s", id, got)
	}
	if start := gjson.GetBytes(got, "client_metadata.x-codex-ws-stream-request-start-ms").String(); start != "1234" {
		t.Fatalf("client_metadata.x-codex-ws-stream-request-start-ms = %q, want 1234; body=%s", start, got)
	}

	wsReqBody := buildCodexWebsocketRequestBodyWithCurrentTurnMetadata(got)
	if len(wsReqBody) == 0 || &wsReqBody[0] != &got[0] {
		t.Fatalf("websocket request body should reuse metadata body when type and input already exist; got %s want %s", wsReqBody, got)
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
	headers.Set(codexHeaderSessionID, "session-1")
	headers.Set(codexHeaderThreadID, "thread-1")
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
	assertMetadata("session_id", "session-1")
	assertMetadata("thread_id", "thread-1")
	assertMetadata("turn_id", "turn-1")
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

func TestCodexSetClientMetadataNormalizesEntriesOnce(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	entries := []codexClientMetadataEntry{
		{key: " duplicate ", value: " first "},
		{key: " keep ", value: " value "},
		{key: "duplicate", value: " second "},
		{key: "empty", value: "   "},
	}

	got := codexSetClientMetadata(body, entries, true)
	if value := gjson.GetBytes(got, "client_metadata.duplicate").String(); value != "second" {
		t.Fatalf("duplicate value = %q, want second; body=%s", value, got)
	}
	if value := gjson.GetBytes(got, "client_metadata.keep").String(); value != "value" {
		t.Fatalf("keep value = %q, want value; body=%s", value, got)
	}
	if gjson.GetBytes(got, "client_metadata.empty").Exists() {
		t.Fatalf("empty metadata entry should be omitted; body=%s", got)
	}
}

func TestCodexSetClientMetadataRecognizesEscapedFieldName(t *testing.T) {
	body := []byte(`{"client\u005fmetadata":{"keep":"value"}}`)
	got := codexSetClientMetadata(body, []codexClientMetadataEntry{{key: "added", value: "new"}}, true)

	if value := gjson.GetBytes(got, "client_metadata.keep").String(); value != "value" {
		t.Fatalf("existing escaped client_metadata value = %q, want value; body=%s", value, got)
	}
	if value := gjson.GetBytes(got, "client_metadata.added").String(); value != "new" {
		t.Fatalf("added client_metadata value = %q, want new; body=%s", value, got)
	}
	if bytes.Contains(got, []byte(`,"client_metadata":`)) {
		t.Fatalf("escaped client_metadata key was duplicated; body=%s", got)
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
