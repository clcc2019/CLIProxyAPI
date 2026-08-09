package cliproxy

import (
	"context"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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

func TestRegisterModelsForAuth_APIKeyExcludedModelsMatchUpstreamNameWithAlias(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			ClaudeKey: []config.ClaudeKey{{
				APIKey: "claude-api-key",
				Models: []internalconfig.ClaudeModel{
					{Name: "claude-upstream-a", Alias: "claude-public-a"},
					{Name: "claude-upstream-b", Alias: "claude-public-b"},
				},
				ExcludedModels: []string{"claude-upstream-a"},
			}},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-claude-api-key-alias-exclusion",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "api_key",
			"api_key":   "claude-api-key",
		},
	}

	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() { registry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(auth)
	models := registry.GetModelsForClient(auth.ID)
	seen := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		if model != nil {
			seen[model.ID] = model
		}
	}
	if _, ok := seen["claude-public-a"]; ok {
		t.Fatal("upstream-name exclusion must remove the aliased public model")
	}
	if model := seen["claude-public-b"]; model == nil || model.Name != "claude-upstream-b" {
		t.Fatalf("expected remaining model to retain upstream identity, got %#v", model)
	}
}

func TestRegisterModelsForAuth_StatusDisabledDoesNotRegisterModels(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "auth-status-disabled",
		Provider: "claude",
		Status:   coreauth.StatusDisabled,
	}
	registry := internalregistry.GetGlobalRegistry()
	registry.RegisterClient(auth.ID, "claude", []*ModelInfo{{ID: "stale-model"}})
	t.Cleanup(func() { registry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(auth)
	if models := registry.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("status-disabled auth still has registered models: %#v", models)
	}
}

func TestApplyConfigUpdate_RefreshesConfigDependentModelRegistrations(t *testing.T) {
	const authID = "auth-config-reload-models"
	service := &Service{
		cfg: &config.Config{
			ClaudeKey: []config.ClaudeKey{{
				APIKey: "claude-api-key",
				Models: []internalconfig.ClaudeModel{{Name: "claude-upstream-v1", Alias: "claude-public-v1"}},
			}},
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "api_key",
			"api_key":   "claude-api-key",
		},
	}
	if _, err := service.coreManager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(authID)
	t.Cleanup(func() { registry.UnregisterClient(authID) })

	service.registerModelsForAuth(auth)
	if models := registry.GetModelsForClient(authID); len(models) != 1 || models[0].ID != "claude-public-v1" {
		t.Fatalf("initial registered models = %#v, want claude-public-v1", models)
	}

	service.applyConfigUpdate(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey: "claude-api-key",
			Models: []internalconfig.ClaudeModel{{Name: "claude-upstream-v2", Alias: "claude-public-v2"}},
		}},
	})

	models := registry.GetModelsForClient(authID)
	if len(models) != 1 || models[0].ID != "claude-public-v2" || models[0].Name != "claude-upstream-v2" {
		t.Fatalf("reloaded registered models = %#v, want v2 alias/upstream", models)
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

func TestRegisterModelsForAuth_OpenAICompatibilityEmptyNameUsesBaseURL(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{{
				BaseURL: "https://anonymous.example/v1",
				Models: []config.OpenAICompatibilityModel{{
					Name:  "upstream-model",
					Alias: "public-model",
				}},
			}},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-empty-name",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"base_url":     "https://anonymous.example/v1/",
			"compat_name":  "",
			"provider_key": "openai-compatibility",
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(auth)
	models := modelRegistry.GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != "public-model" || models[0].Name != "upstream-model" {
		t.Fatalf("registered models = %#v, want public-model", models)
	}
}

func TestApplyPersistedConfigUpdateRefreshesOpenAICompatibilityModels(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "provider",
				BaseURL: "https://provider.example/v1",
				Models:  []config.OpenAICompatibilityModel{{Name: "upstream-v1", Alias: "public-v1"}},
			}},
		},
		coreManager: manager,
	}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-persisted-update",
		Provider: "provider",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"base_url":     "https://provider.example/v1",
			"compat_name":  "provider",
			"provider_key": "provider",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })
	service.registerModelsForAuth(auth)

	service.applyPersistedConfigUpdate(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "provider",
			BaseURL: "https://provider.example/v1",
			Models:  []config.OpenAICompatibilityModel{{Name: "upstream-v2", Alias: "public-v2"}},
		}},
	})

	models := modelRegistry.GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != "public-v2" || models[0].Name != "upstream-v2" {
		t.Fatalf("registered models = %#v, want public-v2", models)
	}
}

func BenchmarkMatchWildcard(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if !matchWildcard("claude*sonnet*4-5", "claude-3-5-sonnet-4-5") {
			b.Fatal("expected wildcard pattern to match")
		}
	}
}
