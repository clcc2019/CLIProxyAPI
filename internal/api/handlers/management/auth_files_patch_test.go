package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFilesFromDiskExposesRuntimeStateError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	runtimeUpdatedAt := time.Date(2026, 5, 13, 9, 30, 0, 0, time.UTC)
	runtimeSavedAt := runtimeUpdatedAt.Add(2 * time.Second)
	raw := map[string]any{
		"type":  "oauth",
		"email": "oauth@example.com",
		"cliproxy_runtime_state": map[string]any{
			"version":        1,
			"status":         "error",
			"status_message": "refresh token invalid - re-login required",
			"unavailable":    true,
			"updated_at":     runtimeUpdatedAt.Format(time.RFC3339),
			"saved_at":       runtimeSavedAt.Format(time.RFC3339),
			"last_error": map[string]any{
				"code":        "refresh_failed",
				"message":     "oauth refresh rejected",
				"retryable":   false,
				"http_status": http.StatusUnauthorized,
			},
		},
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oauth-google.json"), data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	resp := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(resp)
	h.ListAuthFiles(c)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	var body struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Files) != 1 {
		t.Fatalf("files = %#v, want one file", body.Files)
	}
	file := body.Files[0]
	if got := file["status_message"]; got != "refresh token invalid - re-login required" {
		t.Fatalf("status_message = %#v", got)
	}
	lastError, ok := file["last_error"].(map[string]any)
	if !ok {
		t.Fatalf("last_error = %#v, want object", file["last_error"])
	}
	if got := lastError["message"]; got != "oauth refresh rejected" {
		t.Fatalf("last_error.message = %#v", got)
	}
	if got := int(lastError["http_status"].(float64)); got != http.StatusUnauthorized {
		t.Fatalf("last_error.http_status = %d", got)
	}
}

func TestListAuthFiles_PaginatedManagerResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)

	for _, item := range []struct {
		id       string
		name     string
		provider string
	}{
		{id: "alpha.json", name: "alpha.json", provider: "codex"},
		{id: "alpha-copy", name: "alpha.json", provider: "codex"},
		{id: "bravo.json", name: "bravo.json", provider: "codex"},
		{id: "charlie.json", name: "charlie.json", provider: "oauth"},
	} {
		path := filepath.Join(authDir, item.name)
		if err := os.WriteFile(path, []byte(`{"type":"`+item.provider+`"}`), 0o600); err != nil {
			t.Fatalf("write auth file %s: %v", item.name, err)
		}
		_, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:       item.id,
			FileName: item.name,
			Provider: item.provider,
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				"path": path,
			},
			Metadata: map[string]any{
				"type": item.provider,
			},
		})
		if err != nil {
			t.Fatalf("register auth file %s: %v", item.name, err)
		}
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-config-runtime",
		FileName: "claude-config-runtime",
		Provider: "claude",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register pathless claude auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?page=2&page_size=1&type=codex&sort=az", nil)

	h.ListAuthFiles(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Files      []map[string]any `json:"files"`
		Total      int              `json:"total"`
		Page       int              `json:"page"`
		PageSize   int              `json:"page_size"`
		TypeCounts map[string]int   `json:"type_counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Total != 2 || body.Page != 2 || body.PageSize != 1 {
		t.Fatalf("pagination = total:%d page:%d size:%d", body.Total, body.Page, body.PageSize)
	}
	if len(body.Files) != 1 || body.Files[0]["name"] != "bravo.json" {
		t.Fatalf("files = %#v, want bravo.json", body.Files)
	}
	if body.TypeCounts["all"] != 3 || body.TypeCounts["codex"] != 2 || body.TypeCounts["oauth"] != 1 {
		t.Fatalf("type_counts = %#v", body.TypeCounts)
	}
	if body.TypeCounts["claude"] != 0 {
		t.Fatalf("type_counts should not include pathless claude auth: %#v", body.TypeCounts)
	}
}

func TestListAuthFiles_PaginatedManagerSubscriptionExpirySortIsGlobal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)

	items := []struct {
		name      string
		planType  string
		expiresAt string
	}{
		{name: "alpha-no-expiry.json", planType: "plus"},
		{name: "bravo-free.json", planType: "free"},
		{name: "charlie-soon.json", planType: "plus", expiresAt: "2026-06-01T00:00:00Z"},
		{name: "delta-late.json", planType: "plus", expiresAt: "2026-08-01T00:00:00Z"},
	}
	for _, item := range items {
		path := filepath.Join(authDir, item.name)
		if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("write auth file %s: %v", item.name, err)
		}
		metadata := map[string]any{
			"type":      "codex",
			"plan_type": item.planType,
		}
		if item.expiresAt != "" {
			metadata["subscription_expires_at"] = item.expiresAt
		}
		_, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:         item.name,
			FileName:   item.name,
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			Metadata:   metadata,
			Attributes: map[string]string{"path": path},
		})
		if err != nil {
			t.Fatalf("register auth file %s: %v", item.name, err)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?page=1&page_size=2&sort=subscription_expiry&codex_subscription=skip", nil)

	h.ListAuthFiles(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Files []map[string]any `json:"files"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Total != 4 || len(body.Files) != 2 {
		t.Fatalf("payload = %#v, want total 4 and two files", body)
	}
	want := []string{"charlie-soon.json", "delta-late.json"}
	for i, name := range want {
		if got := body.Files[i]["name"]; got != name {
			t.Fatalf("files[%d].name = %#v, want %s; files=%#v", i, got, name, body.Files)
		}
	}
}

