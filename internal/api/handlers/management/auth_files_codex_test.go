package management

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCodexLoginRequestUserAgentUsesNonWebUIHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/codex-auth-url", nil)
	req.Header.Set("User-Agent", "codex-cli-test/1.0")
	ctx.Request = req

	if got := codexLoginRequestUserAgent(ctx); got != "codex-cli-test/1.0" {
		t.Fatalf("codexLoginRequestUserAgent() = %q, want %q", got, "codex-cli-test/1.0")
	}
}

func TestCodexLoginRequestUserAgentSkipsWebUIBrowserHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/codex-auth-url?is_webui=true", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	ctx.Request = req

	if got := codexLoginRequestUserAgent(ctx); got != "" {
		t.Fatalf("codexLoginRequestUserAgent() = %q, want empty", got)
	}
}

func TestExtractCodexIDTokenClaimsFallsBackToMetadata(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"account_id": "acct_123",
			"plan_type":  "plus",
		},
	}

	got := extractCodexIDTokenClaims(auth)
	if got["chatgpt_account_id"] != "acct_123" {
		t.Fatalf("chatgpt_account_id = %#v, want %q", got["chatgpt_account_id"], "acct_123")
	}
	if got["plan_type"] != "plus" {
		t.Fatalf("plan_type = %#v, want %q", got["plan_type"], "plus")
	}
}
