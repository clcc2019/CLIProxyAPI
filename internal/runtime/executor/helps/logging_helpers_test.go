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