func TestSortAuthFileEntriesForListUsesPrecomputedKeys(t *testing.T) {
	files := []gin.H{
		{"name": "Zulu.json", "type": "Codex", "priority": 5},
		{"name": "bravo.json", "type": "Claude", "priority": 10},
		{"name": "Alpha.json", "type": "Claude", "priority": 10},
		{"name": "aardvark.json", "type": "Codex", "priority": 100, "disabled": true},
	}

	sortAuthFileEntriesForList(files, "priority")

	want := []string{"Alpha.json", "bravo.json", "Zulu.json", "aardvark.json"}
	for i, name := range want {
		if got := files[i]["name"]; got != name {
			t.Fatalf("priority files[%d].name = %#v, want %q; files=%#v", i, got, name, files)
		}
	}

	sortAuthFileEntriesForList(files, "default")
	want = []string{"Alpha.json", "bravo.json", "Zulu.json", "aardvark.json"}
	for i, name := range want {
		if got := files[i]["name"]; got != name {
			t.Fatalf("default files[%d].name = %#v, want %q; files=%#v", i, got, name, files)
		}
	}
}

func TestAuthFilesListQueryNormalizesSearchOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?q=%20AlPhA*JsOn%20", nil)

	q := authFilesListQueryFromRequest(c)

	if q.Search != "alpha*json" {
		t.Fatalf("search = %q, want alpha*json", q.Search)
	}
	if len(q.SearchParts) != 2 || q.SearchParts[0] != "alpha" || q.SearchParts[1] != "json" {
		t.Fatalf("search parts = %#v, want [alpha json]", q.SearchParts)
	}
	if !authFileMatchesNormalizedSearch(q.Search, q.SearchParts, "alpha-file.json", "codex") {
		t.Fatal("normalized wildcard search should match")
	}
}

func BenchmarkAuthFileMatchesNormalizedWildcardSearch(b *testing.B) {
	search := "alpha*json"
	parts := strings.Split(search, "*")
	b.ReportAllocs()
	for b.Loop() {
		if !authFileMatchesNormalizedSearch(search, parts, "alpha-file.json", "codex") {
			b.Fatal("normalized wildcard search should match")
		}
	}
}

func BenchmarkSortAuthFileEntriesForListSubscription(b *testing.B) {
	seed := make([]gin.H, 256)
	for i := range seed {
		seed[i] = gin.H{
			"name":                    "Auth-" + strconv.Itoa(255-i) + ".json",
			"type":                    []string{"Codex", "Claude", "OpenAI"}[i%3],
			"priority":                i % 11,
			"plan_type":               []string{"Plus", "Free", "Team"}[i%3],
			"subscription_expires_at": int64(1_800_000_000_000 + i*60_000),
			"disabled":                i%29 == 0,
		}
	}
	files := make([]gin.H, len(seed))

	b.ReportAllocs()
	for b.Loop() {
		copy(files, seed)
		sortAuthFileEntriesForList(files, "subscription_expiry")
	}
}

