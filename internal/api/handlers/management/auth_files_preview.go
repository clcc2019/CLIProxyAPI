package management

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func authFilePreviewJSON(data []byte) ([]byte, error) {
	doc := make(map[string]any)
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if isCodexAuthFilePreviewSource(doc) {
		preview, err := json.MarshalIndent(buildCodexAuthFilePreview(doc), "", "  ")
		if err != nil {
			return nil, err
		}
		return append(preview, '\n'), nil
	}
	delete(doc, authFileRuntimeStateJSONKey)
	preview, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(preview, '\n'), nil
}

type codexAuthFilePreview struct {
	Type                           string   `json:"type"`
	AccountID                      string   `json:"account_id,omitempty"`
	ChatGPTAccountID               string   `json:"chatgpt_account_id,omitempty"`
	Email                          string   `json:"email,omitempty"`
	Name                           string   `json:"name,omitempty"`
	PlanType                       string   `json:"plan_type,omitempty"`
	ChatGPTPlanType                string   `json:"chatgpt_plan_type,omitempty"`
	IDToken                        string   `json:"id_token,omitempty"`
	IDTokenSynthetic               *bool    `json:"id_token_synthetic,omitempty"`
	AccessToken                    string   `json:"access_token,omitempty"`
	RefreshToken                   string   `json:"refresh_token"`
	SessionToken                   string   `json:"session_token,omitempty"`
	LastRefresh                    any      `json:"last_refresh,omitempty"`
	Expired                        any      `json:"expired,omitempty"`
	SubscriptionExpiresAt          any      `json:"subscription_expires_at,omitempty"`
	ChatGPTSubscriptionActiveStart any      `json:"chatgpt_subscription_active_start,omitempty"`
	ChatGPTSubscriptionActiveUntil any      `json:"chatgpt_subscription_active_until,omitempty"`
	Priority                       any      `json:"priority,omitempty"`
	Note                           string   `json:"note,omitempty"`
	UserAgent                      string   `json:"user_agent,omitempty"`
	Originator                     string   `json:"originator,omitempty"`
	BetaFeatures                   string   `json:"beta_features,omitempty"`
	InstallationID                 string   `json:"installation_id,omitempty"`
	IncludeTimingMetrics           *bool    `json:"include_timing_metrics,omitempty"`
	ExcludedModels                 []string `json:"excluded_models,omitempty"`
	DisableCooling                 *bool    `json:"disable_cooling,omitempty"`
	Websockets                     *bool    `json:"websockets,omitempty"`
	ServiceTierPassthrough         *bool    `json:"service_tier_passthrough,omitempty"`
	Disabled                       *bool    `json:"disabled,omitempty"`
}

func isCodexAuthFilePreviewSource(doc map[string]any) bool {
	if len(doc) == 0 {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(valueAsString(doc["type"])), "codex") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(valueAsString(doc["provider"])), "codex") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(valueAsString(doc["authProvider"])), "openai")
}

