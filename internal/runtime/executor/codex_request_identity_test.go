package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexIsAPIKeyAuthTreatsMirroredAccessTokenAsOAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "access-token",
		},
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "acct_123",
		},
	}

	if codexIsAPIKeyAuth(auth) {
		t.Fatal("mirrored OAuth access token should not be treated as API key auth")
	}
}

func TestCodexIsAPIKeyAuthTreatsOAuthIdentityWithStaleAPIKeyMirrorAsOAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "old-access-token",
		},
		Metadata: map[string]any{
			"access_token": "new-access-token",
			"account_id":   "acct_123",
		},
	}

	if codexIsAPIKeyAuth(auth) {
		t.Fatal("OAuth auth with account metadata should not be treated as API key auth")
	}
}

func TestCodexIsAPIKeyAuthHonorsMetadataOAuthKind(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "access-token",
		},
		Metadata: map[string]any{
			"auth_kind":    " OAuth ",
			"access_token": "access-token",
		},
	}

	if codexIsAPIKeyAuth(auth) {
		t.Fatal("metadata auth_kind=oauth should not be treated as API key auth")
	}
}

func TestCodexIsAPIKeyAuthHonorsExplicitAPIKeyKind(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind": " APIKey ",
			"api_key":   "sk-test",
		},
		Metadata: map[string]any{
			"access_token": "sk-test",
		},
	}

	if !codexIsAPIKeyAuth(auth) {
		t.Fatal("explicit API key auth kind should be treated as API key auth")
	}
}

func BenchmarkCodexAuthKind(b *testing.B) {
	for b.Loop() {
		if got := codexAuthKind(" ChatGPT_Auth_Tokens "); got != "chatgpt_auth_tokens" {
			b.Fatalf("codexAuthKind() = %q", got)
		}
	}
}

func TestCodexCredsPrefersOAuthAccessTokenOverStaleAPIKeyMirror(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "old-access-token",
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
		Metadata: map[string]any{
			"access_token": "new-access-token",
			"account_id":   "acct_123",
		},
	}

	apiKey, baseURL := codexCreds(auth)
	if apiKey != "new-access-token" {
		t.Fatalf("apiKey = %q, want new-access-token", apiKey)
	}
	if baseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("baseURL = %q, want configured base URL", baseURL)
	}
}

func TestCodexCredsKeepsCustomAPIKeyWhenAccessTokenMetadataIsUnidentified(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "custom-api-key",
		},
		Metadata: map[string]any{
			"access_token": "old-access-token",
		},
	}

	apiKey, _ := codexCreds(auth)
	if apiKey != "custom-api-key" {
		t.Fatalf("apiKey = %q, want custom-api-key", apiKey)
	}
}
