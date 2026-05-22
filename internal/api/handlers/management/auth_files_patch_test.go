package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		"type":  "kiro",
		"email": "kiro@example.com",
		"cliproxy_runtime_state": map[string]any{
			"version":        1,
			"status":         "error",
			"status_message": "refresh token invalid - re-login required",
			"unavailable":    true,
			"updated_at":     runtimeUpdatedAt.Format(time.RFC3339),
			"saved_at":       runtimeSavedAt.Format(time.RFC3339),
			"last_error": map[string]any{
				"code":        "refresh_failed",
				"message":     "kiro refresh rejected",
				"retryable":   false,
				"http_status": http.StatusUnauthorized,
			},
		},
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kiro-google.json"), data, 0o600); err != nil {
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
	if got := lastError["message"]; got != "kiro refresh rejected" {
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
		{id: "charlie.json", name: "charlie.json", provider: "kiro"},
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
	if body.TypeCounts["all"] != 3 || body.TypeCounts["codex"] != 2 || body.TypeCounts["kiro"] != 1 {
		t.Fatalf("type_counts = %#v", body.TypeCounts)
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
		ID:       "kiro-google.json",
		FileName: "kiro-google.json",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":         "kiro",
			"last_refresh": lastRefresh.Format(time.RFC3339),
		},
		Attributes: map[string]string{
			"path": "/tmp/kiro-google.json",
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
		ID:       "kiro-google.json",
		FileName: "kiro-google.json",
		Provider: "kiro",
		Metadata: map[string]any{
			"type": "kiro",
			"cliproxy_runtime_state": map[string]any{
				"version":    1,
				"updated_at": runtimeUpdatedAt.Format(time.RFC3339),
				"saved_at":   runtimeSavedAt.Format(time.RFC3339),
			},
		},
		Attributes: map[string]string{
			"path": "/tmp/kiro-google.json",
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
