package auth

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	// AuthFileServiceTierPassthroughKey is the canonical auth-file field that
	// lets Codex executors preserve a client-provided service_tier.
	AuthFileServiceTierPassthroughKey = "service_tier_passthrough"

	// AuthFileCodexBetaFeaturesKey is the canonical auth-file field for the
	// X-Codex-Beta-Features client profile header.
	AuthFileCodexBetaFeaturesKey = "beta_features"
	// AuthFileCodexInstallationIDKey is the canonical auth-file field for the
	// X-Codex-Installation-Id client profile header.
	AuthFileCodexInstallationIDKey = "installation_id"
	// AuthFileCodexIncludeTimingMetricsKey is the canonical auth-file field for
	// x-responsesapi-include-timing-metrics.
	AuthFileCodexIncludeTimingMetricsKey = "include_timing_metrics"
	AuthFileCodexOriginatorKey           = "originator"
	// AuthFileCodexClientProfilePinnedKey makes the client profile stored in an
	// auth file authoritative for that credential. It prevents downstream
	// requests from changing the credential's Codex client fingerprint.
	AuthFileCodexClientProfilePinnedKey = "codex_client_profile_pinned"

	AuthFileCodexBetaFeaturesHeader         = "X-Codex-Beta-Features"
	AuthFileCodexInstallationIDHeader       = "X-Codex-Installation-Id"
	AuthFileCodexIncludeTimingMetricsHeader = "x-responsesapi-include-timing-metrics"
	AuthFileCodexOriginatorHeader           = "Originator"
)

var authFileServiceTierPassthroughKeys = []string{
	AuthFileServiceTierPassthroughKey,
	"service-tier-passthrough",
	"serviceTierPassthrough",
	"fast",
}

var authFileCodexBetaFeaturesKeys = []string{
	AuthFileCodexBetaFeaturesKey,
	"beta-features",
	"betaFeatures",
}

var authFileCodexInstallationIDKeys = []string{
	AuthFileCodexInstallationIDKey,
	"installation-id",
	"installationId",
}

var authFileCodexIncludeTimingMetricsKeys = []string{
	AuthFileCodexIncludeTimingMetricsKey,
	"include-timing-metrics",
	"includeTimingMetrics",
}

var authFileCodexOriginatorKeys = []string{
	AuthFileCodexOriginatorKey,
	AuthFileCodexOriginatorHeader,
}

var authFileClientProfileObjectKeys = []string{
	"client_profile",
	"clientProfile",
	"client_features",
	"clientFeatures",
}

