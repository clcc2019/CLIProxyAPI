package cliproxy

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestRegisterModelsForAuthCodexAPIKeyUsesOnlyMatchingConfigModels(t *testing.T) {
	const authID = "codex-permissions-explicit"
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(authID)
	t.Cleanup(func() { registry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{CodexKey: []config.CodexKey{{
		APIKey: "configured-key",
		Models: []internalconfig.CodexModel{{Name: "gpt-6.1-sol", Alias: "gpt-6.1-sol"}},
	}}}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"api_key":      "configured-key",
			"config_index": "0",
			"source":       "config:codex[test]",
			"plan_type":    "plus",
		},
	}

	service.registerModelsForAuth(auth)
	models := registry.GetModelsForClient(authID)
	if len(models) != 1 || models[0].ID != "gpt-6.1-sol" {
		t.Fatalf("registered models = %#v, want only the configured model", models)
	}
}

func TestRegisterModelsForAuthCodexAPIKeyDefaultsRequireMatchingConfigEntry(t *testing.T) {
	const authID = "codex-permissions-mismatch"
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(authID)
	registry.RegisterClient(authID, "codex", []*internalregistry.ModelInfo{{ID: "stale-model"}})
	t.Cleanup(func() { registry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{CodexKey: []config.CodexKey{{APIKey: "configured-key"}}}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"api_key":      "stale-key",
			"config_index": "0",
			"source":       "config:codex[stale]",
			"plan_type":    "free",
		},
	}

	service.registerModelsForAuth(auth)
	if models := registry.GetModelsForClient(authID); len(models) != 0 {
		t.Fatalf("mismatched API key retained model permissions: %#v", models)
	}
}

func TestRegisterModelsForAuthCodexAPIKeyStaleIndexFallsBackToMatchingCredentials(t *testing.T) {
	const authID = "codex-permissions-stale-index"
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(authID)
	t.Cleanup(func() { registry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{CodexKey: []config.CodexKey{
		{APIKey: "old-key", Models: []internalconfig.CodexModel{{Name: "old-model", Alias: "old-model"}}},
		{APIKey: "current-key"},
	}}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"api_key":      "current-key",
			"config_index": "0",
			"source":       "config:codex[stale]",
		},
	}

	service.registerModelsForAuth(auth)
	want := modelIDs(internalregistry.GetCodexProModels())
	got := modelIDs(registry.GetModelsForClient(authID))
	if len(got) != len(want) {
		t.Fatalf("registered model count = %d, want %d (got %v)", len(got), len(want), got)
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Fatalf("registered models omitted default model %q: %v", id, got)
		}
	}
}

func TestRegisterModelsForAuthCodexOAuthDoesNotUseAccountCatalogAsEntitlements(t *testing.T) {
	const authID = "codex-permissions-oauth"
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(authID)
	t.Cleanup(func() { registry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"plan_type": "plus",
		},
		Metadata: map[string]any{"access_token": "test-token"},
	}
	service.codexRemoteCatalogs.Store(authID, codexRemoteCatalogCacheEntry{
		payload:   []byte(`{"models":[{"slug":"gpt-6.1-sol"}]}`),
		fetchedAt: time.Now(),
		sourceKey: codexRemoteCatalogSourceKey(auth, codexServiceBaseURL(auth), ""),
	})

	service.registerModelsForAuth(auth)
	got := modelIDs(registry.GetModelsForClient(authID))
	if _, ok := got["gpt-6.1-sol"]; ok {
		t.Fatalf("unsupported account-catalog model was registered: %v", got)
	}
	if _, ok := got["gpt-5.6-sol"]; !ok {
		t.Fatalf("plus plan model missing from OAuth registration: %v", got)
	}
}

func TestRegisterModelsForAuthCodexUsesMetadataAuthKindPrecedence(t *testing.T) {
	const authID = "codex-permissions-metadata-kind"
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(authID)
	t.Cleanup(func() { registry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{CodexKey: []config.CodexKey{{
		APIKey: "configured-key",
		Models: []internalconfig.CodexModel{{Name: "configured-only", Alias: "configured-only"}},
	}}}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"api_key":      "configured-key",
			"config_index": "0",
			"source":       "config:codex[test]",
			"plan_type":    "plus",
		},
		Metadata: map[string]any{
			"auth_kind":    "oauth",
			"access_token": "oauth-token",
		},
	}

	service.registerModelsForAuth(auth)
	got := modelIDs(registry.GetModelsForClient(authID))
	if _, ok := got["configured-only"]; ok {
		t.Fatalf("stale API-key attributes overrode metadata OAuth kind: %v", got)
	}
	if _, ok := got["gpt-5.6-sol"]; !ok {
		t.Fatalf("OAuth plan model missing after metadata kind resolution: %v", got)
	}
}

func TestRegisterModelsForAuthCodexLegacyAPIKeyFieldBeatsOAuthAccountFallback(t *testing.T) {
	const authID = "codex-permissions-legacy-api-key"
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(authID)
	t.Cleanup(func() { registry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{CodexKey: []config.CodexKey{{
		APIKey: "legacy-key",
		Models: []internalconfig.CodexModel{{Name: "legacy-upstream", Alias: "legacy-model"}},
	}}}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":      "legacy-key",
			"config_index": "0",
			"source":       "config:codex[test]",
		},
		Metadata: map[string]any{"email": "legacy@example.test"},
	}

	service.registerModelsForAuth(auth)
	got := modelIDs(registry.GetModelsForClient(authID))
	if _, ok := got["legacy-model"]; !ok {
		t.Fatalf("legacy API-key auth was treated as OAuth: %v", got)
	}
}

func modelIDs(models []*internalregistry.ModelInfo) map[string]struct{} {
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil && model.ID != "" {
			ids[model.ID] = struct{}{}
		}
	}
	return ids
}
