package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractAccessToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata map[string]any
		expected string
	}{
		{
			"antigravity top-level access_token",
			map[string]any{"access_token": "tok-abc"},
			"tok-abc",
		},
		{
			"gemini nested token.access_token",
			map[string]any{
				"token": map[string]any{"access_token": "tok-nested"},
			},
			"tok-nested",
		},
		{
			"top-level takes precedence over nested",
			map[string]any{
				"access_token": "tok-top",
				"token":        map[string]any{"access_token": "tok-nested"},
			},
			"tok-top",
		},
		{
			"empty metadata",
			map[string]any{},
			"",
		},
		{
			"whitespace-only access_token",
			map[string]any{"access_token": "   "},
			"",
		},
		{
			"wrong type access_token",
			map[string]any{"access_token": 12345},
			"",
		},
		{
			"token is not a map",
			map[string]any{"token": "not-a-map"},
			"",
		},
		{
			"nested whitespace-only",
			map[string]any{
				"token": map[string]any{"access_token": "  "},
			},
			"",
		},
		{
			"fallback to nested when top-level empty",
			map[string]any{
				"access_token": "",
				"token":        map[string]any{"access_token": "tok-fallback"},
			},
			"tok-fallback",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractAccessToken(tt.metadata)
			if got != tt.expected {
				t.Errorf("extractAccessToken() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFileTokenStoreListNormalizesKiroCLIToken(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{
		"provider": "google",
		"accessToken": "access-token",
		"refreshToken": "refresh-token",
		"profileArn": "arn:aws:codewhisperer:us-east-1:123:profile/social",
		"expiresAt": "2026-05-09T06:54:01Z",
		"clientId": "client-id",
		"clientSecret": "client-secret",
		"email": "user@example.com"
	}`)
	if err := os.WriteFile(filepath.Join(dir, "kiro-user.json"), raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(nil)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auths = %d, want 1", len(auths))
	}
	auth := auths[0]
	if got := auth.Provider; got != "kiro" {
		t.Fatalf("Provider = %q, want kiro", got)
	}
	if got := auth.Metadata["type"]; got != "kiro" {
		t.Fatalf("metadata type = %#v, want kiro", got)
	}
	if got := auth.Metadata["auth_method"]; got != "kiro-cli-social" {
		t.Fatalf("auth_method = %#v, want kiro-cli-social", got)
	}
	if got := auth.Metadata["profile_arn"]; got != "arn:aws:codewhisperer:us-east-1:123:profile/social" {
		t.Fatalf("profile_arn = %#v", got)
	}
}