func TestPatchAuthFileFieldsUpdatesUserAgent(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth.json",
		FileName: "codex-auth.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":      "codex@example.com",
			"user_agent": "old-ua",
			"user-agent": "legacy-old-ua",
		},
		Attributes: map[string]string{
			"path":              "/tmp/codex-auth.json",
			"header:User-Agent": "old-ua",
			"user_agent":        "legacy-old-ua",
			"user-agent":        "legacy-old-ua",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	body, err := json.Marshal(map[string]any{
		"name":       "codex-auth.json",
		"user_agent": "new-ua",
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex-auth.json")
	if !ok || updated == nil {
		t.Fatal("expected updated auth to exist")
	}
	if got, _ := updated.Metadata["user_agent"].(string); got != "new-ua" {
		t.Fatalf("Metadata[user_agent] = %q, want %q", got, "new-ua")
	}
	if _, ok := updated.Metadata["user-agent"]; ok {
		t.Fatal("Metadata[user-agent] should be removed")
	}
	if got := updated.Attributes["header:User-Agent"]; got != "new-ua" {
		t.Fatalf("Attributes[header:User-Agent] = %q, want %q", got, "new-ua")
	}
	if _, ok := updated.Attributes["user_agent"]; ok {
		t.Fatal("Attributes[user_agent] should be removed")
	}
	if _, ok := updated.Attributes["user-agent"]; ok {
		t.Fatal("Attributes[user-agent] should be removed")
	}
}

func TestBuildAuthFileEntryExposesUserAgent(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth.json",
		FileName: "codex-auth.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":      "codex@example.com",
			"user_agent": "codex-cli-test/1.0",
		},
		Attributes: map[string]string{
			"path": "/tmp/codex-auth.json",
		},
	}

	entry := h.buildAuthFileEntry(auth)
	if got, _ := entry["user_agent"].(string); got != "codex-cli-test/1.0" {
		t.Fatalf("entry[user_agent] = %q, want %q", got, "codex-cli-test/1.0")
	}
}

func TestBuildAuthFileEntryExposesCodexClientProfile(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth.json",
		FileName: "codex-auth.json",
		Provider: "codex",
		Metadata: map[string]any{
			"codex_client_profile_pinned": true,
			"email":                       "codex@example.com",
			"originator":                  "codex_vscode",
			"user_agent":                  "codex-cli-test/1.0",
			"headers": map[string]any{
				"X-Codex-Installation-Id": "install-1",
				"X-Codex-Beta-Features":   "feature-1",
				"empty":                   "",
			},
		},
		Attributes: map[string]string{
			"path": "/tmp/codex-auth.json",
		},
	}

	entry := h.buildAuthFileEntry(auth)
	profile, ok := entry["client_profile"].(gin.H)
	if !ok {
		t.Fatalf("entry[client_profile] = %T, want gin.H", entry["client_profile"])
	}
	if got, _ := profile["pinned"].(bool); !got {
		t.Fatalf("client_profile[pinned] = %#v, want true", profile["pinned"])
	}
	if got, _ := profile["user_agent"].(string); got != "codex-cli-test/1.0" {
		t.Fatalf("client_profile[user_agent] = %q, want %q", got, "codex-cli-test/1.0")
	}
	if got, _ := profile["originator"].(string); got != "codex_vscode" {
		t.Fatalf("client_profile[originator] = %q, want codex_vscode", got)
	}
	headers, ok := profile["headers"].(map[string]string)
	if !ok {
		t.Fatalf("client_profile[headers] = %T, want map[string]string", profile["headers"])
	}
	if got := headers["X-Codex-Installation-Id"]; got != "install-1" {
		t.Fatalf("client_profile headers installation id = %q, want install-1", got)
	}
	if _, ok := headers["empty"]; ok {
		t.Fatalf("client_profile headers should omit empty values")
	}
}

func TestBuildAuthFileEntryExposesCodexClientProfileFromHeaders(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth.json",
		FileName: "codex-auth.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "codex@example.com",
			"headers": map[string]any{
				"User-Agent":                            "codex-cli-test/1.0",
				"Originator":                            "codex_vscode",
				"X-Codex-Beta-Features":                 "feature-1",
				"X-Codex-Installation-Id":               "install-1",
				"X-ResponsesAPI-Include-Timing-Metrics": "true",
			},
		},
		Attributes: map[string]string{
			"path": "/tmp/codex-auth.json",
		},
	}

	entry := h.buildAuthFileEntry(auth)
	if got, _ := entry["user_agent"].(string); got != "codex-cli-test/1.0" {
		t.Fatalf("entry[user_agent] = %q, want %q", got, "codex-cli-test/1.0")
	}
	profile, ok := entry["client_profile"].(gin.H)
	if !ok {
		t.Fatalf("entry[client_profile] = %T, want gin.H", entry["client_profile"])
	}
	for key, want := range map[string]string{
		"user_agent":      "codex-cli-test/1.0",
		"originator":      "codex_vscode",
		"beta_features":   "feature-1",
		"installation_id": "install-1",
	} {
		if got, _ := profile[key].(string); got != want {
			t.Fatalf("client_profile[%s] = %q, want %q", key, got, want)
		}
	}
	if got, _ := profile["include_timing_metrics"].(bool); !got {
		t.Fatalf("client_profile[include_timing_metrics] = %#v, want true", profile["include_timing_metrics"])
	}
}

