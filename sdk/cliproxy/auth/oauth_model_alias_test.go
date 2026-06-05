package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestResolveOAuthUpstreamModel_SuffixPreservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		aliases map[string][]internalconfig.OAuthModelAlias
		channel string
		input   string
		want    string
	}{
		{
			name: "numeric suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "grok-4"}},
			},
			channel: "xai",
			input:   "grok-4(8192)",
			want:    "grok-4-fast(8192)",
		},
		{
			name: "level suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5(high)",
			want:    "claude-sonnet-4-5-20250514(high)",
		},
		{
			name: "no suffix unchanged",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "grok-4"}},
			},
			channel: "xai",
			input:   "grok-4",
			want:    "grok-4-fast",
		},
		{
			name: "config suffix takes priority",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514(low)", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5(high)",
			want:    "claude-sonnet-4-5-20250514(low)",
		},
		{
			name: "auto suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "grok-4"}},
			},
			channel: "xai",
			input:   "grok-4(auto)",
			want:    "grok-4-fast(auto)",
		},
		{
			name: "none suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "grok-4"}},
			},
			channel: "xai",
			input:   "grok-4(none)",
			want:    "grok-4-fast(none)",
		},
		{
			name: "kimi suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"kimi": {{Name: "kimi-k2.5", Alias: "k2.5"}},
			},
			channel: "kimi",
			input:   "k2.5(high)",
			want:    "kimi-k2.5(high)",
		},
		{
			name: "case insensitive alias lookup with suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "Grok-4"}},
			},
			channel: "xai",
			input:   "grok-4(high)",
			want:    "grok-4-fast(high)",
		},
		{
			name: "no alias returns empty",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "grok-4"}},
			},
			channel: "xai",
			input:   "unknown-model(high)",
			want:    "",
		},
		{
			name: "wrong channel returns empty",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "grok-4"}},
			},
			channel: "claude",
			input:   "grok-4(high)",
			want:    "",
		},
		{
			name: "empty suffix filtered out",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "grok-4"}},
			},
			channel: "xai",
			input:   "grok-4()",
			want:    "grok-4-fast",
		},
		{
			name: "incomplete suffix treated as no suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"xai": {{Name: "grok-4-fast", Alias: "grok-4(high"}},
			},
			channel: "xai",
			input:   "grok-4(high",
			want:    "grok-4-fast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mgr := NewManager(nil, nil, nil)
			mgr.SetConfig(&internalconfig.Config{})
			mgr.SetOAuthModelAlias(tt.aliases)

			auth := createAuthForChannel(tt.channel)
			got := mgr.resolveOAuthUpstreamModel(auth, tt.input)
			if got != tt.want {
				t.Errorf("resolveOAuthUpstreamModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func createAuthForChannel(channel string) *Auth {
	switch channel {
	case "claude":
		return &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	case "codex":
		return &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}
	case "kimi":
		return &Auth{Provider: "kimi"}
	case "xai":
		return &Auth{Provider: "xai"}
	default:
		return &Auth{Provider: channel}
	}
}

func TestOAuthModelAliasChannel_DirectProviders(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{"kimi", "xai"} {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			if got := OAuthModelAliasChannel(provider, "oauth"); got != provider {
				t.Fatalf("OAuthModelAliasChannel() = %q, want %q", got, provider)
			}
		})
	}
}

func TestApplyOAuthModelAlias_SuffixPreservation(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: "claude-sonnet-4-5-20250514", Alias: "claude-sonnet-4-5"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{ID: "test-auth-id", Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "claude-sonnet-4-5(8192)")
	if resolvedModel != "claude-sonnet-4-5-20250514(8192)" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "claude-sonnet-4-5-20250514(8192)")
	}
}
