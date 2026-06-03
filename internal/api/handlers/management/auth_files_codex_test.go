package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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

func TestCodexUsageDefaultUserAgentUsesCurrentCodexFingerprint(t *testing.T) {
	if got := codexUsageUserAgent; got != misc.CodexCLIUserAgent {
		t.Fatalf("codexUsageUserAgent = %q, want %q", got, misc.CodexCLIUserAgent)
	}
	if !strings.Contains(codexUsageUserAgent, "/"+misc.CodexCLIVersion+" ") {
		t.Fatalf("codexUsageUserAgent = %q, missing current Codex version %q", codexUsageUserAgent, misc.CodexCLIVersion)
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

func TestCodexSubscriptionUntilValueReadsNestedEntitlement(t *testing.T) {
	got, ok := codexSubscriptionUntilValue(map[string]any{
		"plan_type":  "plus",
		"expires_at": "2026-05-09T06:54:01Z",
		"entitlement": map[string]any{
			"subscription_plan": "plus",
			"expiresAt":         "2026-07-01T00:00:00Z",
		},
	})
	if !ok {
		t.Fatal("codexSubscriptionUntilValue() ok = false, want true")
	}
	if got != "2026-07-01T00:00:00Z" {
		t.Fatalf("codexSubscriptionUntilValue() = %#v", got)
	}
}

func TestCodexSubscriptionUntilValueDoesNotUseTokenExpiry(t *testing.T) {
	_, ok := codexSubscriptionUntilValue(map[string]any{
		"plan_type":  "plus",
		"expires_at": "2026-05-09T06:54:01Z",
	})
	if ok {
		t.Fatal("top-level expires_at is token expiry and should not be used as subscription_expires_at")
	}
}

func TestListAuthFiles_CodexSubscriptionDefaultUsesCache(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	clearCodexSubscriptionCacheForTest(t)

	manager := coreauth.NewManager(nil, nil, nil)
	accessToken := "cached-access-token"
	auth := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": accessToken,
		},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	cacheKey := codexSubscriptionCacheKey(accessToken, "")
	codexSubscriptionCache.Store(cacheKey, codexSubscriptionCacheEntry{
		found:     true,
		expiresAt: time.Now().Add(time.Hour),
		info: codexAccountSubscriptionInfo{
			PlanType:              "pro",
			Email:                 "cached@example.com",
			SubscriptionExpiresAt: "2026-06-01T00:00:00Z",
		},
	})
	t.Cleanup(func() { codexSubscriptionCache.Delete(cacheKey) })

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	file := firstAuthFilePayload(t, rec.Body.Bytes())
	if got := file["subscription_expires_at"]; got != "2026-06-01T00:00:00Z" {
		t.Fatalf("subscription_expires_at = %#v", got)
	}
	if got := file["email"]; got != "cached@example.com" {
		t.Fatalf("email = %#v, want cached@example.com", got)
	}
	claims, ok := file["id_token"].(map[string]any)
	if !ok {
		t.Fatalf("id_token = %#v, want object", file["id_token"])
	}
	if got := claims["plan_type"]; got != "pro" {
		t.Fatalf("id_token.plan_type = %#v, want pro", got)
	}
}

func TestListAuthFiles_CodexSubscriptionSkipIgnoresCache(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	clearCodexSubscriptionCacheForTest(t)

	manager := coreauth.NewManager(nil, nil, nil)
	accessToken := "skip-cache-token"
	auth := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": accessToken,
		},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	cacheKey := codexSubscriptionCacheKey(accessToken, "")
	codexSubscriptionCache.Store(cacheKey, codexSubscriptionCacheEntry{
		found:     true,
		expiresAt: time.Now().Add(time.Hour),
		info: codexAccountSubscriptionInfo{
			SubscriptionExpiresAt: "2026-06-01T00:00:00Z",
		},
	})
	t.Cleanup(func() { codexSubscriptionCache.Delete(cacheKey) })

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?codex_subscription=skip", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	file := firstAuthFilePayload(t, rec.Body.Bytes())
	if _, ok := file["subscription_expires_at"]; ok {
		t.Fatalf("subscription_expires_at should be absent in skip mode, got %#v", file["subscription_expires_at"])
	}
}

