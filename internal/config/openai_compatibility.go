package config

import "strings"

const openAICompatibilityProvider = "openai-compatibility"

// ResolveOpenAICompatibility returns the first enabled provider matching the
// auth identity. Empty-name providers are matched by base URL.
func ResolveOpenAICompatibility(
	entries []OpenAICompatibility,
	providerKey, compatName, authProvider, baseURL string,
) *OpenAICompatibility {
	candidates := make([]string, 0, 3)
	if name := strings.TrimSpace(compatName); name != "" {
		candidates = append(candidates, name)
	}
	for _, value := range []string{providerKey, authProvider} {
		value = strings.TrimSpace(value)
		if value != "" && !strings.EqualFold(value, openAICompatibilityProvider) {
			candidates = append(candidates, value)
		}
	}

	baseURL = normalizeOpenAICompatibilityBaseURL(baseURL)
	var namedMatch *OpenAICompatibility
	for i := range entries {
		entry := &entries[i]
		if entry.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if strings.EqualFold(candidate, strings.TrimSpace(entry.Name)) {
				if baseURL != "" && normalizeOpenAICompatibilityBaseURL(entry.BaseURL) == baseURL {
					return entry
				}
				if namedMatch == nil {
					namedMatch = entry
				}
			}
		}
	}
	if namedMatch != nil {
		return namedMatch
	}
	if baseURL == "" {
		return nil
	}
	for i := range entries {
		entry := &entries[i]
		if !entry.Disabled && normalizeOpenAICompatibilityBaseURL(entry.BaseURL) == baseURL {
			return entry
		}
	}
	return nil
}

// ResolveOpenAICompatibilityAPIKey returns the configured API-key entry that
// belongs to a resolved OpenAI-compatible provider. API keys are compared
// case-insensitively after trimming so the same identity rules are used by
// configuration loading and request-time routing.
func ResolveOpenAICompatibilityAPIKey(
	provider *OpenAICompatibility,
	apiKey string,
) *OpenAICompatibilityAPIKey {
	if provider == nil {
		return nil
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	for i := range provider.APIKeyEntries {
		if strings.EqualFold(strings.TrimSpace(provider.APIKeyEntries[i].APIKey), apiKey) {
			return &provider.APIKeyEntries[i]
		}
	}
	return nil
}

// OpenAICompatibilityModelsForAPIKey returns the model rules visible to one
// API key. A nil or empty per-key Models field inherits provider-level rules.
func OpenAICompatibilityModelsForAPIKey(
	provider *OpenAICompatibility,
	apiKey string,
) []OpenAICompatibilityModel {
	if provider == nil {
		return nil
	}
	if entry := ResolveOpenAICompatibilityAPIKey(provider, apiKey); entry != nil && len(entry.Models) > 0 {
		return entry.Models
	}
	return provider.Models
}

func normalizeOpenAICompatibilityBaseURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}
