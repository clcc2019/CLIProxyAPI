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
	if userAgent, ok := authFileMetadataFirstString(auth.Metadata, "user_agent", "user-agent", "userAgent"); ok {
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
	if originator, ok := authFileMetadataFirstString(auth.Metadata, authFileCodexOriginatorKeys...); ok {
		authFileSetOriginatorAttribute(auth, originator)
	}
	if betaFeatures, ok := authFileMetadataFirstString(auth.Metadata, authFileCodexBetaFeaturesKeys...); ok {
		authFileSetHeaderAttribute(auth, AuthFileCodexBetaFeaturesHeader, betaFeatures, authFileCodexBetaFeaturesKeys...)
	}
	if installationID, ok := authFileMetadataFirstString(auth.Metadata, authFileCodexInstallationIDKeys...); ok {
		authFileSetHeaderAttribute(auth, AuthFileCodexInstallationIDHeader, installationID, authFileCodexInstallationIDKeys...)
	}
	if includeTimingMetrics, ok := authFileMetadataFirstBool(auth.Metadata, authFileCodexIncludeTimingMetricsKeys...); ok {
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
