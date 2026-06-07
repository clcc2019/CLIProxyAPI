package cliproxy

import (
	"strings"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestMatchWildcard(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		value   string
		want    bool
	}{
		{name: "exact", pattern: "claude-sonnet", value: "claude-sonnet", want: true},
		{name: "prefix wildcard", pattern: "claude-*", value: "claude-sonnet-4-5", want: true},
		{name: "suffix wildcard", pattern: "*-4-5", value: "claude-sonnet-4-5", want: true},
		{name: "middle segments in order", pattern: "claude*sonnet*4-5", value: "claude-3-5-sonnet-4-5", want: true},
		{name: "middle segments out of order", pattern: "claude*4-5*sonnet", value: "claude-3-5-sonnet-4-5", want: false},
		{name: "consecutive wildcards", pattern: "gpt-**preview", value: "gpt-5-preview", want: true},
		{name: "empty pattern", pattern: "", value: "gpt-5", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchWildcard(tt.pattern, tt.value)
			if got != tt.want {
				t.Fatalf("matchWildcard(%q, %q) = %t, want %t", tt.pattern, tt.value, got, tt.want)
			}
		})
	}
}

func TestRegisterModelsForAuth_UsesPreMergedExcludedModelsAttribute(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"claude": {"claude-opus-4-5-20251101"},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-claude",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": "claude-sonnet-4-5-20250929",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	models := registry.GetAvailableModelsByProvider("claude")
	if len(models) == 0 {
		t.Fatal("expected claude models to be registered")
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if strings.EqualFold(modelID, "claude-sonnet-4-5-20250929") {
			t.Fatalf("expected model %q to be excluded by auth attribute", modelID)
		}
	}

	seenGlobalExcluded := false
	for _, model := range models {
		if model == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(model.ID), "claude-opus-4-5-20251101") {
			seenGlobalExcluded = true
			break
		}
	}
	if !seenGlobalExcluded {
		t.Fatal("expected global excluded model to be present when attribute override is set")
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityImageModelType(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "images",
					BaseURL: "https://example.com/v1",
					Models: []config.OpenAICompatibilityModel{
						{Name: "upstream-image", Alias: "compat-image", Image: true},
						{Name: "upstream-chat", Alias: "compat-chat"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-image",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"compat_name":  "images",
			"provider_key": "images",
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	models := modelRegistry.GetModelsForClient(auth.ID)
	var imageModel *internalregistry.ModelInfo
	var chatModel *internalregistry.ModelInfo
	for _, model := range models {
		if model == nil {
			continue
		}
		switch strings.TrimSpace(model.ID) {
		case "compat-image":
			imageModel = model
		case "compat-chat":
			chatModel = model
		}
	}
	if imageModel == nil {
		t.Fatal("expected compat-image to be registered")
	}
	if imageModel.Type != internalregistry.OpenAIImageModelType {
		t.Fatalf("image model type = %q, want %q", imageModel.Type, internalregistry.OpenAIImageModelType)
	}
	if imageModel.Thinking != nil {
		t.Fatalf("image model thinking = %+v, want nil", imageModel.Thinking)
	}
	if chatModel == nil {
		t.Fatal("expected compat-chat to be registered")
	}
	if chatModel.Type != "openai-compatibility" {
		t.Fatalf("chat model type = %q, want openai-compatibility", chatModel.Type)
	}
	if chatModel.Thinking == nil {
		t.Fatal("expected chat model to keep default thinking support")
	}
}

func BenchmarkMatchWildcard(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if !matchWildcard("claude*sonnet*4-5", "claude-3-5-sonnet-4-5") {
			b.Fatal("expected wildcard pattern to match")
		}
	}
}
