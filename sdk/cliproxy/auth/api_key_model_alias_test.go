package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestLookupAPIKeyUpstreamModel(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey:  "k",
				BaseURL: "https://example.com",
				Models: []internalconfig.ClaudeModel{
					{Name: "claude-sonnet-4", Alias: "cs4"},
					{Name: "claude-haiku-4(low)", Alias: "ch4"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "claude", Attributes: map[string]string{"api_key": "k", "base_url": "https://example.com"}})
	if !mgr.hasAPIKeyModelAliasSnapshot("a1") {
		t.Fatal("registered API-key auth should have an alias snapshot")
	}

	tests := []struct {
		name   string
		authID string
		input  string
		want   string
	}{
		// Fast path + suffix preservation
		{"alias with suffix", "a1", "cs4(8192)", "claude-sonnet-4(8192)"},
		{"alias without suffix", "a1", "cs4", "claude-sonnet-4"},

		// Config suffix takes priority
		{"config suffix priority", "a1", "ch4(high)", "claude-haiku-4(low)"},
		{"config suffix no user suffix", "a1", "ch4", "claude-haiku-4(low)"},

		// Case insensitive
		{"uppercase alias", "a1", "CS4", "claude-sonnet-4"},
		{"mixed case with suffix", "a1", "Cs4(4096)", "claude-sonnet-4(4096)"},

		// Direct name lookup
		{"upstream name direct", "a1", "claude-sonnet-4", "claude-sonnet-4"},
		{"upstream name with suffix", "a1", "claude-sonnet-4(8192)", "claude-sonnet-4(8192)"},

		// Cache miss scenarios
		{"non-existent auth", "non-existent", "cs4", ""},
		{"unknown alias", "a1", "unknown-alias", ""},
		{"empty auth ID", "", "cs4", ""},
		{"empty model", "a1", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input)
			if resolved != tt.want {
				t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
			}
		})
	}
}

func TestLookupAPIKeyUpstreamModel_PreservesExactSuffixAliases(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{{
			APIKey: "k",
			Models: []internalconfig.ClaudeModel{
				{Name: "claude-upstream-high", Alias: "claude-public(high)"},
				{Name: "claude-upstream-low", Alias: "claude-public(low)"},
			},
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)
	_, _ = mgr.Register(context.Background(), &Auth{
		ID:       "suffix-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "k",
		},
	})

	for input, want := range map[string]string{
		"claude-public(high)": "claude-upstream-high(high)",
		"claude-public(low)":  "claude-upstream-low(low)",
		"claude-public":       "",
	} {
		if got := mgr.lookupAPIKeyUpstreamModel("suffix-auth", input); got != want {
			t.Errorf("lookupAPIKeyUpstreamModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLookupAPIKeyUpstreamModel_DoesNotCollapseSuffixQualifiedNames(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{{
			APIKey: "k",
			Models: []internalconfig.ClaudeModel{
				{Name: "claude-upstream(high)", Alias: "claude-public(high)"},
			},
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)
	_, _ = mgr.Register(context.Background(), &Auth{
		ID:       "suffix-name-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "k",
		},
	})

	if got := mgr.lookupAPIKeyUpstreamModel("suffix-name-auth", "claude-upstream(low)"); got != "" {
		t.Fatalf("lookupAPIKeyUpstreamModel() = %q, want no cross-suffix match", got)
	}
	if got := mgr.lookupAPIKeyUpstreamModel("suffix-name-auth", "claude-upstream(high)"); got != "claude-upstream(high)" {
		t.Fatalf("lookupAPIKeyUpstreamModel() = %q, want exact upstream name", got)
	}
}

func TestAPIKeyModelAlias_ConfigHotReload(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey: "k",
				Models: []internalconfig.ClaudeModel{{Name: "claude-sonnet-4", Alias: "cs4"}},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "claude", Attributes: map[string]string{"api_key": "k"}})

	// Initial alias
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "cs4"); resolved != "claude-sonnet-4" {
		t.Fatalf("before reload: got %q, want %q", resolved, "claude-sonnet-4")
	}

	// Hot reload with new alias
	mgr.SetConfig(&internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey: "k",
				Models: []internalconfig.ClaudeModel{{Name: "claude-haiku-4", Alias: "cs4"}},
			},
		},
	})

	// New alias should take effect
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "cs4"); resolved != "claude-haiku-4" {
		t.Fatalf("after reload: got %q, want %q", resolved, "claude-haiku-4")
	}
}

func TestAPIKeyModelAlias_MultipleProviders(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{{APIKey: "claude-key", Models: []internalconfig.ClaudeModel{{Name: "claude-sonnet-4", Alias: "cs4"}}}},
		CodexKey:  []internalconfig.CodexKey{{APIKey: "codex-key", Models: []internalconfig.CodexModel{{Name: "o3", Alias: "o"}}}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}})
	_, _ = mgr.Register(ctx, &Auth{ID: "codex-auth", Provider: "codex", Attributes: map[string]string{"api_key": "codex-key"}})

	tests := []struct {
		authID, input, want string
	}{
		{"claude-auth", "cs4", "claude-sonnet-4"},
		{"codex-auth", "o", "o3"},
	}

	for _, tt := range tests {
		if resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input); resolved != tt.want {
			t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
		}
	}
}

func TestApplyAPIKeyModelAlias(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{APIKey: "k", Models: []internalconfig.ClaudeModel{{Name: "claude-sonnet-4", Alias: "cs4"}}},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	apiKeyAuth := &Auth{ID: "a1", Provider: "claude", Attributes: map[string]string{"api_key": "k"}}
	oauthAuth := &Auth{ID: "oauth-auth", Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	_, _ = mgr.Register(ctx, apiKeyAuth)

	tests := []struct {
		name       string
		auth       *Auth
		inputModel string
		wantModel  string
	}{
		{
			name:       "api_key auth with alias",
			auth:       apiKeyAuth,
			inputModel: "cs4(8192)",
			wantModel:  "claude-sonnet-4(8192)",
		},
		{
			name:       "oauth auth passthrough",
			auth:       oauthAuth,
			inputModel: "some-model",
			wantModel:  "some-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolvedModel := mgr.applyAPIKeyModelAlias(tt.auth, tt.inputModel)

			if resolvedModel != tt.wantModel {
				t.Errorf("model = %q, want %q", resolvedModel, tt.wantModel)
			}
		})
	}
}
