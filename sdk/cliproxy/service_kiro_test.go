package cliproxy

import (
	"context"
	"strings"
	"testing"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestEnsureExecutorsForAuth_Kiro(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "kiro-auth-1",
		Provider: "kiro",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)

	got, ok := service.coreManager.Executor("kiro")
	if !ok || got == nil {
		t.Fatal("expected kiro executor to be registered")
	}
	if _, ok := got.(*executor.KiroExecutor); !ok {
		t.Fatalf("expected *executor.KiroExecutor, got %T", got)
	}
}

func TestRebindExecutorsRegistersLoadedKiroAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kiro-loaded-auth",
		Provider: "kiro",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}

	service.rebindExecutors()

	got, ok := service.coreManager.Executor("kiro")
	if !ok || got == nil {
		t.Fatal("expected loaded Kiro auth to bind executor")
	}
	if _, ok := got.(*executor.KiroExecutor); !ok {
		t.Fatalf("expected *executor.KiroExecutor, got %T", got)
	}
}

func TestRegisterModelsForAuth_Kiro(t *testing.T) {
	// After the static-fallback removal, an auth without a usable access token
	// does not register any kiro models. The dynamic catalog is fetched via
	// listAvailableModels when a token is present, which is covered by
	// TestConvertKiroAPIModelsAddsDynamicModel below.
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "kiro-auth-models",
		Provider: "kiro",
		Status:   coreauth.StatusActive,
	}
	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	// Probe a handful of previously hard-coded kiro model IDs — none of them
	// should resolve to this tokenless client. This is robust against other
	// registered kiro auths running in the same test binary because
	// ClientSupportsModel checks the specific client ID.
	for _, modelID := range []string{"claude-opus-4.5", "claude-opus-4-5", "auto", "qwen3-coder-next"} {
		if registry.ClientSupportsModel(auth.ID, modelID) {
			t.Fatalf("expected no kiro models for tokenless auth, but %q was registered", modelID)
		}
	}
}

func TestConvertKiroAPIModelsAddsDynamicModel(t *testing.T) {
	converted := convertKiroAPIModels([]*kiroauth.KiroModel{{
		ModelID:        "claude-sonnet-4.7",
		ModelName:      "Claude Sonnet 4.7",
		Description:    "latest sonnet",
		RateMultiplier: 0.25,
		MaxInputTokens: 262144,
	}})

	foundBase := false
	foundAlias := false
	for _, model := range converted {
		if model == nil {
			continue
		}
		switch model.ID {
		case "claude-sonnet-4.7":
			foundBase = model.ContextLength == 262144 && strings.Contains(model.DisplayName, "0.25x credit")
		case "claude-sonnet-4-7":
			foundAlias = model.ContextLength == 262144 && strings.Contains(model.DisplayName, "0.25x credit")
		}
	}
	if !foundBase {
		t.Fatalf("expected converted dynamic model in %+v", converted)
	}
	if !foundAlias {
		t.Fatalf("expected converted dynamic alias model in %+v", converted)
	}
}

func TestKiroModelCacheReturnsFreshClone(t *testing.T) {
	service := &Service{}
	auth := &coreauth.Auth{ID: "kiro-cache-auth", Provider: "kiro"}
	models := []*ModelInfo{{
		ID:            "claude-sonnet-4.7",
		DisplayName:   "Kiro Claude Sonnet 4.7",
		ContextLength: 262144,
	}}

	service.storeKiroModelCache(auth, models)
	models[0].DisplayName = "mutated source"

	cached, ok := service.cachedKiroModels(auth)
	if !ok || len(cached) != 1 {
		t.Fatalf("cachedKiroModels() ok=%v len=%d", ok, len(cached))
	}
	if cached[0].DisplayName != "Kiro Claude Sonnet 4.7" {
		t.Fatalf("cache did not preserve stored clone: %+v", cached[0])
	}
	cached[0].DisplayName = "mutated result"
	again, ok := service.cachedKiroModels(auth)
	if !ok || again[0].DisplayName != "Kiro Claude Sonnet 4.7" {
		t.Fatalf("cache returned shared model pointer: ok=%v models=%+v", ok, again)
	}
}

func TestKiroModelCacheIgnoresExpiredEntry(t *testing.T) {
	service := &Service{
		kiroModelCache: map[string]kiroModelCacheEntry{
			"kiro-cache-auth": {
				models:    []*ModelInfo{{ID: "claude-sonnet-4.7"}},
				fetchedAt: time.Now().Add(-kiroModelCacheTTL - time.Minute),
			},
		},
	}
	if cached, ok := service.cachedKiroModels(&coreauth.Auth{ID: "kiro-cache-auth"}); ok || len(cached) != 0 {
		t.Fatalf("expected expired cache miss, got ok=%v models=%+v", ok, cached)
	}
}

func TestExtractKiroTokenDataReadsAuthFileMetadata(t *testing.T) {
	tokenData := extractKiroTokenData(&coreauth.Auth{
		Metadata: map[string]any{
			"access_token":  "access",
			"refresh_token": "refresh",
			"profileArn":    "profile",
			"clientId":      "client",
		},
	})
	if tokenData == nil {
		t.Fatal("expected token data")
	}
	if tokenData.AccessToken != "access" || tokenData.RefreshToken != "refresh" || tokenData.ProfileArn != "profile" || tokenData.ClientID != "client" {
		t.Fatalf("unexpected token data: %+v", tokenData)
	}
}

func TestExtractKiroTokenDataPrefersRefreshedMetadataOverAttributes(t *testing.T) {
	tokenData := extractKiroTokenData(&coreauth.Auth{
		Attributes: map[string]string{
			"access_token":  "stale-access",
			"refresh_token": "stale-refresh",
			"profile_arn":   "stale-profile",
		},
		Metadata: map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "fresh-refresh",
			"profile_arn":   "fresh-profile",
		},
	})
	if tokenData == nil {
		t.Fatal("expected token data")
	}
	if tokenData.AccessToken != "fresh-access" || tokenData.RefreshToken != "fresh-refresh" || tokenData.ProfileArn != "fresh-profile" {
		t.Fatalf("unexpected token data: %+v", tokenData)
	}
}
