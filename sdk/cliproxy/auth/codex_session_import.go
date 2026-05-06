package auth

import "strings"

// NormalizeImportedAuthMetadata converts supported OpenAI web session exports
// into the native Codex auth-file shape used by the proxy. It returns the
// normalized metadata plus a flag indicating whether a conversion occurred.
func NormalizeImportedAuthMetadata(metadata map[string]any) (map[string]any, bool) {
	if len(metadata) == 0 {
		return metadata, false
	}
	if strings.TrimSpace(metadataString(metadata, "type")) != "" {
		return metadata, false
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
