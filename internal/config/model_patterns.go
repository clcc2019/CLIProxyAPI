package config

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

// NormalizeModelPatternList trims, lowercases, and deduplicates model patterns.
// It preserves the order of first occurrences and drops empty entries.
func NormalizeModelPatternList(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CanonicalModelPatternKey strips thinking suffixes and provider path prefixes
// so model restrictions can be written against the client-visible model id.
func CanonicalModelPatternKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	name := strings.TrimSpace(parsed.ModelName)
	if name == "" {
		name = model
	}
	name = strings.TrimPrefix(name, "models/")
	return strings.ToLower(strings.TrimSpace(name))
}

// MatchModelPattern performs case-insensitive wildcard matching where '*'
// matches any substring.
func MatchModelPattern(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = CanonicalModelPatternKey(value)
	if pattern == "" || value == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	return true
}

// IsModelAllowed returns whether model is permitted by the provided allow/deny
// pattern lists. Exclusions always win over allow-list matches.
func IsModelAllowed(model string, allowed, excluded []string) bool {
	modelKey := CanonicalModelPatternKey(model)
	if modelKey == "" {
		return true
	}
	for _, pattern := range excluded {
		if MatchModelPattern(pattern, modelKey) {
			return false
		}
	}
	if len(allowed) == 0 {
		return true
	}
	for _, pattern := range allowed {
		if MatchModelPattern(pattern, modelKey) {
			return true
		}
	}
	return false
}
