package management

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func authFileUserAgent(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if ua := strings.TrimSpace(auth.Attributes["header:User-Agent"]); ua != "" {
			return ua
		}
		if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
			return ua
		}
		if ua := strings.TrimSpace(auth.Attributes["user-agent"]); ua != "" {
			return ua
		}
	}
	if auth.Metadata == nil {
		return ""
	}
	if raw, ok := auth.Metadata["user_agent"].(string); ok {
		if ua := strings.TrimSpace(raw); ua != "" {
			return ua
		}
	}
	if raw, ok := auth.Metadata["user-agent"].(string); ok {
		if ua := strings.TrimSpace(raw); ua != "" {
			return ua
		}
	}
	if ua := authFileHeaderValue(authFileMetadataHeaders(auth.Metadata), "User-Agent"); ua != "" {
		return ua
	}
	return ""
}

func authFileWebsockets(auth *coreauth.Auth) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			if value, err := strconv.ParseBool(raw); err == nil {
				return value, true
			}
		}
	}
	if auth.Metadata == nil {
		return false, false
	}
	if raw, ok := auth.Metadata["websockets"]; ok {
		switch value := raw.(type) {
		case bool:
			return value, true
		case string:
			if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				return parsed, true
			}
		}
	}
	if raw, ok := auth.Metadata["websocket"]; ok {
		switch value := raw.(type) {
		case bool:
			return value, true
		case string:
			if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				return parsed, true
			}
		}
	}
	return false, false
}

func authFileClientProfile(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}
	profile := gin.H{}
	if auth.Metadata != nil {
		if pinned, ok := auth.Metadata["codex_client_profile_pinned"].(bool); ok {
			profile["pinned"] = pinned
		}
		if originator, ok := auth.Metadata["originator"].(string); ok {
			if trimmed := strings.TrimSpace(originator); trimmed != "" {
				profile["originator"] = trimmed
			}
		}
		if headers := authFileMetadataHeaders(auth.Metadata); len(headers) > 0 {
			profile["headers"] = headers
		}
	}
	if originator := authFileClientProfileString(auth, coreauth.AuthFileCodexOriginatorKey, coreauth.AuthFileCodexOriginatorHeader, "header:"+coreauth.AuthFileCodexOriginatorHeader); originator != "" {
		profile["originator"] = originator
	}
	if betaFeatures := authFileClientProfileString(auth, coreauth.AuthFileCodexBetaFeaturesKey, "beta-features", "betaFeatures", "header:"+coreauth.AuthFileCodexBetaFeaturesHeader); betaFeatures != "" {
		profile["beta_features"] = betaFeatures
	}
	if installationID := authFileClientProfileString(auth, coreauth.AuthFileCodexInstallationIDKey, "installation-id", "installationId", "header:"+coreauth.AuthFileCodexInstallationIDHeader); installationID != "" {
		profile["installation_id"] = installationID
	}
	if includeTimingMetrics, ok := authFileClientProfileBool(auth, coreauth.AuthFileCodexIncludeTimingMetricsKey, "include-timing-metrics", "includeTimingMetrics", "header:"+coreauth.AuthFileCodexIncludeTimingMetricsHeader); ok {
		profile["include_timing_metrics"] = includeTimingMetrics
	}
	if userAgent := authFileUserAgent(auth); userAgent != "" {
		profile["user_agent"] = userAgent
	}
	return profile
}

