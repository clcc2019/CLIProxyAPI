package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestDownloadAuthFile_ReturnsFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	fileName := "download-user.json"
	expected := []byte(`{"type":"codex"}`)
	if err := os.WriteFile(filepath.Join(authDir, fileName), expected, 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/download?name="+url.QueryEscape(fileName), nil)
	h.DownloadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected download status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); string(got) != string(expected) {
		t.Fatalf("unexpected download content: %q", string(got))
	}
}

func TestPreviewAuthFile_RemovesRuntimeState(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	fileName := "preview-user.json"
	idToken := testJWT(t, map[string]any{
		"email": "claim@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "acct_from_claim",
			"chatgpt_plan_type":                 "pro",
			"chatgpt_subscription_active_until": "2026-06-01T00:00:00Z",
		},
	})
	raw := []byte(`{
  "type": "codex",
  "email": "user@example.com",
  "name": "Display Name",
  "id_token": "` + idToken + `",
  "id_token_synthetic": true,
  "access_token": "access-token",
  "refresh_token": "refresh-token",
  "session_token": "session-token",
  "last_refresh": "2026-05-09T06:54:01Z",
  "expired": "2026-08-06T14:29:36Z",
  "disabled": true,
  "prefix": "team-a",
  "proxy_url": "http://proxy.example",
  "headers": {"X-Test": "value"},
  "cliproxy_runtime_state": {
    "version": 1,
    "success": 3,
    "failed": 1
  }
}`)
	path := filepath.Join(authDir, fileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/preview?name="+url.QueryEscape(fileName), nil)
	h.PreviewAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected preview status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("preview returned invalid JSON: %v", err)
	}
	if _, ok := got["cliproxy_runtime_state"]; ok {
		t.Fatalf("preview should not include cliproxy_runtime_state: %s", rec.Body.String())
	}
	for _, key := range []string{"prefix", "proxy_url", "headers"} {
		if _, ok := got[key]; ok {
			t.Fatalf("preview should not include %s: %s", key, rec.Body.String())
		}
	}
	if got["type"] != "codex" || got["email"] != "user@example.com" {
		t.Fatalf("preview lost auth fields: %#v", got)
	}
	if got["chatgpt_account_id"] != "acct_from_claim" || got["plan_type"] != "pro" || got["chatgpt_plan_type"] != "pro" {
		t.Fatalf("preview did not derive Codex account fields: %#v", got)
	}
	if got["id_token"] != idToken || got["id_token_synthetic"] != true || got["access_token"] != "access-token" || got["refresh_token"] != "refresh-token" || got["session_token"] != "session-token" {
		t.Fatalf("preview did not keep CPA token fields: %#v", got)
	}
	if got["subscription_expires_at"] != "2026-06-01T00:00:00Z" || got["disabled"] != true {
		t.Fatalf("preview did not keep CPA status fields: %#v", got)
	}
	if got["chatgpt_subscription_active_start"] != "2026-05-01T00:00:00Z" {
		t.Fatalf("preview did not derive Codex subscription active start: %#v", got)
	}

	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read auth file: %v", err)
	}
	if !strings.Contains(string(disk), "cliproxy_runtime_state") {
		t.Fatal("preview should not mutate the stored auth file")
	}
}

func TestDownloadAuthFile_RejectsPathSeparators(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)

	for _, name := range []string{
		"../external/secret.json",
		`..\\external\\secret.json`,
		"nested/secret.json",
		`nested\\secret.json`,
	} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/download?name="+url.QueryEscape(name), nil)
		h.DownloadAuthFile(ctx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected %d for name %q, got %d with body %s", http.StatusBadRequest, name, rec.Code, rec.Body.String())
		}
	}
}
