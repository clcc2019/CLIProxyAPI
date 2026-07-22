package auth

import (
	"net/url"
	"strings"
)

const (
	CodexAgentIdentityProductionAuthAPIBaseURL = "https://auth.openai.com/api/accounts"
	CodexAgentIdentityStagingAuthAPIBaseURL    = "https://auth.api.openai.org/api/accounts"
)

// CodexAgentIdentityAuthAPIBaseURL resolves the trusted Agent Identity
// control-plane endpoint for a Codex credential. Custom inference base URLs do
// not redirect private-key registration proofs; only OpenAI's known staging
// environment selects the staging control plane.
func CodexAgentIdentityAuthAPIBaseURL(auth *Auth) string {
	baseURL := ""
	if auth != nil {
		if auth.Attributes != nil {
			for _, key := range []string{"base_url", "base-url", "baseUrl"} {
				if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
					baseURL = value
					break
				}
			}
		}
		if baseURL == "" {
			baseURL = codexAuthMetadataString(auth.Metadata, "base_url", "base-url", "baseUrl")
		}
	}

	parsed, err := url.Parse(baseURL)
	if err == nil {
		switch strings.ToLower(strings.TrimSpace(parsed.Hostname())) {
		case "chatgpt-staging.com", "auth.api.openai.org":
			return CodexAgentIdentityStagingAuthAPIBaseURL
		}
	}
	return CodexAgentIdentityProductionAuthAPIBaseURL
}
