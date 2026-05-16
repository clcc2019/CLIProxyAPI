package management

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestExtractCodexIDTokenClaimsIncludesSubscriptionUntil(t *testing.T) {
	idToken := testJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "acct_123",
			"chatgpt_plan_type":                 "pro",
			"chatgpt_subscription_active_until": "2026-06-01T00:00:00Z",
		},
	})
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"id_token": idToken},
	}

	got := extractCodexIDTokenClaims(auth)
	if got["chatgpt_subscription_active_until"] != "2026-06-01T00:00:00Z" {
		t.Fatalf("chatgpt_subscription_active_until = %#v", got["chatgpt_subscription_active_until"])
	}
	if got["plan_type"] != "pro" {
		t.Fatalf("plan_type = %#v, want %q", got["plan_type"], "pro")
	}
}

func TestCodexSubscriptionUntilValueAcceptsSubscriptionExpiresAt(t *testing.T) {
	got, ok := codexSubscriptionUntilValue(map[string]any{
		"subscription_expires_at": " 2026-06-01T00:00:00Z ",
	})
	if !ok {
		t.Fatal("codexSubscriptionUntilValue() ok = false, want true")
	}
	if got != "2026-06-01T00:00:00Z" {
		t.Fatalf("codexSubscriptionUntilValue() = %#v", got)
	}
}

func TestParseCodexAccountSubscriptionInfoMatchesOrgID(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{
		"accounts": {
			"org_free": {
				"account": {"plan_type": "free", "email": "free@example.com", "is_default": true},
				"entitlement": {"subscription_plan": "free"}
			},
			"org_pro": {
				"account": {"plan_type": "pro", "email": "pro@example.com"},
				"entitlement": {"subscription_plan": "pro", "expires_at": "2026-06-01T00:00:00Z"}
			}
		}
	}`), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	got := parseCodexAccountSubscriptionInfo(payload, "org_pro")
	if got == nil {
		t.Fatal("parseCodexAccountSubscriptionInfo() = nil")
	}
	if got.PlanType != "pro" {
		t.Fatalf("PlanType = %q, want pro", got.PlanType)
	}
	if got.Email != "pro@example.com" {
		t.Fatalf("Email = %q, want pro@example.com", got.Email)
	}
	if got.SubscriptionExpiresAt != "2026-06-01T00:00:00Z" {
		t.Fatalf("SubscriptionExpiresAt = %q", got.SubscriptionExpiresAt)
	}
}

func TestParseCodexAccountSubscriptionInfoFallsBackToDefault(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{
		"accounts": {
			"org_default": {
				"account": {"plan_type": "plus", "email": "plus@example.com", "is_default": true},
				"entitlement": {"subscription_plan": "plus", "expires_at": "2026-07-01T00:00:00Z"}
			},
			"org_other": {
				"account": {"plan_type": "pro", "email": "pro@example.com"},
				"entitlement": {"subscription_plan": "pro", "expires_at": "2026-08-01T00:00:00Z"}
			}
		}
	}`), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	got := parseCodexAccountSubscriptionInfo(payload, "missing")
	if got == nil {
		t.Fatal("parseCodexAccountSubscriptionInfo() = nil")
	}
	if got.Email != "plus@example.com" {
		t.Fatalf("Email = %q, want plus@example.com", got.Email)
	}
	if got.SubscriptionExpiresAt != "2026-07-01T00:00:00Z" {
		t.Fatalf("SubscriptionExpiresAt = %q", got.SubscriptionExpiresAt)
	}
}

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(value map[string]any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return strings.Join([]string{
		encode(map[string]any{"alg": "none", "typ": "JWT"}),
		encode(claims),
		"signature",
	}, ".")
}