func TestListAuthFiles_CodexSubscriptionRefreshBypassesNegativeCacheAndPersists(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	clearCodexSubscriptionCacheForTest(t)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("User-Agent"); got != codexAccountsCheckUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, codexAccountsCheckUserAgent)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer refresh-access-token" {
			http.Error(w, "bad authorization", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"accounts": {
				"org_plus": {
					"account": {"plan_type": "plus", "email": "plus@example.com", "is_default": true},
					"entitlement": {"subscription_plan": "plus", "currentPeriodStart": "2026-08-01T00:00:00Z", "currentPeriodEnd": "2026-09-01T00:00:00Z"}
				}
			}
		}`))
	}))
	t.Cleanup(server.Close)
	originalURL := codexAccountsCheckURL
	codexAccountsCheckURL = server.URL
	t.Cleanup(func() { codexAccountsCheckURL = originalURL })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(authPath, []byte(`{"type":"codex","access_token":"refresh-access-token","plan_type":"free"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	auth := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "refresh-access-token",
			"plan_type":    "free",
		},
		Attributes: map[string]string{"path": authPath},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	cacheKey := codexSubscriptionCacheKey("refresh-access-token", "")
	codexSubscriptionCache.Store(cacheKey, codexSubscriptionCacheEntry{found: false, expiresAt: time.Now().Add(time.Hour)})
	t.Cleanup(func() { codexSubscriptionCache.Delete(cacheKey) })

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?codex_subscription=refresh", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("accounts check calls = %d, want 1", got)
	}
	file := firstAuthFilePayload(t, rec.Body.Bytes())
	if got := file["subscription_expires_at"]; got != "2026-09-01T00:00:00Z" {
		t.Fatalf("subscription_expires_at = %#v", got)
	}
	if got := file["chatgpt_subscription_active_start"]; got != "2026-08-01T00:00:00Z" {
		t.Fatalf("chatgpt_subscription_active_start = %#v", got)
	}
	current, ok := manager.GetByID("codex.json")
	if !ok {
		t.Fatal("codex auth missing from manager")
	}
	if got := current.Metadata["subscription_expires_at"]; got != "2026-09-01T00:00:00Z" {
		t.Fatalf("manager subscription_expires_at = %#v", got)
	}
	store.mu.Lock()
	stored := store.items["codex.json"]
	store.mu.Unlock()
	if stored == nil {
		t.Fatal("codex auth was not persisted to store")
	}
	if got := stored.Metadata["subscription_expires_at"]; got != "2026-09-01T00:00:00Z" {
		t.Fatalf("stored subscription_expires_at = %#v", got)
	}
	if got := stored.Metadata["chatgpt_subscription_active_start"]; got != "2026-08-01T00:00:00Z" {
		t.Fatalf("stored chatgpt_subscription_active_start = %#v", got)
	}
	if got := stored.Metadata["plan_type"]; got != "plus" {
		t.Fatalf("stored plan_type = %#v", got)
	}
	if got := stored.Metadata["chatgpt_plan_type"]; got != "plus" {
		t.Fatalf("stored chatgpt_plan_type = %#v", got)
	}
	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("decode saved auth file: %v", err)
	}
	if got := saved["subscription_expires_at"]; got != "2026-09-01T00:00:00Z" {
		t.Fatalf("saved subscription_expires_at = %#v", got)
	}
	if got := saved["chatgpt_subscription_active_until"]; got != "2026-09-01T00:00:00Z" {
		t.Fatalf("saved chatgpt_subscription_active_until = %#v", got)
	}
	if got := saved["chatgpt_subscription_active_start"]; got != "2026-08-01T00:00:00Z" {
		t.Fatalf("saved chatgpt_subscription_active_start = %#v", got)
	}
	if got := saved["subscription_active_start"]; got != "2026-08-01T00:00:00Z" {
		t.Fatalf("saved subscription_active_start = %#v", got)
	}
	if got := saved["plan_type"]; got != "plus" {
		t.Fatalf("saved plan_type = %#v", got)
	}
	if got := saved["chatgpt_plan_type"]; got != "plus" {
		t.Fatalf("saved chatgpt_plan_type = %#v", got)
	}
}

