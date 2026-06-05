package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
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
