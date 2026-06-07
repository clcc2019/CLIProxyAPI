package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestMiddlewareAcceptsMixedCaseBearerAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{
		cfg:                 &config.Config{},
		failedAttempts:      make(map[string]*attemptInfo),
		allowRemoteOverride: true,
		envSecret:           "test-secret",
	}
	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/management/ping", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/management/ping", nil)
	req.Header.Set("Authorization", "bEaReR test-secret")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
}

func BenchmarkMiddlewareMixedCaseBearerAuthorization(b *testing.B) {
	gin.SetMode(gin.TestMode)
	h := &Handler{
		cfg:                 &config.Config{},
		failedAttempts:      make(map[string]*attemptInfo),
		allowRemoteOverride: true,
		envSecret:           "test-secret",
	}
	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/management/ping", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/management/ping", nil)
	req.Header.Set("Authorization", "bEaReR test-secret")
	for b.Loop() {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			b.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
		}
	}
}

func TestAuthenticateManagementKey_LocalhostIPBan_BlocksCorrectKeyDuringBan(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}

	for i := 0; i < 5; i++ {
		allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret")
		if allowed {
			t.Fatalf("expected auth to be denied at attempt %d", i+1)
		}
		if statusCode != http.StatusUnauthorized || errMsg != "invalid management key" {
			t.Fatalf("unexpected auth failure at attempt %d: status=%d msg=%q", i+1, statusCode, errMsg)
		}
	}

	allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret")
	if allowed {
		t.Fatalf("expected correct key to be denied while banned")
	}
	if statusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden status while banned, got %d", statusCode)
	}
	if !strings.HasPrefix(errMsg, "IP banned due to too many failed attempts. Try again in") {
		t.Fatalf("unexpected banned message: %q", errMsg)
	}
}