func TestListAuthFiles_CodexSubscriptionRefreshCompletesExistingExpiryWithActiveStart(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	clearCodexSubscriptionCacheForTest(t)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer existing-expiry-token" {
			http.Error(w, "bad authorization", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"accounts": {
					"org_plus": {
						"account": {"is_default": true},
						"entitlement": {"current_period_start": "2026-08-01T00:00:00Z"}
					}
			}
		}`))
	}))
	t.Cleanup(server.Close)
	originalURL := codexAccountsCheckURL
	codexAccountsCheckURL = server.URL
	t.Cleanup(func() { codexAccountsCheckURL = originalURL })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(authPath, []byte(`{"type":"codex","access_token":"existing-expiry-token","subscription_expires_at":"2026-09-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":                    "codex",
			"access_token":            "existing-expiry-token",
			"subscription_expires_at": "2026-09-01T00:00:00Z",
		},
		Attributes: map[string]string{"path": authPath},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?codex_subscription=refresh", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("accounts check calls = %d, want 1", got)
	}
	file := firstAuthFilePayload(t, rec.Body.Bytes())
	if got := file["chatgpt_subscription_active_start"]; got != "2026-08-01T00:00:00Z" {
		t.Fatalf("chatgpt_subscription_active_start = %#v", got)
	}
	current, ok := manager.GetByID("codex.json")
	if !ok {
		t.Fatal("codex auth missing from manager")
	}
	if got := current.Metadata["chatgpt_subscription_active_start"]; got != "2026-08-01T00:00:00Z" {
		t.Fatalf("manager chatgpt_subscription_active_start = %#v", got)
	}
	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("decode saved auth file: %v", err)
	}
	if got := saved["chatgpt_subscription_active_start"]; got != "2026-08-01T00:00:00Z" {
		t.Fatalf("saved chatgpt_subscription_active_start = %#v", got)
	}
}

