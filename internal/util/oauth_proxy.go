package util

import (
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

var oauthProxyPoolCursor atomic.Uint64

// OAuthProxyURL returns the proxy to use for a new OAuth login flow.
//
// When the proxy pool is enabled, login flows rotate through valid pool
// entries. The caller must resolve this once per flow and reuse the returned
// value for device authorization, polling, discovery, and token exchange so a
// single login never changes its upstream IP midway through authentication.
// If the pool is disabled or contains no valid entries, the global proxy URL
// is returned unchanged.
func OAuthProxyURL(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	fallback := strings.TrimSpace(cfg.ProxyURL)
	if !cfg.ProxyPool.Enabled || len(cfg.ProxyPool.Proxies) == 0 {
		return fallback
	}

	proxies := make([]string, 0, len(cfg.ProxyPool.Proxies))
	for _, raw := range cfg.ProxyPool.Proxies {
		candidate := strings.TrimSpace(raw)
		if candidate == "" {
			continue
		}
		setting, err := proxyutil.Parse(candidate)
		if err != nil || setting.Mode == proxyutil.ModeInherit || setting.Mode == proxyutil.ModeInvalid {
			continue
		}
		proxies = append(proxies, candidate)
	}
	if len(proxies) == 0 {
		return fallback
	}

	index := oauthProxyPoolCursor.Add(1) - 1
	return proxies[index%uint64(len(proxies))]
}
