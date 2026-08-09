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

func normalizeOpenAICompatibilityBaseURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}
