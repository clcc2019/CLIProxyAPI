package cliproxy

import (
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func configuredCredentialSelector(strategy string, sessionAffinity bool, sessionAffinityTTL string) coreauth.Selector {
	var selector coreauth.Selector
	if !sessionAffinity && isFillFirstStrategy(strategy) {
		selector = &coreauth.FillFirstSelector{}
	} else {
		selector = &coreauth.RoundRobinSelector{}
	}

	if !sessionAffinity {
		return selector
	}

	ttl := time.Hour
	if ttlStr := strings.TrimSpace(sessionAffinityTTL); ttlStr != "" {
		if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
			ttl = parsed
		}
	}
	return coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: selector,
		TTL:      ttl,
	})
}

func normalizeRoutingStrategy(strategy string) string {
	if isFillFirstStrategy(strategy) {
		return "fill-first"
	}
	return "round-robin"
}

func effectiveRoutingStrategy(strategy string, sessionAffinity bool) string {
	if sessionAffinity {
		return "round-robin"
	}
	return normalizeRoutingStrategy(strategy)
}

func isFillFirstStrategy(strategy string) bool {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "fill-first", "fillfirst", "ff":
		return true
	default:
		return false
	}
}