func buildCodexAuthFilePreview(doc map[string]any) codexAuthFilePreview {
	idToken := authFilePreviewMetadataString(doc, "id_token", "idToken")
	claims := parseCodexJWTClaims(idToken)

	accountID := authFilePreviewFirstNonEmptyString(
		authFilePreviewMetadataString(doc, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"),
		authFilePreviewNestedString(doc, "account", "id"),
		authFilePreviewNestedString(doc, "providerSpecificData", "chatgpt_account_id"),
		authFilePreviewNestedString(doc, "providerSpecificData", "chatgptAccountId"),
		authFilePreviewNestedString(doc, "credentials", "chatgpt_account_id"),
		codexClaimsAccountID(claims),
	)
	planType := authFilePreviewFirstNonEmptyString(
		authFilePreviewMetadataString(doc, "plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"),
		authFilePreviewNestedString(doc, "account", "plan_type"),
		authFilePreviewNestedString(doc, "account", "planType"),
		authFilePreviewNestedString(doc, "providerSpecificData", "chatgpt_plan_type"),
		authFilePreviewNestedString(doc, "providerSpecificData", "chatgptPlanType"),
		authFilePreviewNestedString(doc, "credentials", "plan_type"),
		codexClaimsPlanType(claims),
	)
	email := authFilePreviewFirstNonEmptyString(
		authFilePreviewMetadataString(doc, "email"),
		authFilePreviewNestedString(doc, "user", "email"),
		authFilePreviewNestedString(doc, "credentials", "email"),
		authFilePreviewNestedString(doc, "providerSpecificData", "email"),
		codexClaimsEmail(claims),
	)
	subscriptionExpiresAt := authFilePreviewSubscriptionExpiresAt(doc, claims)

	preview := codexAuthFilePreview{
		Type:                           "codex",
		AccountID:                      accountID,
		ChatGPTAccountID:               accountID,
		Email:                          email,
		Name:                           authFilePreviewFirstNonEmptyString(authFilePreviewMetadataString(doc, "name"), email),
		PlanType:                       planType,
		ChatGPTPlanType:                planType,
		IDToken:                        idToken,
		IDTokenSynthetic:               authFilePreviewBoolPtr(doc["id_token_synthetic"]),
		AccessToken:                    authFilePreviewMetadataString(doc, "access_token", "accessToken"),
		RefreshToken:                   authFilePreviewMetadataString(doc, "refresh_token", "refreshToken"),
		SessionToken:                   authFilePreviewMetadataString(doc, "session_token", "sessionToken"),
		LastRefresh:                    authFilePreviewFirstValue(doc, append([]string{}, lastRefreshKeys...)...),
		Expired:                        authFilePreviewFirstValue(doc, "expired", "expire", "expires_at", "expiresAt", "expiry", "expires"),
		SubscriptionExpiresAt:          subscriptionExpiresAt,
		ChatGPTSubscriptionActiveStart: authFilePreviewSubscriptionActiveStart(doc, claims, subscriptionExpiresAt),
		ChatGPTSubscriptionActiveUntil: authFilePreviewFirstValue(doc, "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil"),
		Priority:                       authFilePreviewFirstValue(doc, "priority"),
		Note:                           authFilePreviewMetadataString(doc, "note"),
		UserAgent:                      authFilePreviewClientProfileString(doc, "user_agent", "user-agent", "userAgent", "header:User-Agent"),
		Originator:                     authFilePreviewClientProfileString(doc, coreauth.AuthFileCodexOriginatorKey, coreauth.AuthFileCodexOriginatorHeader, "header:"+coreauth.AuthFileCodexOriginatorHeader),
		BetaFeatures:                   authFilePreviewClientProfileString(doc, coreauth.AuthFileCodexBetaFeaturesKey, "beta-features", "betaFeatures", "header:"+coreauth.AuthFileCodexBetaFeaturesHeader),
		InstallationID:                 authFilePreviewClientProfileString(doc, coreauth.AuthFileCodexInstallationIDKey, "installation-id", "installationId", "header:"+coreauth.AuthFileCodexInstallationIDHeader),
		IncludeTimingMetrics:           authFilePreviewClientProfileBool(doc, coreauth.AuthFileCodexIncludeTimingMetricsKey, "include-timing-metrics", "includeTimingMetrics", "header:"+coreauth.AuthFileCodexIncludeTimingMetricsHeader),
		ExcludedModels:                 extractExcludedModelsFromMetadata(doc),
		DisableCooling:                 authFilePreviewOptionalBool(doc, "disable_cooling", "disable-cooling", "disableCooling"),
		Websockets:                     authFilePreviewOptionalBool(doc, "websockets", "websocket"),
		ServiceTierPassthrough:         authFilePreviewOptionalBool(doc, coreauth.AuthFileServiceTierPassthroughKey, "service-tier-passthrough", "serviceTierPassthrough", "fast"),
	}
	if disabled := authFilePreviewBoolPtr(doc["disabled"]); disabled != nil && *disabled {
		preview.Disabled = disabled
	}
	return preview
}

func authFilePreviewMetadataString(metadata map[string]any, keys ...string) string {
	return codexAuthMetadataString(metadata, keys...)
}

func authFilePreviewClientProfileString(metadata map[string]any, keys ...string) string {
	if value := authFilePreviewMetadataString(metadata, keys...); value != "" {
		return value
	}
	headers := authFileMetadataHeaders(metadata)
	for _, key := range keys {
		headerName, ok := strings.CutPrefix(key, "header:")
		if !ok {
			continue
		}
		if value := authFileHeaderValue(headers, headerName); value != "" {
			return value
		}
	}
	return ""
}

func authFilePreviewClientProfileBool(metadata map[string]any, keys ...string) *bool {
	if parsed := authFilePreviewOptionalBool(metadata, keys...); parsed != nil {
		return parsed
	}
	headers := authFileMetadataHeaders(metadata)
	for _, key := range keys {
		headerName, ok := strings.CutPrefix(key, "header:")
		if !ok {
			continue
		}
		value := authFileHeaderValue(headers, headerName)
		if parsed, err := strconv.ParseBool(value); err == nil {
			return &parsed
		}
	}
	return nil
}

func authFilePreviewNestedString(metadata map[string]any, outerKey, innerKey string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[outerKey]
	if !ok || raw == nil {
		return ""
	}
	nested, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(valueAsString(nested[innerKey]))
}

func authFilePreviewFirstValue(metadata map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := metadata[key]; ok && !authFilePreviewEmptyValue(value) {
			return value
		}
	}
	for _, containerKey := range []string{"token", "tokens", "token_data", "tokenData", "credentials"} {
		container, ok := metadata[containerKey]
		if !ok || container == nil {
			continue
		}
		if nested, ok := container.(map[string]any); ok {
			if value := authFilePreviewFirstValue(nested, keys...); !authFilePreviewEmptyValue(value) {
				return value
			}
		}
	}
	return nil
}

