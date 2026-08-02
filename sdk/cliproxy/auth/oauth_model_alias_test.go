package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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

func TestResolveOAuthUpstreamModel_ConfiguredEffortAliases(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"codex": {
			{
				Name:  "gpt-5.6-terra",
				Alias: "gpt-5.5",
				ReasoningEffort: map[string]string{
					"default": "high",
					"low":     "medium",
					"medium":  "high",
					"high":    "max",
					"xhigh":   "max",
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)
	auth := createAuthForChannel("codex")

	for input, want := range map[string]string{
		"gpt-5.5":         "gpt-5.6-terra",
		"gpt-5.5(low)":    "gpt-5.6-terra",
		"gpt-5.5(medium)": "gpt-5.6-terra",
		"gpt-5.5(high)":   "gpt-5.6-terra",
		"gpt-5.5(xhigh)":  "gpt-5.6-terra",
		"gpt-5.5(none)":   "gpt-5.6-terra(none)",
	} {
		if got := mgr.resolveOAuthUpstreamModel(auth, input); got != want {
			t.Errorf("resolveOAuthUpstreamModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWithOAuthModelAliasReasoningEffort(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"codex": {{
			Name:  "gpt-5.6-terra",
			Alias: "gpt-5.5",
			ReasoningEffort: map[string]string{
				"default": "high",
				"low":     "medium",
				"high":    "max",
			},
		}},
	})
	auth := createAuthForChannel("codex")
	auth.Prefix = "team"

	tests := []struct {
		name   string
		effort string
		want   string
	}{
		{name: "default when client omits effort", want: "high"},
		{name: "configured client effort", effort: "low", want: "medium"},
		{name: "another configured client effort", effort: "high", want: "max"},
		{name: "unconfigured client effort is untouched", effort: "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := cliproxyexecutor.Request{
				Model:    "gpt-5.6-terra",
				Metadata: map[string]any{"preserved": true},
			}
			opts := cliproxyexecutor.Options{}
			if tt.effort != "" {
				opts.Metadata = map[string]any{cliproxyexecutor.ReasoningEffortMetadataKey: tt.effort}
			}

			got := mgr.withOAuthModelAliasReasoningEffort(req, auth, "team/gpt-5.5", opts)
			if got.Model != req.Model {
				t.Fatalf("model = %q, want %q", got.Model, req.Model)
			}
			if got.Metadata["preserved"] != true {
				t.Fatalf("request metadata was not preserved: %#v", got.Metadata)
			}
			if actual, _ := got.Metadata[cliproxyexecutor.UpstreamReasoningEffortOverrideMetadataKey].(string); actual != tt.want {
				t.Fatalf("upstream override = %q, want %q; metadata=%#v", actual, tt.want, got.Metadata)
			}
			if _, exists := req.Metadata[cliproxyexecutor.UpstreamReasoningEffortOverrideMetadataKey]; exists {
				t.Fatalf("input request metadata was mutated: %#v", req.Metadata)
			}
		})
	}
}

func TestOAuthModelAlias_EffortOnlyRule(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"codex": {{
			Name:            "gpt-5.6-sol",
			Alias:           "gpt-5.6-sol",
			ReasoningEffort: map[string]string{"max": "xhigh"},
		}},
	})
	auth := createAuthForChannel("codex")

	const requestedModel = "gpt-5.6-sol(max)"
	if got := mgr.applyOAuthModelAlias(auth, requestedModel); got != "gpt-5.6-sol" {
		t.Fatalf("applyOAuthModelAlias() = %q, want clean upstream model gpt-5.6-sol", got)
	}

	req := cliproxyexecutor.Request{
		Model:   requestedModel,
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	got := mgr.withOAuthModelAliasReasoningEffort(
		req,
		auth,
		requestedModel,
		cliproxyexecutor.Options{},
	)
	if actual, _ := got.Metadata[cliproxyexecutor.UpstreamReasoningEffortOverrideMetadataKey].(string); actual != "xhigh" {
		t.Fatalf("upstream override = %q, want xhigh; metadata=%#v", actual, got.Metadata)
	}
}

func TestOAuthModelAlias_EffortOnlyRuleSuppressesBuiltinRename(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"codex": {{
			Name:            "gpt-5.6",
			Alias:           "gpt-5.6",
			ReasoningEffort: map[string]string{"max": "xhigh"},
		}},
	})
	auth := createAuthForChannel("codex")

	if got := mgr.applyOAuthModelAlias(auth, "gpt-5.6(max)"); got != "gpt-5.6" {
		t.Fatalf("applyOAuthModelAlias() = %q, want effort-only rule to suppress built-in rename", got)
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

func TestApplyOAuthModelAlias_BuiltinCodexGPT56Alias(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})

	auth := &Auth{ID: "codex-oauth", Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "gpt-5.6(max)")
	if resolvedModel != "gpt-5.6-sol(max)" {
		t.Fatalf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "gpt-5.6-sol(max)")
	}
	if resolvedModel = mgr.applyOAuthModelAlias(auth, "gpt-5.6(ultra)"); resolvedModel != "gpt-5.6-sol(ultra)" {
		t.Fatalf("applyOAuthModelAlias() ultra model = %q, want %q", resolvedModel, "gpt-5.6-sol(ultra)")
	}
}

func TestApplyOAuthModelAlias_BuiltinCodexGPT56AliasSkipsAPIKey(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})

	auth := &Auth{ID: "codex-key", Provider: "codex", Attributes: map[string]string{"auth_kind": "apikey"}}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "gpt-5.6")
	if resolvedModel != "gpt-5.6" {
		t.Fatalf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "gpt-5.6")
	}
}
