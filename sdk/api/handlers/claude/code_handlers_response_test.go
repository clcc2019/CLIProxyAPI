package claude

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAppendClaudeAPIResponseRedactsSensitiveLogPayloads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	appendClaudeAPIResponse(c, []byte(`{"access_token":"secret-token","value":"visible"}`))
	appendClaudeAPIResponse(c, []byte("data: {\"api_key\":\"sk-secret\",\"value\":\"stream\"}\n\n"))

	value, exists := c.Get("API_RESPONSE")
	if !exists {
		t.Fatal("expected API_RESPONSE to be stored")
	}
	got := string(value.([]byte))
	if strings.Contains(got, "secret-token") || strings.Contains(got, "sk-secret") {
		t.Fatalf("Claude API_RESPONSE leaked sensitive value: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, "visible") || !strings.Contains(got, "stream") {
		t.Fatalf("Claude API_RESPONSE missing expected redacted content: %s", got)
	}
}
