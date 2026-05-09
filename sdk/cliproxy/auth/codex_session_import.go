package auth

import "strings"

// NormalizeImportedAuthMetadata converts supported external auth exports into
// native auth-file metadata used by the proxy. It returns the normalized
// metadata plus a flag indicating whether a conversion occurred.
func NormalizeImportedAuthMetadata(metadata map[string]any) (map[string]any, bool) {
	if len(metadata) == 0 {
		return metadata, false
	}
	if strings.TrimSpace(metadataString(metadata, "type")) != "" {
		return metadata, false
	}
	if normalized, changed := normalizeImportedKiroToken(metadata); changed {
		return normalized, true
	}
	if !strings.EqualFold(strings.TrimSpace(metadataString(metadata, "authProvider")), "openai") {
		return metadata, false
	}

	accessToken := strings.TrimSpace(metadataString(metadata, "accessToken"))
	email := strings.TrimSpace(nestedMetadataString(metadata, "user", "email"))
	accountID := strings.TrimSpace(nestedMetadataString(metadata, "account", "id"))
	planType := strings.TrimSpace(nestedMetadataString(metadata, "account", "planType"))

	if accessToken == "" || email == "" || accountID == "" {
		return metadata, false
	}

	normalized := map[string]any{
		"type":         "codex",
		"access_token": accessToken,
		"email":        email,
		"account_id":   accountID,
	}
	if planType != "" {
		normalized["plan_type"] = planType
	}

	for _, key := range []string{
		"user_agent",
		"user-agent",
		"proxy_url",
		"prefix",
		"disabled",
		"priority",
		"note",
		"excluded_models",
		"headers",
		"websockets",
		"websocket",
		"base_url",
		"originator",
	} {
		if value, ok := metadata[key]; ok && value != nil {
			normalized[key] = value
		}
	}

	return normalized, true
}

func normalizeImportedKiroToken(metadata map[string]any) (map[string]any, bool) {
	accessToken := strings.TrimSpace(firstMetadataString(metadata, "accessToken", "access_token"))
	refreshToken := strings.TrimSpace(firstMetadataString(metadata, "refreshToken", "refresh_token"))
	if accessToken == "" || refreshToken == "" {
		return metadata, false
	}

	profileArn := strings.TrimSpace(firstMetadataString(metadata, "profileArn", "profile_arn"))
	authMethod := strings.ToLower(strings.TrimSpace(firstMetadataString(metadata, "authMethod", "auth_method")))
	provider := strings.ToLower(strings.TrimSpace(metadataString(metadata, "provider")))
	clientID := strings.TrimSpace(firstMetadataString(metadata, "clientId", "client_id"))
	clientSecret := strings.TrimSpace(firstMetadataString(metadata, "clientSecret", "client_secret"))
	clientIDHash := strings.TrimSpace(firstMetadataString(metadata, "clientIdHash", "client_id_hash"))
	startURL := strings.TrimSpace(firstMetadataString(metadata, "startUrl", "start_url"))

	if profileArn == "" && authMethod == "" && clientIDHash == "" && startURL == "" && !isKnownKiroRawProvider(provider) {
		return metadata, false
	}

	normalized := make(map[string]any, len(metadata)+10)
	for key, value := range metadata {
		normalized[key] = value
	}
	normalized["type"] = "kiro"
	normalized["access_token"] = accessToken
	normalized["refresh_token"] = refreshToken
	copyNonEmptyMetadataString(normalized, "profile_arn", profileArn)
	copyNonEmptyMetadataString(normalized, "expires_at", firstMetadataString(metadata, "expiresAt", "expires_at"))
	copyNonEmptyMetadataString(normalized, "client_id", clientID)
	copyNonEmptyMetadataString(normalized, "client_secret", clientSecret)
	copyNonEmptyMetadataString(normalized, "client_id_hash", clientIDHash)
	copyNonEmptyMetadataString(normalized, "email", metadataString(metadata, "email"))
	copyNonEmptyMetadataString(normalized, "start_url", startURL)
	copyNonEmptyMetadataString(normalized, "region", metadataString(metadata, "region"))

	if authMethod == "" {
		switch {
		case isKiroSocialProvider(provider):
			authMethod = "kiro-cli-social"
		case clientID != "" && clientSecret != "":
			authMethod = "builder-id"
		}
	}
	copyNonEmptyMetadataString(normalized, "auth_method", authMethod)
	return normalized, true
}

func isKnownKiroRawProvider(provider string) bool {
	return isKiroSocialProvider(provider) || provider == "aws" || provider == "builder-id" || provider == "idc" || provider == "kiro"
}

func isKiroSocialProvider(provider string) bool {
	switch provider {
	case "google", "github", "gitlab", "kiro-cli", "kiro-social", "social":
		return true
	default:
		return false
	}
}

func firstMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := metadataString(metadata, key); strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func copyNonEmptyMetadataString(metadata map[string]any, key, value string) {
	if strings.TrimSpace(value) != "" {
		metadata[key] = strings.TrimSpace(value)
	}
}

func nestedMetadataString(metadata map[string]any, outerKey, innerKey string) string {
	if len(metadata) == 0 {
		return ""
	}
	rawOuter, ok := metadata[outerKey]
	if !ok || rawOuter == nil {
		return ""
	}
	outer, ok := rawOuter.(map[string]any)
	if !ok {
		return ""
	}
	return metadataString(outer, innerKey)
}
