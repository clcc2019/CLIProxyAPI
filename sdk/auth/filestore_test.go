package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type testTokenStorage struct {
	meta map[string]any
}

func (s *testTokenStorage) SetMetadata(meta map[string]any) { s.meta = meta }

func (s *testTokenStorage) SaveTokenToFile(authFilePath string) error {
	raw, err := json.Marshal(s.meta)
	if err != nil {
		return err
	}
	return os.WriteFile(authFilePath, raw, 0o600)
}

func TestFileTokenStoreSaveDisabledPersistsFlagForTokenStorage(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "disabled.json")

	if err := os.WriteFile(path, []byte(`{"type":"test","disabled":true}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	storage := &testTokenStorage{}
	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		Provider: "test",
		FileName: "disabled.json",
		Disabled: true,
		Storage:  storage,
		Metadata: map[string]any{"type": "test"},
	}

	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if disabled, _ := meta["disabled"].(bool); !disabled {
		t.Fatalf("disabled=%v, want true (raw=%s)", meta["disabled"], string(raw))
	}
}

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

func TestFileTokenStoreListAppliesKiroAuthFileOptions(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{
		"type": "kiro",
		"access_token": "access-token",
		"refresh_token": "refresh-token",
		"expires_at": "2026-05-09T06:54:01Z",
		"provider": "google",
		"email": "user@example.com",
		"priority": 7,
		"proxy_url": "http://127.0.0.1:7890",
		"prefix": "kiro-main"
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
	if got := auth.ProxyURL; got != "http://127.0.0.1:7890" {
		t.Fatalf("ProxyURL = %q, want per-auth proxy", got)
	}
	if got := auth.Prefix; got != "kiro-main" {
		t.Fatalf("Prefix = %q, want kiro-main", got)
	}
	if got := auth.Attributes["priority"]; got != "7" {
		t.Fatalf("Attributes[priority] = %q, want 7", got)
	}
}
