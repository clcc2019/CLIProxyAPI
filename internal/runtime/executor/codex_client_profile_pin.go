package executor

import (
	"context"
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const codexPinnedBetaFeaturesHeader = "X-Codex-Beta-Features"

func codexPinClientProfileFromFirstRequest(ctx context.Context, auth *cliproxyauth.Auth, target http.Header, source http.Header) {
	if auth == nil || (target == nil && source == nil) || codexIsAPIKeyAuth(auth) {
		return
	}

	changed := false
	if value := firstNonEmptyHeaderValue(target, source, "User-Agent"); value != "" && codexAuthUserAgent(auth) == "" {
		codexEnsureAuthMetadata(auth)
		auth.Metadata["user_agent"] = value
		codexSetAuthAttribute(auth, "header:User-Agent", value)
		changed = true
	}
	if value := firstNonEmptyHeaderValue(target, source, "Originator"); value != "" && codexAuthOriginator(auth) == "" {
		codexEnsureAuthMetadata(auth)
		auth.Metadata["originator"] = value
		codexSetAuthAttribute(auth, "originator", value)
		changed = true
	}
	if value := firstNonEmptyHeaderValue(target, source, codexPinnedBetaFeaturesHeader); value != "" && !codexAuthHeaderFixed(auth, codexPinnedBetaFeaturesHeader) {
		codexSetAuthMetadataHeader(auth, codexPinnedBetaFeaturesHeader, value)
		codexSetAuthAttribute(auth, "header:"+codexPinnedBetaFeaturesHeader, value)
		changed = true
	}
	if !changed {
		return
	}

	cliproxyauth.PublishAuthUpdate(ctx, auth)
}

func codexEnsureAuthMetadata(auth *cliproxyauth.Auth) {
	if auth != nil && auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
}

func codexSetAuthAttribute(auth *cliproxyauth.Auth, key string, value string) {
	if auth == nil {
		return
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[key] = value
}

func codexAuthHeaderFixed(auth *cliproxyauth.Auth, name string) bool {
	name = strings.TrimSpace(name)
	if auth == nil || name == "" {
		return false
	}
	if len(auth.Attributes) > 0 {
		for key, value := range auth.Attributes {
			headerName, ok := strings.CutPrefix(key, "header:")
			if !ok {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(headerName), name) && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	return codexMetadataHeaderValue(auth.Metadata, name) != ""
}

func codexMetadataHeaderValue(metadata map[string]any, name string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata["headers"]
	if !ok || raw == nil {
		return ""
	}
	switch headers := raw.(type) {
	case map[string]any:
		for key, value := range headers {
			if !strings.EqualFold(strings.TrimSpace(key), name) {
				continue
			}
			if typed, ok := value.(string); ok {
				return strings.TrimSpace(typed)
			}
		}
	case map[string]string:
		for key, value := range headers {
			if strings.EqualFold(strings.TrimSpace(key), name) {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func codexSetAuthMetadataHeader(auth *cliproxyauth.Auth, name string, value string) {
	if auth == nil {
		return
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" || value == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	headers, ok := auth.Metadata["headers"].(map[string]any)
	if !ok || headers == nil {
		headers = make(map[string]any)
		if existing, okExisting := auth.Metadata["headers"].(map[string]string); okExisting {
			for key, existingValue := range existing {
				if strings.TrimSpace(key) != "" && strings.TrimSpace(existingValue) != "" {
					headers[key] = strings.TrimSpace(existingValue)
				}
			}
		}
		auth.Metadata["headers"] = headers
	}
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			delete(headers, key)
		}
	}
	headers[name] = value
}
