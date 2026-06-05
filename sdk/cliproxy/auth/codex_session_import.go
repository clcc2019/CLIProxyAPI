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
	if subscriptionExpiresAt := importedOpenAISubscriptionExpiresAt(metadata); subscriptionExpiresAt != "" {
		normalized["subscription_expires_at"] = subscriptionExpiresAt
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

func importedOpenAISubscriptionExpiresAt(metadata map[string]any) string {
	if value := firstMetadataString(metadata, "subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil"); value != "" {
		return value
	}
	for _, candidate := range []struct {
		container string
		keys      []string
	}{
		{container: "account", keys: []string{"subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil", "subscription_active_until", "subscriptionActiveUntil"}},
		{container: "entitlement", keys: []string{"subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil", "expires_at", "expiresAt", "current_period_end", "currentPeriodEnd", "period_end", "periodEnd"}},
		{container: "subscription", keys: []string{"subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil", "expires_at", "expiresAt", "current_period_end", "currentPeriodEnd", "period_end", "periodEnd"}},
		{container: "providerSpecificData", keys: []string{"subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil"}},
	} {
		for _, key := range candidate.keys {
			if value := strings.TrimSpace(nestedMetadataString(metadata, candidate.container, key)); value != "" {
				return value
			}
		}
	}
	return ""
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