func TestListAuthFiles_CodexSubscriptionRefreshBackfillsNestedDiskMetadata(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	clearCodexSubscriptionCacheForTest(t)

	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(path, []byte(`{
		"type": "codex",
		"plan_type": "plus",
		"entitlement": {"expiresAt": "2026-07-01T00:00:00Z"}
	}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?codex_subscription=refresh", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	file := firstAuthFilePayload(t, rec.Body.Bytes())
	if got := file["subscription_expires_at"]; got != "2026-07-01T00:00:00Z" {
		t.Fatalf("subscription_expires_at = %#v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("decode saved auth file: %v", err)
	}
	if got := saved["subscription_expires_at"]; got != "2026-07-01T00:00:00Z" {
		t.Fatalf("saved subscription_expires_at = %#v", got)
	}
}

func TestListAuthFiles_CodexSubscriptionPaginatedRefreshOnlyCurrentManagerPage(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	clearCodexSubscriptionCacheForTest(t)

	var mu sync.Mutex
	tokens := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		tokens = append(tokens, token)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"accounts": {
				"org_plus": {
					"account": {"plan_type": "plus", "email": "plus@example.com", "is_default": true},
					"entitlement": {"subscription_plan": "plus", "expires_at": "2026-10-01T00:00:00Z"}
				}
			}
		}`))
	}))
	t.Cleanup(server.Close)
	originalURL := codexAccountsCheckURL
	codexAccountsCheckURL = server.URL
	t.Cleanup(func() { codexAccountsCheckURL = originalURL })

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, item := range []struct {
		name  string
		token string
	}{
		{name: "a.json", token: "token-a"},
		{name: "b.json", token: "token-b"},
		{name: "c.json", token: "token-c"},
	} {
		path := filepath.Join(authDir, item.name)
		if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("write auth file: %v", err)
		}
		_, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:       item.name,
			FileName: item.name,
			Provider: "codex",
			Metadata: map[string]any{
				"type":         "codex",
				"access_token": item.token,
			},
			Attributes: map[string]string{"path": path},
		})
		if err != nil {
			t.Fatalf("register %s: %v", item.name, err)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?page=2&page_size=1&sort=az&codex_subscription=refresh", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	file := firstAuthFilePayload(t, rec.Body.Bytes())
	if got := file["name"]; got != "b.json" {
		t.Fatalf("name = %#v, want b.json", got)
	}
	if got := file["subscription_expires_at"]; got != "2026-10-01T00:00:00Z" {
		t.Fatalf("subscription_expires_at = %#v", got)
	}
	if got := file["chatgpt_subscription_active_start"]; got != "2026-09-01T00:00:00Z" {
		t.Fatalf("chatgpt_subscription_active_start = %#v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 1 || tokens[0] != "token-b" {
		t.Fatalf("accounts check tokens = %#v, want only token-b", tokens)
	}
}

func TestListAuthFiles_PaginatedRequestClampsPastLastPage(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, name := range []string{"a.json", "b.json"} {
		path := filepath.Join(authDir, name)
		if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("write auth file: %v", err)
		}
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:         name,
			FileName:   name,
			Provider:   "codex",
			Metadata:   map[string]any{"type": "codex"},
			Attributes: map[string]string{"path": path},
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?page=9&page_size=1&sort=az&codex_subscription=skip", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
		Page  int              `json:"page"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Page != 2 || payload.Total != 2 || len(payload.Files) != 1 {
		t.Fatalf("payload = %#v, want page 2 total 2 and one file", payload)
	}
	if got := payload.Files[0]["name"]; got != "b.json" {
		t.Fatalf("page file = %#v, want b.json", got)
	}
}

func TestGetCodexUsageUsesOfficialHeadersAndLocalSubscription(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if got := r.Header.Get("Authorization"); got != "Bearer usage-access-token" {
			t.Fatalf("Authorization = %q, want bearer access token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct_123" {
			t.Fatalf("ChatGPT-Account-ID = %q, want acct_123", got)
		}
		if got := r.Header.Get("User-Agent"); got != "codex-profile/1.0" {
			t.Fatalf("User-Agent = %q, want %q", got, "codex-profile/1.0")
		}
		for _, header := range []string{
			"Originator",
			"X-Codex-Beta-Features",
			"X-Codex-Installation-Id",
			"x-responsesapi-include-timing-metrics",
		} {
			if got := r.Header.Get(header); got != "" {
				t.Fatalf("%s = %q, want empty for official usage request", header, got)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type": "plus",
			"rate_limit": {
				"primary_window": {"used_percent": 12.5, "limit_window_seconds": 18000, "reset_at": 1780000000},
				"secondary_window": {"used_percent": 50, "limit_window_seconds": 604800, "reset_at": 1780500000}
			},
			"credits": {"has_credits": true, "unlimited": false, "balance": 10}
		}`))
	}))
	t.Cleanup(server.Close)
	originalURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalURL })

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":                    "codex",
			"access_token":            "usage-access-token",
			"account_id":              "acct_123",
			"subscription_expires_at": "2026-06-01T00:00:00Z",
			"user_agent":              "codex-profile/1.0",
			"originator":              "codex_vscode",
			"beta_features":           "feature-a,feature-b",
			"installation_id":         "install-1",
			"include_timing_metrics":  true,
		},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/codex-usage?name=codex.json", nil)

	h.GetCodexUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if !sawRequest {
		t.Fatal("usage endpoint was not called")
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	if got := payload["subscription_expires_at"]; got != "2026-06-01T00:00:00Z" {
		t.Fatalf("subscription_expires_at = %#v", got)
	}
	if got := payload["chatgpt_subscription_active_start"]; got != "2026-05-01T00:00:00Z" {
		t.Fatalf("chatgpt_subscription_active_start = %#v", got)
	}
	if got := payload["chatgpt_account_id"]; got != "acct_123" {
		t.Fatalf("chatgpt_account_id = %#v", got)
	}
	if _, ok := payload["rate_limit"].(map[string]any); !ok {
		t.Fatalf("rate_limit missing from payload: %#v", payload)
	}
}

func TestGetCodexUsageDerivesAccountIDFromAccessTokenClaims(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	accessToken := testJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_from_access",
			"chatgpt_plan_type":  "plus",
		},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct_from_access" {
			t.Fatalf("ChatGPT-Account-ID = %q, want acct_from_access", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit": null}`))
	}))
	t.Cleanup(server.Close)
	originalURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalURL })

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": accessToken,
		},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/codex-usage?name=codex.json", nil)

	h.GetCodexUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	if got := payload["plan_type"]; got != "plus" {
		t.Fatalf("plan_type = %#v, want plus", got)
	}
}

