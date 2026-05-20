package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestNormalizeOAuthProviderSupportsKiro(t *testing.T) {
	got, err := NormalizeOAuthProvider("kiro")
	if err != nil {
		t.Fatalf("NormalizeOAuthProvider() error = %v", err)
	}
	if got != "kiro" {
		t.Fatalf("NormalizeOAuthProvider() = %q, want kiro", got)
	}
}

func TestNormalizeOptionalAuthFileName(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := normalizeOptionalAuthFileName("")
		if err != nil {
			t.Fatalf("normalizeOptionalAuthFileName() error = %v", err)
		}
		if got != "" {
			t.Fatalf("normalizeOptionalAuthFileName() = %q, want empty", got)
		}
	})

	t.Run("appends json", func(t *testing.T) {
		got, err := normalizeOptionalAuthFileName("kiro-work")
		if err != nil {
			t.Fatalf("normalizeOptionalAuthFileName() error = %v", err)
		}
		if got != "kiro-work.json" {
			t.Fatalf("normalizeOptionalAuthFileName() = %q, want kiro-work.json", got)
		}
	})

	t.Run("rejects path", func(t *testing.T) {
		if _, err := normalizeOptionalAuthFileName("../kiro-work"); err == nil {
			t.Fatal("normalizeOptionalAuthFileName() error = nil, want error")
		}
	})
}

func TestRequestKiroTokenReturnsLoginURLAndRegistersState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/kiro-auth-url?idp=github", nil)

	handler.RequestKiroToken(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Status string `json:"status"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" || body.URL == "" || body.State == "" {
		t.Fatalf("response = %+v, want status/url/state", body)
	}
	defer CompleteOAuthSession(body.State)

	provider, status, ok := GetOAuthSession(body.State)
	if !ok {
		t.Fatalf("oauth session for state %q was not registered", body.State)
	}
	if provider != "kiro" || status != "" {
		t.Fatalf("session provider/status = %q/%q, want kiro/pending", provider, status)
	}

	parsed, err := url.Parse(body.URL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "prod.us-east-1.auth.desktop.kiro.dev" || parsed.Path != "/login" {
		t.Fatalf("auth url = %s, want Kiro login endpoint", parsed.String())
	}

	query := parsed.Query()
	if got := query.Get("idp"); got != "Github" {
		t.Fatalf("idp = %q, want Github", got)
	}
	if got := query.Get("redirect_uri"); got != kiroauth.OAuthRedirectURI(kiroauth.DefaultOAuthCallbackPort) {
		t.Fatalf("redirect_uri = %q, want %q", got, kiroauth.OAuthRedirectURI(kiroauth.DefaultOAuthCallbackPort))
	}
	if got := query.Get("state"); got != body.State {
		t.Fatalf("state = %q, want %q", got, body.State)
	}
	if got := query.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if query.Get("code_challenge") == "" {
		t.Fatal("code_challenge is empty")
	}
	if got := query.Get("redirect_from"); got != "" {
		t.Fatalf("redirect_from = %q, want empty", got)
	}
}

func TestRequestKiroTokenForceReauthAddsPrompt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/kiro-auth-url?provider=google&force_reauth=true", nil)

	handler.RequestKiroToken(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	defer CompleteOAuthSession(body.State)

	parsed, err := url.Parse(body.URL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	query := parsed.Query()
	if parsed.Scheme != "https" || parsed.Host != "prod.us-east-1.auth.desktop.kiro.dev" || parsed.Path != "/login" {
		t.Fatalf("auth url = %s, want Kiro login endpoint", parsed.String())
	}
	if got := query.Get("idp"); got != "Google" {
		t.Fatalf("idp = %q, want Google", got)
	}
	if got := query.Get("prompt"); got != "select_account" {
		t.Fatalf("prompt = %q, want select_account", got)
	}
	if got := query.Get("max_age"); got != "" {
		t.Fatalf("max_age = %q, want empty", got)
	}
}

func TestRequestKiroTokenRejectsInvalidCustomFileName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/kiro-auth-url?auth_file_name=../bad", nil)

	handler.RequestKiroToken(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestPostOAuthCallbackAcceptsKiroDeepLink(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	state := "0123456789abcdef0123456789abcdef"
	RegisterOAuthSession(state, "kiro")
	defer CompleteOAuthSession(state)

	body := []byte(fmt.Sprintf(`{"provider":"kiro","redirect_url":"kiro://kiro.kiroAgent/authenticate-success?code=auth-code&state=%s"}`, state))
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.PostOAuthCallback(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(authDir, fmt.Sprintf(".oauth-kiro-%s.oauth", state)))
	if err != nil {
		t.Fatalf("read callback file: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode callback file: %v", err)
	}
	if payload["code"] != "auth-code" || payload["state"] != state || payload["error"] != "" {
		t.Fatalf("unexpected callback payload: %+v", payload)
	}
}

func TestPostOAuthCallbackAcceptsKiroLocalCallbackURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	state := "abcdef0123456789abcdef0123456789"
	RegisterOAuthSession(state, "kiro")
	defer CompleteOAuthSession(state)

	callbackURL := fmt.Sprintf("http://localhost:3128/oauth/callback?login_option=google&code=auth-code&state=%s", state)
	body := []byte(fmt.Sprintf(`{"provider":"kiro","redirect_url":%q}`, callbackURL))
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.PostOAuthCallback(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(authDir, fmt.Sprintf(".oauth-kiro-%s.oauth", state)))
	if err != nil {
		t.Fatalf("read callback file: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode callback file: %v", err)
	}
	if payload["code"] != "auth-code" || payload["state"] != state || payload["error"] != "" {
		t.Fatalf("unexpected callback payload: %+v", payload)
	}
}
