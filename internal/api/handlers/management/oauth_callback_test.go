package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestPostOAuthCallbackCreatesMissingAuthDir(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := filepath.Join(t.TempDir(), "missing-auth")
	state := "test-xai-state"
	RegisterOAuthSession(state, "xai")
	defer CompleteOAuthSession(state)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.POST("/v0/management/oauth-callback", h.PostOAuthCallback)

	body := `{"provider":"xai","redirect_url":"http://localhost:59788/oauth-callback?state=test-xai-state&code=test-code"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, w.Code, w.Body.String())
	}

	callbackPath := filepath.Join(authDir, ".oauth-xai-"+state+".oauth")
	data, errRead := os.ReadFile(callbackPath)
	if errRead != nil {
		t.Fatalf("expected callback file to be written: %v", errRead)
	}

	var payload oauthCallbackFilePayload
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode callback payload: %v", errUnmarshal)
	}
	if payload.State != state || payload.Code != "test-code" || payload.Error != "" {
		t.Fatalf("unexpected callback payload: %+v", payload)
	}
}

func TestWriteOAuthCallbackFileForPendingSessionCreatesMissingAuthDirForCallbackProviders(t *testing.T) {
	providers := []string{"anthropic", "codex", "xai"}
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			authDir := filepath.Join(t.TempDir(), "missing-auth")
			state := provider + "-state"
			RegisterOAuthSession(state, provider)
			defer CompleteOAuthSession(state)

			path, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, provider, state, "code-"+provider, "")
			if errWrite != nil {
				t.Fatalf("expected callback file write to succeed: %v", errWrite)
			}

			data, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatalf("expected callback file to be written: %v", errRead)
			}

			var payload oauthCallbackFilePayload
			if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
				t.Fatalf("failed to decode callback payload: %v", errUnmarshal)
			}
			if payload.State != state || payload.Code != "code-"+provider || payload.Error != "" {
				t.Fatalf("unexpected callback payload: %+v", payload)
			}
		})
	}
}

func TestNormalizeOAuthProviderAliases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: " Anthropic ", want: "anthropic"},
		{input: "CLAUDE", want: "anthropic"},
		{input: " Codex ", want: "codex"},
		{input: "OPENAI", want: "codex"},
		{input: " XAI ", want: "xai"},
		{input: "x-AI", want: "xai"},
		{input: "GROK", want: "xai"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := NormalizeOAuthProvider(tt.input)
			if err != nil {
				t.Fatalf("NormalizeOAuthProvider(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeOAuthProvider(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeOAuthProviderRejectsUnsupported(t *testing.T) {
	if got, err := NormalizeOAuthProvider(" unknown "); err == nil || got != "" {
		t.Fatalf("NormalizeOAuthProvider(unsupported) = %q, %v; want unsupported error", got, err)
	}
}

func BenchmarkNormalizeOAuthProvider(b *testing.B) {
	for b.Loop() {
		got, err := NormalizeOAuthProvider(" Anthropic ")
		if err != nil {
			b.Fatal(err)
		}
		if got != "anthropic" {
			b.Fatalf("got %q", got)
		}
	}
}