func TestGetCodexUsageMarksAuthScopedQuotaCooldown(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         100,
					"limit_window_seconds": 18000,
					"reset_at":             resetAt,
				},
			},
		})
	}))
	t.Cleanup(server.Close)
	originalURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalURL })

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "usage-access-token",
			"account_id":   "acct_123",
		},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/codex-usage?name=codex.json", nil)

	h.GetCodexUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	authFile, ok := payload["auth_file"].(map[string]any)
	if !ok {
		t.Fatalf("auth_file missing from payload: %#v", payload)
	}
	if got := authFile["unavailable"]; got != true {
		t.Fatalf("auth_file.unavailable = %#v, want true", got)
	}
	quota, ok := authFile["quota"].(map[string]any)
	if !ok {
		t.Fatalf("auth_file.quota missing: %#v", authFile)
	}
	if got := quota["exceeded"]; got != true {
		t.Fatalf("auth_file.quota.exceeded = %#v, want true", got)
	}
	updated, ok := manager.GetByID("codex.json")
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if !updated.Quota.Exceeded || !updated.Quota.AuthScope {
		t.Fatalf("quota state = %#v, want auth-scoped exceeded quota", updated.Quota)
	}
	if !updated.Unavailable || !updated.NextRetryAfter.After(time.Now()) {
		t.Fatalf("availability = unavailable:%v next:%v, want future cooldown", updated.Unavailable, updated.NextRetryAfter)
	}
	if got := updated.NextRetryAfter.Unix(); got != resetAt {
		t.Fatalf("next retry unix = %d, want reset_at %d", got, resetAt)
	}
	if coreauth.AuthAvailableForModel(updated, "gpt-5-codex", time.Now()) {
		t.Fatal("auth should be unavailable for Codex model while usage quota is exhausted")
	}
}

func TestGetCodexUsageRetriesTransient502(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	withFastCodexUsageRetry(t)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "temporary gateway failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":{"balance":25},"rate_limit":null}`))
	}))
	t.Cleanup(server.Close)
	originalURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalURL })

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "usage-access-token",
			"account_id":   "acct_123",
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/codex-usage?name=codex.json", nil)

	h.GetCodexUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("usage calls = %d, want 2", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	credits, _ := payload["credits"].(map[string]any)
	if got := credits["balance"]; got != float64(25) {
		t.Fatalf("credits.balance = %#v, want 25", got)
	}
}

func TestGetCodexUsageUsesStaleCacheOnPersistent502(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	withFastCodexUsageRetry(t)

	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "temporary gateway failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":{"balance":99},"rate_limit":null}`))
	}))
	t.Cleanup(server.Close)
	originalURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalURL })

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "usage-access-token",
			"account_id":   "acct_123",
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	first := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(first)
	firstCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/codex-usage?name=codex.json", nil)
	h.GetCodexUsage(firstCtx)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d body=%s", first.Code, http.StatusOK, first.Body.String())
	}

	fail.Store(true)
	second := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(second)
	secondCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/codex-usage?name=codex.json&force=true", nil)
	h.GetCodexUsage(secondCtx)

	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d body=%s", second.Code, http.StatusOK, second.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode stale payload: %v", err)
	}
	if got := payload["codex_usage_stale"]; got != true {
		t.Fatalf("codex_usage_stale = %#v, want true", got)
	}
	credits, _ := payload["credits"].(map[string]any)
	if got := credits["balance"]; got != float64(99) {
		t.Fatalf("credits.balance = %#v, want 99", got)
	}
}

func TestGetCodexUsageReturnsUnavailablePayloadForPersistent502(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	withFastCodexUsageRetry(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary gateway failure", http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	originalURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalURL })

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "usage-access-token",
			"account_id":   "acct_123",
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/codex-usage?name=codex.json", nil)

	h.GetCodexUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode unavailable payload: %v", err)
	}
	if got := payload["codex_usage_unavailable"]; got != true {
		t.Fatalf("codex_usage_unavailable = %#v, want true", got)
	}
	if got := payload["codex_usage_upstream_status"]; got != float64(http.StatusBadGateway) {
		t.Fatalf("codex_usage_upstream_status = %#v, want %d", got, http.StatusBadGateway)
	}
}

