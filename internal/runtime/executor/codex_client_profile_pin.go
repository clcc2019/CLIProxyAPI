package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const codexPinnedBetaFeaturesHeader = "X-Codex-Beta-Features"
const codexClientProfilePinnedMetadataKey = "codex_client_profile_pinned"

var codexPinnedClientProfileHeaders = []string{
	codexPinnedBetaFeaturesHeader,
	"Version",
	codexHeaderInstallationID,
	"X-OpenAI-Subagent",
	"X-OAI-Attestation",
	"Traceparent",
	"Tracestate",
	misc.CodexResidencyHeader,
	"X-OpenAI-Fedramp",
	"x-responsesapi-include-timing-metrics",
}

func codexPinClientProfileFromFirstRequest(ctx context.Context, auth *cliproxyauth.Auth, target http.Header, source http.Header, cfg *config.Config) {
	if auth == nil || (target == nil && source == nil) {
		return
	}
	if codexClientProfilePinned(auth) {
		changed := false
		if codexMaybeUpdatePinnedUserAgent(auth, target, source) {
			changed = true
		}
		if codexMaybeUpdatePinnedVersion(auth, target, source) {
			changed = true
		}
		if changed {
			cliproxyauth.PublishAuthUpdate(ctx, auth)
		}
		return
	}

	changed := false
	codexEnsureAuthMetadata(auth)
	if pinned, ok := auth.Metadata[codexClientProfilePinnedMetadataKey].(bool); !ok || !pinned {
		auth.Metadata[codexClientProfilePinnedMetadataKey] = true
		changed = true
	}
	if value := firstNonEmptyHeaderValue(target, source, "User-Agent"); value != "" && codexAuthUserAgent(auth) == "" {
		auth.Metadata["user_agent"] = value
		codexSetAuthAttribute(auth, "header:User-Agent", value)
		codexSetAuthMetadataHeader(auth, "User-Agent", value)
		changed = true
	}
	if value := firstNonEmptyHeaderValue(target, source, "Originator"); value != "" && codexAuthOriginator(auth) == "" {
		codexEnsureAuthMetadata(auth)
		auth.Metadata["originator"] = value
		codexSetAuthAttribute(auth, "originator", value)
		changed = true
	}
	for _, headerName := range codexPinnedClientProfileHeaders {
		if codexAuthHeaderFixed(auth, headerName) {
			continue
		}
		value := firstNonEmptyHeaderValue(target, source, headerName)
		if value != "" && strings.EqualFold(headerName, "Version") && !codexVersionAtLeast(value, codexDefaultVersionHeader()) {
			value = codexDefaultVersionHeader()
		}
		if value == "" && strings.EqualFold(headerName, codexHeaderInstallationID) {
			if cfg != nil {
				value = strings.TrimSpace(cfg.CodexHeaderDefaults.InstallationID)
			}
			if value == "" {
				value = uuid.NewString()
			}
		}
		if value == "" {
			continue
		}
		codexSetAuthMetadataHeader(auth, headerName, value)
		codexSetAuthAttribute(auth, "header:"+headerName, value)
		changed = true
	}
	if !changed {
		return
	}

	cliproxyauth.PublishAuthUpdate(ctx, auth)
}

func codexMaybeUpdatePinnedUserAgent(auth *cliproxyauth.Auth, target http.Header, source http.Header) bool {
	if auth == nil {
		return false
	}
	candidate := firstNonEmptyHeaderValue(target, source, "User-Agent")
	if candidate == "" {
		return false
	}
	current := codexAuthUserAgent(auth)
	if current != "" && !codexPinnedUserAgentShouldUpdate(current, candidate) {
		return false
	}
	if current == "" || current != candidate {
		codexEnsureAuthMetadata(auth)
		auth.Metadata["user_agent"] = candidate
		codexSetAuthAttribute(auth, "header:User-Agent", candidate)
		codexSetAuthMetadataHeader(auth, "User-Agent", candidate)
		return true
	}
	return false
}

