package util

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestOAuthProxyURLFallsBackToGlobalProxy(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"},
		ProxyPool: config.ProxyPoolConfig{
			Enabled: false,
			Proxies: []string{"http://pool.example.com:8080"},
		},
	}

	if got := OAuthProxyURL(cfg); got != cfg.ProxyURL {
		t.Fatalf("OAuthProxyURL() = %q, want global proxy %q", got, cfg.ProxyURL)
	}
}

func TestOAuthProxyURLRotatesValidPoolEntries(t *testing.T) {
	oauthProxyPoolCursor.Store(0)
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"},
		ProxyPool: config.ProxyPoolConfig{
			Enabled: true,
			Proxies: []string{
				" ",
				"http://pool-a.example.com:8080",
				"invalid-proxy",
				"socks5://pool-b.example.com:1080",
			},
		},
	}

	if got := OAuthProxyURL(cfg); got != "http://pool-a.example.com:8080" {
		t.Fatalf("first OAuthProxyURL() = %q", got)
	}
	if got := OAuthProxyURL(cfg); got != "socks5://pool-b.example.com:1080" {
		t.Fatalf("second OAuthProxyURL() = %q", got)
	}
	if got := OAuthProxyURL(cfg); got != "http://pool-a.example.com:8080" {
		t.Fatalf("third OAuthProxyURL() = %q", got)
	}
}

func TestOAuthProxyURLFallsBackWhenPoolHasNoValidProxy(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "direct"},
		ProxyPool: config.ProxyPoolConfig{
			Enabled: true,
			Proxies: []string{"", "invalid-proxy"},
		},
	}

	if got := OAuthProxyURL(cfg); got != "direct" {
		t.Fatalf("OAuthProxyURL() = %q, want direct fallback", got)
	}
}
