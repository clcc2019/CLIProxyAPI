package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestApplyCodexHeadersOmitsEmptyAuthorizationToken(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer stale")

	applyCodexHeaders(req, nil, "  ", true, nil)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
}

func TestApplyCodexHeadersAllowsCustomAuthorizationWithoutToken(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"header:Authorization": "Bearer custom",
	}}

	applyCodexHeaders(req, auth, "", true, nil)

	if got := req.Header.Get("Authorization"); got != "Bearer custom" {
		t.Fatalf("Authorization = %q, want custom bearer", got)
	}
}

func TestApplyCodexHeadersSetsFedrampForOAuthAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "oauth-token",
			"account_id":   "account-1",
			"fedramp":      true,
		},
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get(codexHeaderOpenAIFedramp); got != "true" {
		t.Fatalf("%s = %q, want true", codexHeaderOpenAIFedramp, got)
	}
}

func TestApplyCodexHeadersDoesNotSetFedrampForAPIKeyAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"fedramp": true,
		},
		Attributes: map[string]string{
			"api_key":   "sk-test",
			"auth_kind": "api_key",
		},
	}

	applyCodexHeaders(req, auth, "sk-test", true, nil)

	if got := req.Header.Get(codexHeaderOpenAIFedramp); got != "" {
		t.Fatalf("%s = %q, want empty for API key auth", codexHeaderOpenAIFedramp, got)
	}
}