// ApplyAuthFileOptionsFromMetadata maps editable auth-file fields from Metadata
// onto the runtime Auth fields used by routing, scheduling, and HTTP clients.
func ApplyAuthFileOptionsFromMetadata(auth *Auth) {
	if auth == nil || len(auth.Metadata) == 0 {
		return
	}
	if proxyURL, ok := authFileMetadataFirstAnyString(auth.Metadata, "proxy_url", "proxy-url", "proxyUrl"); ok {
		auth.ProxyURL = proxyURL
	}
	if prefix, ok := authFileMetadataString(auth.Metadata, "prefix"); ok {
		auth.Prefix = normalizeAuthFilePrefix(prefix)
	}
	if priority, ok := authFileMetadataInt(auth.Metadata["priority"]); ok {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		auth.Attributes["priority"] = strconv.Itoa(priority)
	} else if _, exists := auth.Metadata["priority"]; exists && auth.Attributes != nil {
		delete(auth.Attributes, "priority")
	}
	if note, ok := authFileMetadataStrictString(auth.Metadata, "note"); ok {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		if note == "" {
			delete(auth.Attributes, "note")
		} else {
			auth.Attributes["note"] = note
		}
	}
	if userAgent, ok := authFileClientProfileString(auth.Metadata, "user_agent", "user-agent", "userAgent", "header:User-Agent"); ok {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		delete(auth.Attributes, "user_agent")
		delete(auth.Attributes, "user-agent")
		delete(auth.Attributes, "userAgent")
		if userAgent == "" {
			delete(auth.Attributes, "header:User-Agent")
		} else {
			auth.Attributes["header:User-Agent"] = userAgent
		}
	}
	if originator, ok := authFileClientProfileString(auth.Metadata, AuthFileCodexOriginatorKey, AuthFileCodexOriginatorHeader, "header:"+AuthFileCodexOriginatorHeader); ok {
		authFileSetOriginatorAttribute(auth, originator)
	}
	if betaFeatures, ok := authFileClientProfileString(auth.Metadata, AuthFileCodexBetaFeaturesKey, "beta-features", "betaFeatures", "header:"+AuthFileCodexBetaFeaturesHeader); ok {
		authFileSetHeaderAttribute(auth, AuthFileCodexBetaFeaturesHeader, betaFeatures, authFileCodexBetaFeaturesKeys...)
	}
	if installationID, ok := authFileClientProfileString(auth.Metadata, AuthFileCodexInstallationIDKey, "installation-id", "installationId", "header:"+AuthFileCodexInstallationIDHeader); ok {
		authFileSetHeaderAttribute(auth, AuthFileCodexInstallationIDHeader, installationID, authFileCodexInstallationIDKeys...)
	}
	if includeTimingMetrics, ok := authFileClientProfileBool(auth.Metadata, AuthFileCodexIncludeTimingMetricsKey, "include-timing-metrics", "includeTimingMetrics", "header:"+AuthFileCodexIncludeTimingMetricsHeader); ok {
		if includeTimingMetrics {
			authFileSetHeaderAttribute(auth, AuthFileCodexIncludeTimingMetricsHeader, "true", authFileCodexIncludeTimingMetricsKeys...)
		} else {
			authFileSetHeaderAttribute(auth, AuthFileCodexIncludeTimingMetricsHeader, "", authFileCodexIncludeTimingMetricsKeys...)
		}
	}
	if websockets, ok := authFileMetadataFirstBool(auth.Metadata, "websockets", "websocket"); ok {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		auth.Attributes["websockets"] = strconv.FormatBool(websockets)
	}
	if serviceTierPassthrough, ok := authFileMetadataFirstBool(auth.Metadata, authFileServiceTierPassthroughKeys...); ok {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		auth.Attributes[AuthFileServiceTierPassthroughKey] = strconv.FormatBool(serviceTierPassthrough)
	}
}

// AuthFileProviderFromMetadata infers the runtime provider for auth-file JSON.
func AuthFileProviderFromMetadata(metadata map[string]any) string {
	provider := strings.TrimSpace(authFileProjectionString(metadata, "type"))
	if provider != "" {
		return provider
	}
	if AuthFileLooksLikeCodexClientProfile(metadata) {
		return "codex"
	}
	return ""
}

// AuthFileLooksLikeCodexClientProfile recognizes auth files that only contain
// Codex client identity fields, such as installation_id plus user_agent.
func AuthFileLooksLikeCodexClientProfile(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	installationID, ok := authFileClientProfileString(metadata, AuthFileCodexInstallationIDKey, "installation-id", "installationId", "header:"+AuthFileCodexInstallationIDHeader)
	if !ok || strings.TrimSpace(installationID) == "" {
		return false
	}
	if value, ok := authFileClientProfileString(metadata, "user_agent", "user-agent", "userAgent", "header:User-Agent"); ok && strings.TrimSpace(value) != "" {
		return true
	}
	if value, ok := authFileClientProfileString(metadata, AuthFileCodexOriginatorKey, AuthFileCodexOriginatorHeader, "header:"+AuthFileCodexOriginatorHeader); ok && strings.TrimSpace(value) != "" {
		return true
	}
	value, ok := authFileClientProfileString(metadata, AuthFileCodexBetaFeaturesKey, "beta-features", "betaFeatures", "header:"+AuthFileCodexBetaFeaturesHeader)
	return ok && strings.TrimSpace(value) != ""
}

// ApplyAuthFileWebsocketDefault enables Codex auth-file websocket transport by
// default while preserving explicit websockets/websocket values.
func ApplyAuthFileWebsocketDefault(auth *Auth) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	if len(auth.Metadata) > 0 {
		for _, key := range []string{"websockets", "websocket"} {
			if _, ok := auth.Metadata[key]; ok {
				return
			}
		}
	}
	if len(auth.Attributes) > 0 {
		for _, key := range []string{"websockets", "websocket"} {
			if raw := strings.TrimSpace(auth.Attributes[key]); raw != "" {
				return
			}
		}
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["websockets"] = "true"
}

func authFileSetOriginatorAttribute(auth *Auth, originator string) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	originator = strings.TrimSpace(originator)
	delete(auth.Attributes, AuthFileCodexOriginatorKey)
	delete(auth.Attributes, AuthFileCodexOriginatorHeader)
	delete(auth.Attributes, "header:"+AuthFileCodexOriginatorHeader)
	if originator == "" {
		return
	}
	auth.Attributes[AuthFileCodexOriginatorKey] = originator
	auth.Attributes["header:"+AuthFileCodexOriginatorHeader] = originator
}