func codexMaybeUpdatePinnedVersion(auth *cliproxyauth.Auth, target http.Header, source http.Header) bool {
	if auth == nil {
		return false
	}
	candidate := firstNonEmptyHeaderValue(target, source, "Version")
	if candidate == "" {
		return false
	}
	if !codexVersionAtLeast(candidate, codexDefaultVersionHeader()) {
		candidate = codexDefaultVersionHeader()
	}
	current := codexAuthHeaderValue(auth, "Version")
	if current != "" && !codexPinnedVersionShouldUpdate(auth, target, source, current, candidate) {
		return false
	}
	if current == candidate {
		return false
	}
	codexSetAuthAttribute(auth, "header:Version", candidate)
	codexSetAuthMetadataHeader(auth, "Version", candidate)
	return true
}

func codexPinnedVersionShouldUpdate(auth *cliproxyauth.Auth, target http.Header, source http.Header, current string, candidate string) bool {
	current = strings.TrimSpace(current)
	candidate = strings.TrimSpace(candidate)
	if current == "" || candidate == "" || current == candidate {
		return false
	}
	currentProduct, _, okCurrentUA := codexUserAgentProductVersion(codexAuthUserAgent(auth))
	candidateProduct, _, okCandidateUA := codexUserAgentProductVersion(firstNonEmptyHeaderValue(target, source, "User-Agent"))
	if !okCurrentUA || !okCandidateUA || !strings.EqualFold(currentProduct, candidateProduct) {
		return false
	}
	cmp, ok := codexCompareVersions(candidate, current)
	return ok && cmp > 0
}

func codexPinnedUserAgentShouldUpdate(current string, candidate string) bool {
	current = strings.TrimSpace(current)
	candidate = strings.TrimSpace(candidate)
	if current == "" || candidate == "" || current == candidate {
		return false
	}
	currentProduct, currentVersion, okCurrent := codexUserAgentProductVersion(current)
	candidateProduct, candidateVersion, okCandidate := codexUserAgentProductVersion(candidate)
	if !okCurrent || !okCandidate || !strings.EqualFold(currentProduct, candidateProduct) {
		return false
	}
	cmp, ok := codexCompareVersions(candidateVersion, currentVersion)
	return ok && cmp > 0
}

func codexUserAgentProductVersion(userAgent string) (string, string, bool) {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return "", "", false
	}
	token := userAgent
	if idx := strings.IndexFunc(token, func(r rune) bool { return r == ' ' || r == '\t' }); idx >= 0 {
		token = token[:idx]
	}
	product, version, ok := strings.Cut(token, "/")
	if !ok {
		return "", "", false
	}
	product = strings.TrimSpace(product)
	version = strings.TrimSpace(version)
	if product == "" || version == "" {
		return "", "", false
	}
	if _, ok := codexParseVersion(version); !ok {
		return "", "", false
	}
	return product, version, true
}

func codexClientProfilePinned(auth *cliproxyauth.Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	pinned, _ := auth.Metadata[codexClientProfilePinnedMetadataKey].(bool)
	return pinned
}

func codexClientProfileSourceHeaders(auth *cliproxyauth.Auth, source http.Header) http.Header {
	if codexClientProfilePinned(auth) {
		return nil
	}
	return source
}

func codexPreparePinnedClientProfileHeaders(headers http.Header, auth *cliproxyauth.Auth) {
	if headers == nil || !codexClientProfilePinned(auth) {
		return
	}

	for existingKey := range headers {
		switch {
		case strings.EqualFold(existingKey, "User-Agent"):
			if codexAuthUserAgent(auth) == "" && !codexAuthHeaderFixed(auth, "User-Agent") {
				delete(headers, existingKey)
			}
		case strings.EqualFold(existingKey, "Originator"):
			if codexAuthOriginator(auth) == "" && !codexAuthHeaderFixed(auth, "Originator") {
				delete(headers, existingKey)
			}
		default:
			for _, headerName := range codexPinnedClientProfileHeaders {
				if !strings.EqualFold(existingKey, headerName) {
					continue
				}
				if !codexAuthHeaderFixed(auth, headerName) {
					delete(headers, existingKey)
				}
				break
			}
		}
	}
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

func codexAuthHeaderValue(auth *cliproxyauth.Auth, name string) string {
	name = strings.TrimSpace(name)
	if auth == nil || name == "" {
		return ""
	}
	if len(auth.Attributes) > 0 {
		for key, value := range auth.Attributes {
			headerName, ok := strings.CutPrefix(key, "header:")
			if !ok {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(headerName), name) && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return ""
	}
	return codexMetadataHeaderValue(auth.Metadata, name)
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
