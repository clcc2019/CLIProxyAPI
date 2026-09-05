package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestClientModelAllowedForContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("accessMetadata", map[string]string{
		"allowed_models":  "gpt-5*, claude-*",
		"excluded_models": "*-mini",
	})

	ctx := context.WithValue(context.Background(), "gin", c)

	if !clientModelAllowedForContext(ctx, "gpt-5") {
		t.Fatalf("expected gpt-5 to be allowed")
	}
	if !clientModelAllowedForContext(ctx, "models/gpt-5") {
		t.Fatalf("expected codex model prefix to be canonicalized")
	}
	if clientModelAllowedForContext(ctx, "gpt-5-mini") {
		t.Fatalf("expected excluded model to be denied")
	}
	if clientModelAllowedForContext(ctx, "gpt-4o") {
		t.Fatalf("expected non-allowed model to be denied")
	}
}

func TestResolveClientAuthFileIDs(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: "codex-a.json", FileName: "codex-a.json", Provider: "codex"})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	ids, missing := resolveClientAuthFileIDs(manager, []string{"./codex-a.json", "missing.json"})
	if len(ids) != 1 || ids[0] != "codex-a.json" || len(missing) != 1 || missing[0] != "missing.json" {
		t.Fatalf("resolved ids=%v missing=%v", ids, missing)
	}
}

func TestClientAuthFilesFromGin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	encoded, _ := json.Marshal([]string{"a.json", "b.json"})
	c.Set("accessMetadata", map[string]string{coreexecutor.ClientAuthFilesMetadataKey: string(encoded)})
	if got := clientAuthFilesFromGin(c); len(got) != 2 || got[0] != "a.json" || got[1] != "b.json" {
		t.Fatalf("auth files = %#v", got)
	}
}

func TestFilterModelMapsForClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("accessMetadata", map[string]string{
		"allowed_models":  "gpt-5*",
		"excluded_models": "*-mini",
	})

	models := []map[string]any{
		{"id": "gpt-5"},
		{"id": "gpt-5-mini"},
		{"name": "gpt-4o"},
	}

	filtered := FilterModelMapsForClient(c, models)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 model after filtering, got %d", len(filtered))
	}
	if got := filtered[0]["id"]; got != "gpt-5" {
		t.Fatalf("unexpected remaining model: %#v", filtered[0])
	}
}

func TestFilterOpenAIModelSummariesForClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("accessMetadata", map[string]string{
		"allowed_models":  "claude-*",
		"excluded_models": "*-haiku",
	})

	models := []registry.OpenAIModelSummary{
		{ID: "claude-sonnet-4-5"},
		{ID: "claude-3-5-haiku"},
		{ID: "gpt-5"},
	}

	filtered := FilterOpenAIModelSummariesForClient(c, models)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 model after filtering, got %d", len(filtered))
	}
	if filtered[0].ID != "claude-sonnet-4-5" {
		t.Fatalf("unexpected remaining model: %#v", filtered[0])
	}
}
