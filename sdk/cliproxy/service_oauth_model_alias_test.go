package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestApplyOAuthModelAlias_Rename(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "g5" {
		t.Fatalf("expected model id %q, got %q", "g5", out[0].ID)
	}
	if out[0].Name != "models/g5" {
		t.Fatalf("expected model name %q, got %q", "models/g5", out[0].Name)
	}
}

func TestApplyOAuthModelAlias_MatchesProviderFacingName(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "provider-gpt-5", Alias: "public-gpt-5"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "catalog-gpt-5", Name: "provider-gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "public-gpt-5" {
		t.Fatalf("expected model id %q, got %q", "public-gpt-5", out[0].ID)
	}
	if out[0].Name != "provider-gpt-5" {
		t.Fatalf("expected provider-facing name %q, got %q", "provider-gpt-5", out[0].Name)
	}
}

func TestApplyOAuthModelAlias_ForkAddsAlias(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 2 {
		t.Fatalf("expected 2 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
}

func TestApplyOAuthModelAlias_ForkAddsMultipleAliases(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true},
				{Name: "gpt-5", Alias: "g5-2", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 3 {
		t.Fatalf("expected 3 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
	if out[2].ID != "g5-2" {
		t.Fatalf("expected third model id %q, got %q", "g5-2", out[2].ID)
	}
	if out[2].Name != "models/g5-2" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5-2", out[2].Name)
	}
}

func TestApplyOAuthModelAlias_EffortOnlyRuleKeepsSingleModel(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{
				Name:            "gpt-5.6-sol",
				Alias:           "gpt-5.6-sol",
				ReasoningEffort: map[string]string{"max": "xhigh"},
			}},
		},
	}
	models := []*ModelInfo{{ID: "gpt-5.6-sol", Name: "models/gpt-5.6-sol"}}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected effort-only rule to keep one listed model, got %d", len(out))
	}
	if out[0].ID != "gpt-5.6-sol" || out[0].Name != "models/gpt-5.6-sol" {
		t.Fatalf("effort-only rule changed listed model: %#v", out[0])
	}
}

func TestApplyOAuthModelAlias_OverridesNativeAliasID(t *testing.T) {
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"codex": {{Name: "gpt-5.6-luna", Alias: "gpt-6-astra"}},
	}}
	native := &ModelInfo{ID: "gpt-6-astra", DisplayName: "native Astra"}
	upstream := &ModelInfo{ID: "gpt-5.6-luna", DisplayName: "Luna"}

	for _, models := range [][]*ModelInfo{
		{native, upstream},
		{upstream, native},
	} {
		out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
		if len(out) != 1 || out[0].ID != "gpt-6-astra" {
			t.Fatalf("alias output = %#v, want only configured alias", out)
		}
		if out[0].DisplayName != "Luna" {
			t.Fatalf("alias metadata came from native model: %#v", out[0])
		}
	}
	withoutSource := applyOAuthModelAlias(cfg, "codex", "oauth", []*ModelInfo{native})
	if len(withoutSource) != 1 || withoutSource[0] != native {
		t.Fatalf("native model disappeared without an available alias source: %#v", withoutSource)
	}
}

func TestRegisterModelsForAuthOAuthAliasSurvivesUpstreamExclusion(t *testing.T) {
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"codex": {{Name: "gpt-5.6-luna", Alias: "gpt-6-astra"}},
	}, OAuthExcludedModels: map[string][]string{"codex": {"gpt-6-astra"}}}
	s := &Service{cfg: cfg}
	a := &coreauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}
	r := registry.GetGlobalRegistry()
	t.Cleanup(func() { r.UnregisterClient(a.ID) })
	s.registerModelsForAuth(a)
	models := r.GetModelsForClient(a.ID)
	if len(models) == 0 {
		t.Fatal("upstream exclusion removed all models")
	}
	seen := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		if model != nil {
			seen[model.ID] = model
		}
	}
	if model := seen["gpt-6-astra"]; model == nil || model.DisplayName != "GPT 5.6 Luna" {
		t.Fatalf("configured alias was not retained with upstream metadata: %#v", model)
	}
	if _, native := seen["gpt-5.6-luna"]; native {
		t.Fatal("rename alias unexpectedly kept the upstream model")
	}
}
