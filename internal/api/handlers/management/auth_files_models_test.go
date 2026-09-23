package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGetAuthFileModelsRefreshesCodexBeforeReadingRegistry(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-model-refresh.json",
		FileName: "codex-model-refresh.json",
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	t.Cleanup(h.Close)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	refreshCalls := 0
	h.SetAuthFileModelRefreshHandler(func(_ context.Context, got *coreauth.Auth) error {
		refreshCalls++
		if got == nil || got.ID != auth.ID {
			t.Fatalf("refresh auth = %#v, want ID %q", got, auth.ID)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{
			ID:      "new-upstream-model",
			Object:  "model",
			OwnedBy: "openai",
			Type:    "openai",
		}})
		return nil
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/models?name=codex-model-refresh.json", nil)
	h.GetAuthFileModels(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	var payload struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Models) != 1 || payload.Models[0].ID != "new-upstream-model" {
		t.Fatalf("models = %#v, want refreshed model", payload.Models)
	}
}

func TestGetAuthFileModelsReturnsBadGatewayWhenRefreshFails(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-model-refresh-error.json",
		FileName: "codex-model-refresh-error.json",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	t.Cleanup(h.Close)
	h.SetAuthFileModelRefreshHandler(func(context.Context, *coreauth.Auth) error {
		return errors.New("upstream catalog unavailable")
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/models?name=codex-model-refresh-error.json", nil)
	h.GetAuthFileModels(c)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["error"] != "failed to refresh auth file models" || payload["detail"] != "upstream catalog unavailable" {
		t.Fatalf("response = %#v", payload)
	}
}
