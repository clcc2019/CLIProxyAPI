package auth

import (
	"strings"
	"unicode"
)

const CodexRemoteCompactionV2Feature = "remote_compaction_v2"

// FilterCodexBetaFeaturesForPersistence removes request-scoped Codex features
// from a beta-feature list before it is retained in an auth record.
func FilterCodexBetaFeaturesForPersistence(value string) string {
	filtered, _ := filterCodexBetaFeaturesForPersistence(value)
	return filtered
}

func filterCodexBetaFeaturesForPersistence(value string) (string, bool) {
	features := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	if len(features) == 0 {
		return value, false
	}

	kept := make([]string, 0, len(features))
	removed := false
	for _, feature := range features {
		if strings.EqualFold(feature, CodexRemoteCompactionV2Feature) {
			removed = true
			continue
		}
		kept = append(kept, feature)
	}
	if !removed {
		return value, false
	}
	return strings.Join(kept, ","), true
}

// SanitizeCodexAuthMetadata removes request-scoped Codex features from every
// supported auth-file representation. It returns a copy-on-write result so a
// persistence snapshot cannot mutate nested maps shared with the live auth.
func SanitizeCodexAuthMetadata(metadata map[string]any) (map[string]any, bool) {
	return sanitizeCodexAuthMetadataMap(metadata)
}

// StripNonPersistentCodexFeatures removes request-scoped Codex features from
// both serialized metadata and projected runtime attributes.
func StripNonPersistentCodexFeatures(auth *Auth) bool {
	if auth == nil {
		return false
	}

	changed := false
	if sanitized, metadataChanged := SanitizeCodexAuthMetadata(auth.Metadata); metadataChanged {
		auth.Metadata = sanitized
		changed = true
	}
	for key, value := range auth.Attributes {
		if !codexBetaFeaturesStorageKey(key) {
			continue
		}
		filtered, removed := filterCodexBetaFeaturesForPersistence(value)
		if !removed {
			continue
		}
		changed = true
		if filtered == "" {
			delete(auth.Attributes, key)
			continue
		}
		auth.Attributes[key] = filtered
	}
	return changed
}

func sanitizeCodexAuthMetadataMap(source map[string]any) (map[string]any, bool) {
	if len(source) == 0 {
		return source, false
	}

	result := source
	changed := false
	clone := func() {
		if changed {
			return
		}
		result = make(map[string]any, len(source))
		for key, value := range source {
			result[key] = value
		}
		changed = true
	}

	for key, value := range source {
		if codexBetaFeaturesStorageKey(key) {
			if text, ok := value.(string); ok {
				filtered, removed := filterCodexBetaFeaturesForPersistence(text)
				if removed {
					clone()
					if filtered == "" {
						delete(result, key)
					} else {
						result[key] = filtered
					}
					continue
				}
			}
		}

		sanitized, nestedChanged := sanitizeCodexAuthMetadataValue(value)
		if !nestedChanged {
			continue
		}
		clone()
		result[key] = sanitized
	}
	return result, changed
}

func sanitizeCodexAuthMetadataValue(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return sanitizeCodexAuthMetadataMap(typed)
	case map[string]string:
		return sanitizeCodexAuthStringMap(typed)
	case []any:
		return sanitizeCodexAuthMetadataSlice(typed)
	default:
		return value, false
	}
}

func sanitizeCodexAuthStringMap(source map[string]string) (map[string]string, bool) {
	if len(source) == 0 {
		return source, false
	}
	var result map[string]string
	for key, value := range source {
		if !codexBetaFeaturesStorageKey(key) {
			continue
		}
		filtered, removed := filterCodexBetaFeaturesForPersistence(value)
		if !removed {
			continue
		}
		if result == nil {
			result = make(map[string]string, len(source))
			for sourceKey, sourceValue := range source {
				result[sourceKey] = sourceValue
			}
		}
		if filtered == "" {
			delete(result, key)
		} else {
			result[key] = filtered
		}
	}
	if result == nil {
		return source, false
	}
	return result, true
}

func sanitizeCodexAuthMetadataSlice(source []any) ([]any, bool) {
	var result []any
	for index, value := range source {
		sanitized, changed := sanitizeCodexAuthMetadataValue(value)
		if !changed {
			continue
		}
		if result == nil {
			result = append([]any(nil), source...)
		}
		result[index] = sanitized
	}
	if result == nil {
		return source, false
	}
	return result, true
}

func codexBetaFeaturesStorageKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "beta_features", "beta-features", "betafeatures",
		"x-codex-beta-features", "header:x-codex-beta-features":
		return true
	default:
		return false
	}
}
