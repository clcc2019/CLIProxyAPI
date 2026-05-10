package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

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
