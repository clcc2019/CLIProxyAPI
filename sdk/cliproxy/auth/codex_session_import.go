package auth

import "strings"

// NormalizeImportedAuthMetadata converts supported external auth exports into
// native auth-file metadata used by the proxy and removes request-scoped Codex
// features. It returns the normalized metadata plus a flag indicating whether
// any conversion or cleanup occurred.
func NormalizeImportedAuthMetadata(metadata map[string]any) (map[string]any, bool) {
	normalized, changed := normalizeImportedAuthMetadata(metadata)
	sanitized, sanitizedChanged := SanitizeCodexAuthMetadata(normalized)
	return sanitized, changed || sanitizedChanged
}

func normalizeImportedAuthMetadata(metadata map[string]any) (map[string]any, bool) {
	if len(metadata) == 0 {
		return metadata, false
	}
	if unwrapped, ok := unwrapSingleSub2APIAuthExport(metadata); ok {
		if normalized, changed := NormalizeImportedAuthMetadata(unwrapped); changed {
			return normalized, true
		}
		return unwrapped, true
	}
	if normalized, ok := normalizeImportedCodexAgentIdentity(metadata); ok {
		return normalized, true
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

func unwrapSingleSub2APIAuthExport(metadata map[string]any) (map[string]any, bool) {
	if !strings.EqualFold(strings.TrimSpace(metadataString(metadata, "type")), "sub2api-data") {
		return nil, false
	}
	accounts, ok := metadata["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		return nil, false
	}
	account, ok := accounts[0].(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(metadataString(account, "platform")), "openai") {
		return nil, false
	}
	credentials, ok := account["credentials"].(map[string]any)
	if !ok || len(credentials) == 0 {
		return nil, false
	}
	authMode := strings.TrimSpace(firstMetadataString(credentials, "auth_mode", "authMode", "auth_kind", "authKind"))
	if !strings.EqualFold(authMode, "agentIdentity") && !strings.EqualFold(authMode, "agent_identity") {
		return nil, false
	}

	unwrapped := make(map[string]any, len(credentials)+8)
	for key, value := range credentials {
		unwrapped[key] = value
	}
	unwrapped["type"] = "codex"
	if name := strings.TrimSpace(metadataString(account, "name")); name != "" {
		if strings.TrimSpace(metadataString(unwrapped, "email")) == "" && strings.Contains(name, "@") {
			unwrapped["email"] = name
		}
		unwrapped["label"] = name
	}
	for _, key := range []string{"priority", "disabled", "proxy_url", "proxyUrl", "note"} {
		if value, exists := account[key]; exists && value != nil {
			unwrapped[key] = value
		}
	}
	if extra, okExtra := account["extra"].(map[string]any); okExtra {
		for _, key := range []string{"last_refresh", "lastRefresh", "email", "account_id", "chatgpt_account_id", "workspace_id"} {
			if _, exists := unwrapped[key]; exists {
				continue
			}
			if value, exists := extra[key]; exists && value != nil {
				unwrapped[key] = value
			}
		}
	}
	return unwrapped, true
}

func normalizeImportedCodexAgentIdentity(metadata map[string]any) (map[string]any, bool) {
	sources := []map[string]any{metadata}
	for _, key := range []string{"agent_identity", "agentIdentity", "credentials"} {
		if nested, ok := metadata[key].(map[string]any); ok && len(nested) > 0 {
			sources = append(sources, nested)
		}
	}
	firstString := func(keys ...string) string {
		for _, source := range sources {
			for _, key := range keys {
				if value := strings.TrimSpace(metadataString(source, key)); value != "" {
					return value
				}
			}
		}
		return ""
	}
	authMode := firstString("auth_kind", "authKind", "auth_mode", "authMode")
	runtimeID := firstString("agent_runtime_id", "agentRuntimeId", "agentRuntimeID")
	privateKey := firstString("agent_private_key", "agentPrivateKey")
	isAgentIdentity := strings.EqualFold(authMode, "agentIdentity") || strings.EqualFold(authMode, "agent_identity")
	// A non-Agent explicit mode is authoritative. This allows native auth
	// files to retain reusable Agent Identity material while actively using
	// their access token.
	if authMode != "" && !isAgentIdentity {
		return metadata, false
	}
	if !isAgentIdentity && (runtimeID == "" || privateKey == "") {
		return metadata, false
	}

	normalized := make(map[string]any, len(metadata)+8)
	for key, value := range metadata {
		normalized[key] = value
	}
	normalized["type"] = "codex"
	normalized["auth_kind"] = "agent_identity"
	copyNonEmptyMetadataString(normalized, "agent_runtime_id", runtimeID)
	copyNonEmptyMetadataString(normalized, "agent_private_key", privateKey)
	copyNonEmptyMetadataString(normalized, "task_id", firstString("task_id", "taskId"))
	copyNonEmptyMetadataString(normalized, "account_id", firstString("account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"))
	copyNonEmptyMetadataString(normalized, "chatgpt_user_id", firstString("chatgpt_user_id", "chatgptUserId", "chatgptUserID"))
	normalizeImportedCodexClientFeatures(normalized, sources)
	// Agent assertions are tied to the client that created the credential. Keep
	// the auth-file client profile authoritative so requests from different
	// downstream clients cannot change that fingerprint. An explicit false is
	// preserved as an opt-out for operators who intentionally want that.
	if _, explicitlyConfigured := normalized[AuthFileCodexClientProfilePinnedKey]; !explicitlyConfigured {
		normalized[AuthFileCodexClientProfilePinnedKey] = true
	}

	for _, source := range sources {
		for _, key := range []string{"chatgpt_account_is_fedramp", "chatgptAccountIsFedramp", "fedramp", "openai_fedramp"} {
			if value, exists := source[key]; exists && value != nil {
				normalized["fedramp"] = value
				break
			}
		}
		if _, exists := normalized["fedramp"]; exists {
			break
		}
	}

	for _, key := range []string{
		"auth_mode", "authMode", "authKind", "agentRuntimeId", "agentRuntimeID", "agentPrivateKey",
		"taskId", "accountId", "chatgptAccountId", "chatgptUserId", "chatgptUserID",
		"chatgpt_account_is_fedramp", "chatgptAccountIsFedramp", "agent_identity", "agentIdentity",
	} {
		delete(normalized, key)
	}
	return normalized, true
}

// normalizeImportedCodexClientFeatures promotes a client profile nested under
// an external agent_identity/agentIdentity object. Earlier imports removed
// that wrapper after extracting the signing material, inadvertently dropping
// the client fingerprint that must stay paired with an agent identity.
func normalizeImportedCodexClientFeatures(normalized map[string]any, sources []map[string]any) {
	if len(normalized) == 0 || hasImportedCodexClientFeatureObject(normalized) {
		return
	}
	for _, source := range sources {
		for _, key := range authFileClientProfileObjectKeys {
			profile, ok := source[key].(map[string]any)
			if !ok || len(profile) == 0 {
				continue
			}
			normalized["client_features"] = cloneImportedMetadataMap(profile)
			return
		}
	}
}

func hasImportedCodexClientFeatureObject(metadata map[string]any) bool {
	for _, key := range authFileClientProfileObjectKeys {
		profile, ok := metadata[key].(map[string]any)
		if ok && len(profile) > 0 {
			return true
		}
	}
	return false
}

func cloneImportedMetadataMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
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
