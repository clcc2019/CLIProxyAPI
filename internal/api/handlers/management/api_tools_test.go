package management

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestAPICallTransportInvalidAuthFallsBackToGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "bad-value"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportAPIKeyAuthFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
			ClaudeKey: []config.ClaudeKey{{
				APIKey:   "claude-key",
				ProxyURL: "http://claude-proxy.example.com:8080",
			}},
			CodexKey: []config.CodexKey{{
				APIKey:   "codex-key",
				ProxyURL: "http://codex-proxy.example.com:8080",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "bohe",
				BaseURL: "https://bohe.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey:   "compat-key",
					ProxyURL: "http://compat-proxy.example.com:8080",
				}},
			}},
		},
	}

	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantProxy string
	}{
		{
			name: "claude",
			auth: &coreauth.Auth{
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "claude-key"},
			},
			wantProxy: "http://claude-proxy.example.com:8080",
		},
		{
			name: "claude mixed case",
			auth: &coreauth.Auth{
				Provider:   " Claude ",
				Attributes: map[string]string{"api_key": "claude-key"},
			},
			wantProxy: "http://claude-proxy.example.com:8080",
		},
		{
			name: "codex",
			auth: &coreauth.Auth{
				Provider:   "codex",
				Attributes: map[string]string{"api_key": "codex-key"},
			},
			wantProxy: "http://codex-proxy.example.com:8080",
		},
		{
			name: "codex mixed case",
			auth: &coreauth.Auth{
				Provider:   " CODEX ",
				Attributes: map[string]string{"api_key": "codex-key"},
			},
			wantProxy: "http://codex-proxy.example.com:8080",
		},
		{
			name: "openai-compatibility",
			auth: &coreauth.Auth{
				Provider: "bohe",
				Attributes: map[string]string{
					"api_key":      "compat-key",
					"compat_name":  "bohe",
					"provider_key": "bohe",
				},
			},
			wantProxy: "http://compat-proxy.example.com:8080",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := h.apiCallTransport(tc.auth)
			httpTransport, ok := transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", transport)
			}

			req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest returned error: %v", errRequest)
			}

			proxyURL, errProxy := httpTransport.Proxy(req)
			if errProxy != nil {
				t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
			}
			if proxyURL == nil || proxyURL.String() != tc.wantProxy {
				t.Fatalf("proxy URL = %v, want %s", proxyURL, tc.wantProxy)
			}
		})
	}
}

func BenchmarkProxyURLFromAPIKeyConfig(b *testing.B) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:   "claude-key",
			ProxyURL: "http://claude-proxy.example.com:8080",
		}},
	}
	auth := &coreauth.Auth{
		Provider:   " Claude ",
		Attributes: map[string]string{"api_key": "claude-key"},
	}

	for b.Loop() {
		if got := proxyURLFromAPIKeyConfig(cfg, auth); got != "http://claude-proxy.example.com:8080" {
			b.Fatalf("proxyURLFromAPIKeyConfig() = %q", got)
		}
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	claudeAuth := &coreauth.Auth{
		ID:       "claude:apikey:123",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "shared-key",
		},
	}
	compatAuth := &coreauth.Auth{
		ID:       "openai-compatibility:bohe:456",
		Provider: "bohe",
		Label:    "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
		},
	}

	if _, errRegister := manager.Register(context.Background(), claudeAuth); errRegister != nil {
		t.Fatalf("register claude auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), compatAuth); errRegister != nil {
		t.Fatalf("register compat auth: %v", errRegister)
	}

	claudeIndex := claudeAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	if claudeIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", claudeIndex)
	}

	h := &Handler{authManager: manager}

	gotClaude := h.authByIndex(claudeIndex)
	if gotClaude == nil {
		t.Fatal("expected claude auth by index")
	}
	if gotClaude.ID != claudeAuth.ID {
		t.Fatalf("authByIndex(claude) returned %q, want %q", gotClaude.ID, claudeAuth.ID)
	}

	gotCompat := h.authByIndex(compatIndex)
	if gotCompat == nil {
		t.Fatal("expected compat auth by index")
	}
	if gotCompat.ID != compatAuth.ID {
		t.Fatalf("authByIndex(compat) returned %q, want %q", gotCompat.ID, compatAuth.ID)
	}
}

func TestApplyAPICallDefaultHeadersUsesCodexConfigUserAgent(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			CodexHeaderDefaults: config.CodexHeaderDefaults{
				UserAgent: "codex-config-ua",
			},
		},
	}
	headers := map[string]string{}

	h.applyAPICallDefaultHeaders(headers, nil, "codex")

	if got := headers["User-Agent"]; got != "codex-config-ua" {
		t.Fatalf("User-Agent = %q, want %q", got, "codex-config-ua")
	}
}

func TestApplyAPICallDefaultHeadersUsesCodexConfigUserAgentBeforeAuth(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			CodexHeaderDefaults: config.CodexHeaderDefaults{
				UserAgent: "codex-config-ua",
			},
		},
	}
	headers := map[string]string{}
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"user_agent": "auth-file-ua",
		},
	}

	h.applyAPICallDefaultHeaders(headers, auth, "")

	if got := headers["User-Agent"]; got != "codex-config-ua" {
		t.Fatalf("User-Agent = %q, want %q", got, "codex-config-ua")
	}
}

func TestApplyAPICallDefaultHeadersUsesClaudeConfigUserAgent(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
				UserAgent: "claude-config-ua",
			},
		},
	}
	headers := map[string]string{}

	h.applyAPICallDefaultHeaders(headers, nil, "claude")

	if got := headers["User-Agent"]; got != "claude-config-ua" {
		t.Fatalf("User-Agent = %q, want %q", got, "claude-config-ua")
	}
}

func TestApplyAPICallDefaultHeadersKeepsExplicitUserAgent(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			CodexHeaderDefaults: config.CodexHeaderDefaults{
				UserAgent: "codex-config-ua",
			},
		},
	}
	headers := map[string]string{
		"user-agent": "explicit-ua",
	}

	h.applyAPICallDefaultHeaders(headers, nil, "codex")

	if got := headers["user-agent"]; got != "explicit-ua" {
		t.Fatalf("user-agent = %q, want %q", got, "explicit-ua")
	}
	if got := headers["User-Agent"]; got != "" {
		t.Fatalf("User-Agent = %q, want empty", got)
	}
}