func TestBuildCodexAuthFilePreviewExposesClientProfileFromHeaders(t *testing.T) {
	preview := buildCodexAuthFilePreview(map[string]any{
		"type": "codex",
		"headers": map[string]any{
			"User-Agent":                            "codex-cli-test/1.0",
			"Originator":                            "codex_vscode",
			"X-Codex-Beta-Features":                 "feature-1",
			"X-Codex-Installation-Id":               "install-1",
			"X-ResponsesAPI-Include-Timing-Metrics": "true",
		},
	})

	if preview.UserAgent != "codex-cli-test/1.0" {
		t.Fatalf("preview.UserAgent = %q, want %q", preview.UserAgent, "codex-cli-test/1.0")
	}
	if preview.Originator != "codex_vscode" {
		t.Fatalf("preview.Originator = %q, want codex_vscode", preview.Originator)
	}
	if preview.BetaFeatures != "feature-1" {
		t.Fatalf("preview.BetaFeatures = %q, want feature-1", preview.BetaFeatures)
	}
	if preview.InstallationID != "install-1" {
		t.Fatalf("preview.InstallationID = %q, want install-1", preview.InstallationID)
	}
	if preview.IncludeTimingMetrics == nil || !*preview.IncludeTimingMetrics {
		t.Fatalf("preview.IncludeTimingMetrics = %#v, want true", preview.IncludeTimingMetrics)
	}
}

func TestBuildAuthFileEntrySeparatesProxyPoolRuntimeProxy(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth.json",
		FileName: "codex-auth.json",
		Provider: "codex",
		ProxyURL: "http://pool-proxy.local:7890",
		Metadata: map[string]any{
			"email": "codex@example.com",
		},
		Attributes: map[string]string{
			"path":                "/tmp/codex-auth.json",
			"proxy_pool_assigned": "true",
		},
	}

	entry := h.buildAuthFileEntry(auth)
	if _, ok := entry["proxy_url"]; ok {
		t.Fatalf("entry[proxy_url] = %#v, want omitted for proxy-pool lease", entry["proxy_url"])
	}
	if got, _ := entry["runtime_proxy_url"].(string); got != "http://pool-proxy.local:7890" {
		t.Fatalf("entry[runtime_proxy_url] = %q, want pool proxy", got)
	}
	if got, ok := entry["proxy_pool_assigned"].(bool); !ok || !got {
		t.Fatalf("entry[proxy_pool_assigned] = %#v, want true", entry["proxy_pool_assigned"])
	}
}

func TestBuildAuthFileEntryKeepsSavedProxyURLForProxyPoolAuth(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth.json",
		FileName: "codex-auth.json",
		Provider: "codex",
		ProxyURL: "http://pool-proxy.local:7890",
		Metadata: map[string]any{
			"email":     "codex@example.com",
			"proxy_url": "http://saved-proxy.local:7890",
		},
		Attributes: map[string]string{
			"path":                "/tmp/codex-auth.json",
			"proxy_pool_assigned": "true",
		},
	}

	entry := h.buildAuthFileEntry(auth)
	if got, _ := entry["proxy_url"].(string); got != "http://saved-proxy.local:7890" {
		t.Fatalf("entry[proxy_url] = %q, want saved proxy", got)
	}
	if got, _ := entry["runtime_proxy_url"].(string); got != "http://pool-proxy.local:7890" {
		t.Fatalf("entry[runtime_proxy_url] = %q, want pool proxy", got)
	}
}

func TestBuildAuthFileEntryExposesWebsockets(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth.json",
		FileName: "codex-auth.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path":       "/tmp/codex-auth.json",
			"websockets": "true",
		},
	}

	entry := h.buildAuthFileEntry(auth)
	if got, ok := entry["websockets"].(bool); !ok || !got {
		t.Fatalf("entry[websockets] = %#v, want true", entry["websockets"])
	}
}

func TestBuildAuthFileEntryExposesLastRefreshFromMetadata(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	lastRefresh := time.Date(2026, 5, 9, 13, 6, 47, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:       "oauth-google.json",
		FileName: "oauth-google.json",
		Provider: "oauth",
		Metadata: map[string]any{
			"type":         "oauth",
			"last_refresh": lastRefresh.Format(time.RFC3339),
		},
		Attributes: map[string]string{
			"path": "/tmp/oauth-google.json",
		},
	}

	entry := h.buildAuthFileEntry(auth)
	got, ok := entry["last_refresh"].(time.Time)
	if !ok || !got.Equal(lastRefresh) {
		t.Fatalf("entry[last_refresh] = %#v, want %s", entry["last_refresh"], lastRefresh)
	}
}