func authFileSetHeaderAttribute(auth *Auth, headerName string, value string, aliasKeys ...string) {
	if auth == nil {
		return
	}
	headerName = strings.TrimSpace(headerName)
	if headerName == "" {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for _, key := range aliasKeys {
		delete(auth.Attributes, key)
	}
	attrKey := "header:" + headerName
	value = strings.TrimSpace(value)
	if value == "" {
		delete(auth.Attributes, attrKey)
		return
	}
	auth.Attributes[attrKey] = value
}

func authFileMetadataString(metadata map[string]any, key string) (string, bool) {
	if len(metadata) == 0 {
		return "", false
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return "", ok
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v), true
	default:
		return strings.TrimSpace(fmt.Sprint(v)), true
	}
}

func authFileMetadataFirstString(metadata map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := authFileMetadataStrictString(metadata, key); ok {
			return value, true
		}
	}
	return "", false
}

func authFileMetadataFirstAnyString(metadata map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := authFileMetadataString(metadata, key); ok {
			return value, true
		}
	}
	return "", false
}

func authFileMetadataStrictString(metadata map[string]any, key string) (string, bool) {
	if len(metadata) == 0 {
		return "", false
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return "", ok
	}
	str, ok := value.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(str), true
}

func authFileMetadataFirstBool(metadata map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		value, exists := metadata[key]
		if !exists {
			continue
		}
		return authFileMetadataBool(value)
	}
	return false, false
}

func authFileClientProfileString(metadata map[string]any, keys ...string) (string, bool) {
	if value, ok := authFileMetadataFirstString(metadata, keys...); ok {
		return value, true
	}
	if value, ok := authFileHeadersString(ExtractCustomHeadersFromMetadata(metadata), keys...); ok {
		return value, true
	}
	for _, objectKey := range authFileClientProfileObjectKeys {
		nested, ok := authFileNestedMetadata(metadata, objectKey)
		if !ok {
			continue
		}
		if value, ok := authFileMetadataFirstString(nested, keys...); ok {
			return value, true
		}
		if value, ok := authFileHeadersString(ExtractCustomHeadersFromMetadata(nested), keys...); ok {
			return value, true
		}
	}
	return "", false
}

func authFileClientProfileBool(metadata map[string]any, keys ...string) (bool, bool) {
	if value, ok := authFileMetadataFirstBool(metadata, keys...); ok {
		return value, true
	}
	if value, ok := authFileHeadersBool(ExtractCustomHeadersFromMetadata(metadata), keys...); ok {
		return value, true
	}
	for _, objectKey := range authFileClientProfileObjectKeys {
		nested, ok := authFileNestedMetadata(metadata, objectKey)
		if !ok {
			continue
		}
		if value, ok := authFileMetadataFirstBool(nested, keys...); ok {
			return value, true
		}
		if value, ok := authFileHeadersBool(ExtractCustomHeadersFromMetadata(nested), keys...); ok {
			return value, true
		}
	}
	return false, false
}

func authFileNestedMetadata(metadata map[string]any, key string) (map[string]any, bool) {
	if len(metadata) == 0 {
		return nil, false
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return nil, false
	}
	nested, ok := raw.(map[string]any)
	return nested, ok
}

func authFileHeadersString(headers map[string]string, keys ...string) (string, bool) {
	for _, key := range keys {
		headerName, ok := strings.CutPrefix(key, "header:")
		if !ok {
			continue
		}
		if value := authFileHeaderValue(headers, headerName); value != "" {
			return value, true
		}
	}
	return "", false
}

func authFileHeadersBool(headers map[string]string, keys ...string) (bool, bool) {
	for _, key := range keys {
		headerName, ok := strings.CutPrefix(key, "header:")
		if !ok {
			continue
		}
		value := authFileHeaderValue(headers, headerName)
		if value == "" {
			continue
		}
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			return parsed, true
		}
	}
	return false, false
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

func authFileMetadataBool(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return parsed, err == nil
	default:
		return false, false
	}
}

func normalizeAuthFilePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" || strings.Contains(prefix, "/") {
		return ""
	}
	return prefix
}

func authFileMetadataInt(value any) (int, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
