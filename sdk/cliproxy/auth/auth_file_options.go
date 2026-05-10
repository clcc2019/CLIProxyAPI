package auth

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ApplyAuthFileOptionsFromMetadata maps editable auth-file fields from Metadata
// onto the runtime Auth fields used by routing, scheduling, and HTTP clients.
func ApplyAuthFileOptionsFromMetadata(auth *Auth) {
	if auth == nil || len(auth.Metadata) == 0 {
		return
	}
	if proxyURL, ok := authFileMetadataString(auth.Metadata, "proxy_url"); ok {
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
	if note, ok := authFileMetadataString(auth.Metadata, "note"); ok {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		if note == "" {
			delete(auth.Attributes, "note")
		} else {
			auth.Attributes["note"] = note
		}
	}
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