func TestBuildAuthFileEntryExposesRuntimeStateTimes(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	runtimeUpdatedAt := time.Date(2026, 5, 9, 13, 27, 26, 0, time.UTC)
	runtimeSavedAt := time.Date(2026, 5, 9, 13, 27, 48, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:       "oauth-google.json",
		FileName: "oauth-google.json",
		Provider: "oauth",
		Metadata: map[string]any{
			"type": "oauth",
			"cliproxy_runtime_state": map[string]any{
				"version":    1,
				"updated_at": runtimeUpdatedAt.Format(time.RFC3339),
				"saved_at":   runtimeSavedAt.Format(time.RFC3339),
			},
		},
		Attributes: map[string]string{
			"path": "/tmp/oauth-google.json",
		},
	}

	entry := h.buildAuthFileEntry(auth)
	gotUpdated, okUpdated := entry["runtime_updated_at"].(time.Time)
	if !okUpdated || !gotUpdated.Equal(runtimeUpdatedAt) {
		t.Fatalf("entry[runtime_updated_at] = %#v, want %s", entry["runtime_updated_at"], runtimeUpdatedAt)
	}
	gotSaved, okSaved := entry["runtime_saved_at"].(time.Time)
	if !okSaved || !gotSaved.Equal(runtimeSavedAt) {
		t.Fatalf("entry[runtime_saved_at] = %#v, want %s", entry["runtime_saved_at"], runtimeSavedAt)
	}
}

func TestBuildAuthFileEntryExposesLastErrorAndModelStates(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:            "claude-auth.json",
		FileName:      "claude-auth.json",
		Provider:      "claude",
		Status:        coreauth.StatusError,
		StatusMessage: "request failed",
		LastError: &coreauth.Error{
			Code:       "upstream_failure",
			Message:    "provider 502",
			Retryable:  true,
			HTTPStatus: http.StatusBadGateway,
		},
		ModelStates: map[string]*coreauth.ModelState{
			"claude-sonnet-4-5": {
				Status:         coreauth.StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
				LastError: &coreauth.Error{
					Code:       "rate_limited",
					Message:    "429 too many requests",
					Retryable:  true,
					HTTPStatus: http.StatusTooManyRequests,
				},
			},
		},
		Attributes: map[string]string{
			"path": "/tmp/claude-auth.json",
		},
	}

	entry := h.buildAuthFileEntry(auth)

	lastError, ok := entry["last_error"].(gin.H)
	if !ok {
		t.Fatalf("entry[last_error] = %#v, want gin.H", entry["last_error"])
	}
	if got := lastError["code"]; got != "upstream_failure" {
		t.Fatalf("entry[last_error][code] = %#v, want %q", got, "upstream_failure")
	}
	if got := lastError["message"]; got != "provider 502" {
		t.Fatalf("entry[last_error][message] = %#v, want %q", got, "provider 502")
	}
	if got := lastError["retryable"]; got != true {
		t.Fatalf("entry[last_error][retryable] = %#v, want true", got)
	}
	if got := lastError["http_status"]; got != http.StatusBadGateway {
		t.Fatalf("entry[last_error][http_status] = %#v, want %d", got, http.StatusBadGateway)
	}

	modelStates, ok := entry["model_states"].(map[string]gin.H)
	if !ok {
		t.Fatalf("entry[model_states] = %#v, want map[string]gin.H", entry["model_states"])
	}
	modelState, ok := modelStates["claude-sonnet-4-5"]
	if !ok {
		t.Fatalf("expected model state for claude-sonnet-4-5, got %#v", modelStates)
	}
	if got := modelState["status_message"]; got != "quota exhausted" {
		t.Fatalf("model_state[status_message] = %#v, want %q", got, "quota exhausted")
	}
	modelLastError, ok := modelState["last_error"].(gin.H)
	if !ok {
		t.Fatalf("model_state[last_error] = %#v, want gin.H", modelState["last_error"])
	}
	if got := modelLastError["code"]; got != "rate_limited" {
		t.Fatalf("model_state[last_error][code] = %#v, want %q", got, "rate_limited")
	}
	if got := modelLastError["http_status"]; got != http.StatusTooManyRequests {
		t.Fatalf("model_state[last_error][http_status] = %#v, want %d", got, http.StatusTooManyRequests)
	}
}
