package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGetAuthFileModelsUsesRegisteredModelsWithoutRefreshingCodexCatalog(t *testing.T) {
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
		return nil
	})
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{
		ID:      "registered-model",
		Object:  "model",
		OwnedBy: "openai",
		Type:    "openai",
	}})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/models?name=codex-model-refresh.json", nil)
	h.GetAuthFileModels(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want 0", refreshCalls)
	}
	var payload struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Models) != 1 || payload.Models[0].ID != "registered-model" {
		t.Fatalf("models = %#v, want registered model", payload.Models)
	}
}

func TestGetAuthFileModelsIgnoresCodexRefreshErrors(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-model-refresh-error.json",
		FileName: "codex-model-refresh-error.json",
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	t.Cleanup(h.Close)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h.SetAuthFileModelRefreshHandler(func(context.Context, *coreauth.Auth) error {
		return context.Canceled
	})
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{
		ID:      "static-model",
		Object:  "model",
		OwnedBy: "openai",
		Type:    "openai",
	}})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/models?name=codex-model-refresh-error.json", nil)
	h.GetAuthFileModels(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Models) != 1 || payload.Models[0].ID != "static-model" {
		t.Fatalf("models = %#v, want static model", payload.Models)
	}
}
