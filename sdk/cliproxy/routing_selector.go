package cliproxy

import (
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func configuredCredentialSelector(strategy string, sessionAffinity bool, sessionAffinityTTL string) coreauth.Selector {
	var selector coreauth.Selector
	switch normalizeRoutingStrategy(strategy) {
	case "weighted-round-robin":
		selector = &coreauth.WeightedRoundRobinSelector{}
	case "fill-first":
		if !sessionAffinity {
			selector = &coreauth.FillFirstSelector{}
		} else {
			selector = &coreauth.RoundRobinSelector{}
		}
	default:
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
	if isWeightedRoundRobinStrategy(strategy) {
		return "weighted-round-robin"
	}
	if isFillFirstStrategy(strategy) {
		return "fill-first"
	}
	return "round-robin"
}

func effectiveRoutingStrategy(strategy string, sessionAffinity bool) string {
	normalized := normalizeRoutingStrategy(strategy)
	if sessionAffinity && normalized != "weighted-round-robin" {
		return "round-robin"
	}
	return normalized
}

func isWeightedRoundRobinStrategy(strategy string) bool {
	strategy = strings.TrimSpace(strategy)
	switch {
	case strings.EqualFold(strategy, "weighted-round-robin"):
		return true
	case strings.EqualFold(strategy, "weightedroundrobin"):
		return true
	case strings.EqualFold(strategy, "weighted"):
		return true
	case strings.EqualFold(strategy, "wrr"):
		return true
	default:
		return false
	}
}

func isFillFirstStrategy(strategy string) bool {
	strategy = strings.TrimSpace(strategy)
	switch {
	case strings.EqualFold(strategy, "fill-first"):
		return true
	case strings.EqualFold(strategy, "fillfirst"):
		return true
	case strings.EqualFold(strategy, "ff"):
		return true
	default:
		return false
	}
}
