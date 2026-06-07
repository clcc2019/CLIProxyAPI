package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestFormatAuthInfoAuthTypeNormalization(t *testing.T) {
	tests := []struct {
		name string
		info UpstreamRequestLog
		want string
	}{
		{name: "api key mixed case", info: UpstreamRequestLog{AuthType: " API_Key "}, want: "type=api_key"},
		{name: "oauth mixed case", info: UpstreamRequestLog{AuthType: "\tOAuth\r\n"}, want: "type=oauth"},
		{name: "unknown lowercased", info: UpstreamRequestLog{AuthType: " Custom ", AuthValue: "value"}, want: "type=custom value=value"},
		{name: "ordered fields", info: UpstreamRequestLog{Provider: " openai ", AuthID: " auth-1 ", AuthLabel: " primary ", AuthType: "oauth"}, want: "provider=openai, auth_id=auth-1, label=primary, type=oauth"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatAuthInfo(tt.info); got != tt.want {
				t.Fatalf("formatAuthInfo() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWebsocketUpgradeRequestURLMatchesMixedCaseScheme(t *testing.T) {
	got := WebsocketUpgradeRequestURL("WSS://chatgpt.com/backend-api/codex/responses")
	if got != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("request URL = %q, want https://chatgpt.com/backend-api/codex/responses", got)
	}
}

func BenchmarkFormatAuthInfoAPIKeyType(b *testing.B) {
	info := UpstreamRequestLog{AuthType: " API_Key "}
	for b.Loop() {
		if got := formatAuthInfo(info); got != "type=api_key" {
			b.Fatalf("formatAuthInfo() = %q", got)
		}
	}
}

func TestSummarizeErrorBodyMixedCaseHTMLContentType(t *testing.T) {
	body := []byte("<html><head><title>Upstream Error</title></head><body>secret details</body></html>")
	if got := SummarizeErrorBody("Text/HTML; charset=utf-8", body); got != "Upstream Error" {
		t.Fatalf("SummarizeErrorBody() = %q, want title", got)
	}
}

func TestSummarizeErrorBodySniffsMixedCaseHTMLBody(t *testing.T) {
	body := []byte(" \n\t<HTML><HEAD><TITLE> Upstream &amp; Error </TITLE></HEAD><BODY>secret details</BODY></HTML>")
	if got := SummarizeErrorBody("", body); got != "Upstream & Error" {
		t.Fatalf("SummarizeErrorBody() = %q, want title", got)
	}
}

func BenchmarkSummarizeErrorBodyHTMLContentType(b *testing.B) {
	body := []byte("<html><head><title>Upstream Error</title></head><body>secret details</body></html>")
	for b.Loop() {
		if got := SummarizeErrorBody("Text/HTML; charset=utf-8", body); got != "Upstream Error" {
			b.Fatalf("SummarizeErrorBody() = %q", got)
		}
	}
}

func BenchmarkSummarizeErrorBodyHTMLBodySniff(b *testing.B) {
	body := []byte(" \n\t<HTML><HEAD><TITLE> Upstream &amp; Error </TITLE></HEAD><BODY>secret details</BODY></HTML>")
	for b.Loop() {
		if got := SummarizeErrorBody("", body); got != "Upstream & Error" {
			b.Fatalf("SummarizeErrorBody() = %q", got)
		}
	}
}

func TestRecordAPIResponseMetadataStoresHeadersWhenRequestLogDisabled(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	headers := http.Header{}
	headers.Add("X-Upstream-Request-Id", "upstream-req-1")

	RecordAPIResponseMetadata(ctx, &config.Config{}, http.StatusOK, headers)
	headers.Set("X-Upstream-Request-Id", "mutated")

	got := logging.GetResponseHeaders(ctx)
	if got.Get("X-Upstream-Request-Id") != "upstream-req-1" {
		t.Fatalf("response header = %q, want %q", got.Get("X-Upstream-Request-Id"), "upstream-req-1")
	}
}

func TestAppendAPIResponseChunkRedactsSensitiveJSONAndSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), "gin", c)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	RecordAPIRequest(ctx, cfg, UpstreamRequestLog{URL: "https://upstream.test", Method: http.MethodPost})
	AppendAPIResponseChunk(ctx, cfg, []byte(`{"access_token":"secret-token","value":"visible"}`))
	AppendAPIResponseChunk(ctx, cfg, []byte("data: {\"api_key\":\"sk-secret\",\"value\":\"stream\"}\n\n"))

	got := apiResponseLogText(t, c)
	if strings.Contains(got, "secret-token") || strings.Contains(got, "sk-secret") {
		t.Fatalf("response log leaked sensitive value: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, "visible") || !strings.Contains(got, "stream") {
		t.Fatalf("response log missing expected redacted content: %s", got)
	}
}

func TestAppendAPIWebsocketResponseRedactsSensitiveJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), "gin", c)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	AppendAPIWebsocketResponse(ctx, cfg, []byte(`{"type":"response.completed","response":{"id":"resp-1"},"access_token":"secret-token"}`))

	value, exists := c.Get(apiWebsocketTimelineKey)
	if !exists {
		t.Fatal("expected websocket timeline to be stored")
	}
	timeline, ok := value.([]byte)
	if !ok {
		t.Fatalf("websocket timeline type = %T, want []byte", value)
	}
	got := string(timeline)
	if strings.Contains(got, "secret-token") {
		t.Fatalf("websocket response log leaked sensitive value: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, "response.completed") {
		t.Fatalf("websocket response log missing expected redacted content: %s", got)
	}
}

func TestRecordAPIWebsocketHandshakeLogsCodexMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx := logging.WithResponseHeadersHolder(context.WithValue(context.Background(), "gin", c))
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
	headers := http.Header{
		"X-Codex-Turn-State":   []string{"turn-state-1"},
		"X-Reasoning-Included": []string{""},
		"X-Models-Etag":        []string{"etag-1"},
		"OpenAI-Model":         []string{"gpt-5-codex"},
	}

	RecordAPIWebsocketHandshake(ctx, cfg, http.StatusSwitchingProtocols, headers)

	value, exists := c.Get(apiWebsocketTimelineKey)
	if !exists {
		t.Fatal("expected websocket timeline to be stored")
	}
	timeline, ok := value.([]byte)
	if !ok {
		t.Fatalf("websocket timeline type = %T, want []byte", value)
	}
	got := string(timeline)
	for _, want := range []string{
		"Event: api.websocket.handshake",
		"Metadata:",
		"x-codex-turn-state: turn-state-1",
		"x-reasoning-included: true",
		"x-models-etag: etag-1",
		"openai-model: gpt-5-codex",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("websocket handshake log missing %q in %s", want, got)
		}
	}

	gotHeaders := logging.GetResponseHeaders(ctx)
	if got := firstHeaderValueCaseInsensitive(gotHeaders, "openai-model"); got != "gpt-5-codex" {
		t.Fatalf("stored response headers openai-model = %q, want gpt-5-codex", got)
	}
}

func apiResponseLogText(t *testing.T, c *gin.Context) string {
	t.Helper()
	value, exists := c.Get(apiResponseKey)
	if !exists {
		t.Fatal("expected API_RESPONSE to be stored")
	}
	switch typed := value.(type) {
	case *strings.Builder:
		return typed.String()
	case []byte:
		return string(typed)
	case string:
		return typed
	default:
		t.Fatalf("API_RESPONSE type = %T", value)
		return ""
	}
}