func authFileClientProfileString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		for _, key := range keys {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
	}
	if auth.Metadata != nil {
		for _, key := range keys {
			if value, ok := auth.Metadata[key].(string); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
		if headers := authFileMetadataHeaders(auth.Metadata); len(headers) > 0 {
			for _, key := range keys {
				headerName, ok := strings.CutPrefix(key, "header:")
				if !ok {
					continue
				}
				if value := authFileHeaderValue(headers, headerName); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func authFileClientProfileBool(auth *coreauth.Auth, keys ...string) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if auth.Attributes != nil {
		for _, key := range keys {
			value, exists := auth.Attributes[key]
			if !exists {
				continue
			}
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			if err == nil {
				return parsed, true
			}
		}
	}
	if auth.Metadata != nil {
		for _, key := range keys {
			value, exists := auth.Metadata[key]
			if !exists {
				continue
			}
			switch typed := value.(type) {
			case bool:
				return typed, true
			case string:
				parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
				if err == nil {
					return parsed, true
				}
			}
		}
		if headers := authFileMetadataHeaders(auth.Metadata); len(headers) > 0 {
			for _, key := range keys {
				headerName, ok := strings.CutPrefix(key, "header:")
				if !ok {
					continue
				}
				parsed, err := strconv.ParseBool(authFileHeaderValue(headers, headerName))
				if err == nil {
					return parsed, true
				}
			}
		}
	}
	return false, false
}

func authFileMetadataHeaders(metadata map[string]any) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	raw, ok := metadata["headers"]
	if !ok || raw == nil {
		return nil
	}
	out := make(map[string]string)
	switch headers := raw.(type) {
	case map[string]any:
		for key, value := range headers {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if typed, ok := value.(string); ok {
				if trimmed := strings.TrimSpace(typed); trimmed != "" {
					out[key] = trimmed
				}
			}
		}
	case map[string]string:
		for key, value := range headers {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				out[key] = trimmed
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func authFileHeaderValue(headers map[string]string, headerName string) string {
	headerName = strings.TrimSpace(headerName)
	if len(headers) == 0 || headerName == "" {
		return ""
	}
	if value := strings.TrimSpace(headers[headerName]); value != "" {
		return value
	}
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), headerName) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func extractCodexIDTokenClaims(auth *coreauth.Auth) gin.H {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	result := gin.H{}
	if v := strings.TrimSpace(valueAsString(auth.Metadata["account_id"])); v != "" {
		result["chatgpt_account_id"] = v
	}
	if v := strings.TrimSpace(valueAsString(auth.Metadata["plan_type"])); v != "" {
		result["plan_type"] = v
	}
	if v, ok := auth.Metadata["chatgpt_subscription_active_start"]; ok && v != nil {
		result["chatgpt_subscription_active_start"] = v
	}
	if v, ok := auth.Metadata["chatgpt_subscription_active_until"]; ok && v != nil {
		result["chatgpt_subscription_active_until"] = v
	}
	if v, ok := codexSubscriptionUntilValue(auth.Metadata); ok {
		result["subscription_expires_at"] = v
	}

	idTokenRaw, ok := auth.Metadata["id_token"].(string)
	if !ok {
		if len(result) == 0 {
			return nil
		}
		return result
	}
	idToken := strings.TrimSpace(idTokenRaw)
	if idToken == "" {
		if len(result) == 0 {
			return nil
		}
		return result
	}
	claims, err := codex.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		if len(result) == 0 {
			return nil
		}
		return result
	}
	if _, ok := result["chatgpt_account_id"]; !ok {
		if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); v != "" {
			result["chatgpt_account_id"] = v
		}
	}
	if _, ok := result["plan_type"]; !ok {
		if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); v != "" {
			result["plan_type"] = v
		}
	}
	if _, ok := result["chatgpt_subscription_active_start"]; !ok {
		if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveStart; v != nil {
			result["chatgpt_subscription_active_start"] = v
		}
	}
	if _, ok := result["chatgpt_subscription_active_until"]; !ok {
		if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil; v != nil {
			result["chatgpt_subscription_active_until"] = v
		}
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func authEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["email"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["email"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["account_email"]); v != "" {
			return v
		}
	}
	return ""
}

func authProjectID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		for _, key := range []string{"project_id", "projectId"} {
			if v := strings.TrimSpace(valueAsString(auth.Metadata[key])); v != "" {
				return v
			}
		}
	}
	if auth.Attributes != nil {
		for _, key := range []string{"project_id", "projectId"} {
			if v := strings.TrimSpace(auth.Attributes[key]); v != "" {
				return v
			}
		}
	}
	return ""
}

func authAttribute(auth *coreauth.Auth, key string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return auth.Attributes[key]
}

func isRuntimeOnlyAuth(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}