func TestGetCodexUsageRefreshUsesGlobalProxyWhenAuthProxyIsNone(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	clearCodexSubscriptionCacheForTest(t)

	var mu sync.Mutex
	hits := make([]string, 0, 2)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.String())
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/accounts/check"):
			_, _ = w.Write([]byte(`{
				"accounts": {
					"acct_123": {
						"account": {"plan_type": "plus", "email": "plus@example.com", "is_default": true},
						"entitlement": {"subscription_plan": "plus", "current_period_start": "2026-05-12T23:56:10Z", "expires_at": "2026-06-12T23:56:10Z"}
					}
				}
			}`))
		case strings.Contains(r.URL.Path, "/wham/usage"):
			_, _ = w.Write([]byte(`{"rate_limit": null}`))
		default:
			http.Error(w, "unexpected proxy target", http.StatusBadGateway)
		}
	}))
	t.Cleanup(proxyServer.Close)

	originalAccountsURL := codexAccountsCheckURL
	originalUsageURL := codexUsageURL
	codexAccountsCheckURL = "http://127.0.0.1:1/backend-api/accounts/check/v4-2023-04-27"
	codexUsageURL = "http://127.0.0.1:1/backend-api/wham/usage"
	t.Cleanup(func() {
		codexAccountsCheckURL = originalAccountsURL
		codexUsageURL = originalUsageURL
	})

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		ProxyURL: "none",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "usage-access-token",
			"account_id":   "acct_123",
			"proxy_url":    "none",
		},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: proxyServer.URL},
		AuthDir:   t.TempDir(),
	}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/codex-usage?name=codex.json&codex_subscription=refresh", nil)

	h.GetCodexUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	if got := payload["subscription_expires_at"]; got != "2026-06-12T23:56:10Z" {
		t.Fatalf("subscription_expires_at = %#v", got)
	}
	if got := payload["chatgpt_subscription_active_start"]; got != "2026-05-12T23:56:10Z" {
		t.Fatalf("chatgpt_subscription_active_start = %#v", got)
	}
	authFile, ok := payload["auth_file"].(map[string]any)
	if !ok {
		t.Fatalf("auth_file missing from payload: %#v", payload)
	}
	if got := authFile["subscription_expires_at"]; got != "2026-06-12T23:56:10Z" {
		t.Fatalf("auth_file.subscription_expires_at = %#v", got)
	}
	if got := authFile["chatgpt_subscription_active_start"]; got != "2026-05-12T23:56:10Z" {
		t.Fatalf("auth_file.chatgpt_subscription_active_start = %#v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawAccounts, sawUsage bool
	for _, hit := range hits {
		if strings.Contains(hit, "/accounts/check") {
			sawAccounts = true
		}
		if strings.Contains(hit, "/wham/usage") {
			sawUsage = true
		}
	}
	if !sawAccounts || !sawUsage {
		t.Fatalf("proxy hits = %#v, want accounts/check and wham/usage", hits)
	}
}

func clearCodexSubscriptionCacheForTest(t *testing.T) {
	t.Helper()
	codexSubscriptionCache.Range(func(key, _ any) bool {
		codexSubscriptionCache.Delete(key)
		return true
	})
}

func withFastCodexUsageRetry(t *testing.T) {
	t.Helper()
	originalMax := codexUsageMaxRequestRetries
	originalDelay := codexUsageRetryBaseDelay
	codexUsageMaxRequestRetries = 1
	codexUsageRetryBaseDelay = time.Millisecond
	t.Cleanup(func() {
		codexUsageMaxRequestRetries = originalMax
		codexUsageRetryBaseDelay = originalDelay
	})
}

func firstAuthFilePayload(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode list payload: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(payload.Files))
	}
	return payload.Files[0]
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

func TestParseCodexAccountSubscriptionInfoReadsCurrentPeriodEnd(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{
		"accounts": {
			"org_plus": {
				"account": {"plan_type": "plus", "email": "plus@example.com", "is_default": true},
				"entitlement": {"subscription_plan": "plus", "currentPeriodEnd": "2026-09-01T00:00:00Z"}
			}
		}
	}`), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	got := parseCodexAccountSubscriptionInfo(payload, "org_plus")
	if got == nil {
		t.Fatal("parseCodexAccountSubscriptionInfo() = nil")
	}
	if got.SubscriptionExpiresAt != "2026-09-01T00:00:00Z" {
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