func authFilePreviewSubscriptionExpiresAt(metadata map[string]any, claims *codex.JWTClaims) any {
	if value, ok := codexSubscriptionUntilValue(metadata); ok {
		return value
	}
	if claims == nil {
		return nil
	}
	if value, ok := normalizeCodexSubscriptionUntilValue(claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil); ok {
		return value
	}
	return nil
}

func authFilePreviewSubscriptionActiveStart(metadata map[string]any, claims *codex.JWTClaims, subscriptionExpiresAt any) any {
	if value := authFilePreviewFirstValue(metadata, "chatgpt_subscription_active_start", "chatgptSubscriptionActiveStart", "subscription_active_start", "subscriptionActiveStart", "current_period_start", "currentPeriodStart", "period_start", "periodStart", "started_at", "startedAt"); !authFilePreviewEmptyValue(value) {
		return value
	}
	if claims != nil {
		if value, ok := normalizeCodexSubscriptionUntilValue(claims.CodexAuthInfo.ChatgptSubscriptionActiveStart); ok {
			return value
		}
	}
	if value, ok := deriveCodexSubscriptionActiveStartFromUntilValue(subscriptionExpiresAt); ok {
		return value
	}
	return nil
}

func authFilePreviewEmptyValue(value any) bool {
	if value == nil {
		return true
	}
	if str, ok := value.(string); ok {
		return strings.TrimSpace(str) == ""
	}
	return false
}

func authFilePreviewBoolPtr(value any) *bool {
	switch v := value.(type) {
	case bool:
		return &v
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		if err == nil {
			return &parsed
		}
	}
	return nil
}

func authFilePreviewOptionalBool(metadata map[string]any, keys ...string) *bool {
	for _, key := range keys {
		value, ok := metadata[key]
		if !ok {
			continue
		}
		if parsed := authFilePreviewBoolPtr(value); parsed != nil {
			return parsed
		}
	}
	return nil
}

func authFilePreviewFirstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func codexClaimsEmail(claims *codex.JWTClaims) string {
	if claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.GetUserEmail())
}

func codexClaimsAccountID(claims *codex.JWTClaims) string {
	if claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.GetAccountID())
}

func codexClaimsPlanType(claims *codex.JWTClaims) string {
	if claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
}
