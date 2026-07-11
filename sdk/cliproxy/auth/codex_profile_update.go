package auth

import (
	"context"
	"strings"

	"golang.org/x/mod/semver"
)

type executionAuthProfileUpdateContextKey struct{}

func withExecutionAuthProfileUpdate(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionAuthProfileUpdateContextKey{}, true)
}

func isExecutionAuthProfileUpdate(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(executionAuthProfileUpdateContextKey{}).(bool)
	return value
}

// mergeCodexExecutionAuthProfile applies an executor-published profile update
// to the latest manager-owned auth snapshot. It runs while Manager.mu is held,
// so concurrent version updates remain monotonic and a stale execution snapshot
// cannot replace a newer client profile.
func mergeCodexExecutionAuthProfile(existing, candidate *Auth) (*Auth, bool) {
	if existing == nil || candidate == nil || !strings.EqualFold(strings.TrimSpace(existing.Provider), "codex") {
		return candidate, true
	}
	if !codexAuthProfilePinned(candidate) {
		return existing.Clone(), false
	}

	merged := existing.Clone()
	detachCodexProfileMetadataHeaders(merged)
	if !codexAuthProfilePinned(existing) {
		copyCodexProfile(merged, candidate)
		return merged, true
	}

	existingProduct, existingVersion, existingOK := codexProfileUserAgentProductVersion(codexProfileUserAgent(existing))
	candidateProduct, candidateVersion, candidateOK := codexProfileUserAgentProductVersion(codexProfileUserAgent(candidate))
	if !existingOK || !candidateOK || !strings.EqualFold(existingProduct, candidateProduct) {
		return merged, false
	}
	if semver.Compare("v"+candidateVersion, "v"+existingVersion) <= 0 {
		return merged, false
	}

	setCodexProfileUserAgent(merged, codexProfileUserAgent(candidate))
	setCodexProfileHeader(merged, "Version", candidateVersion)
	return merged, true
}

func codexAuthProfilePinned(auth *Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	pinned, _ := auth.Metadata["codex_client_profile_pinned"].(bool)
	return pinned
}

func copyCodexProfile(dst, src *Auth) {
	if dst == nil || src == nil {
		return
	}
	if dst.Attributes == nil {
		dst.Attributes = make(map[string]string)
	}
	for key, value := range src.Attributes {
		if strings.HasPrefix(key, "header:") || key == "originator" {
			dst.Attributes[key] = value
		}
	}
	if dst.Metadata == nil {
		dst.Metadata = make(map[string]any)
	}
	for _, key := range []string{"codex_client_profile_pinned", "originator", "user_agent"} {
		if value, ok := src.Metadata[key]; ok {
			dst.Metadata[key] = value
		}
	}
	if headers := ExtractCustomHeadersFromMetadata(src.Metadata); len(headers) > 0 {
		cloned := make(map[string]any, len(headers))
		for key, value := range headers {
			cloned[key] = value
		}
		dst.Metadata["headers"] = cloned
	}
}

func detachCodexProfileMetadataHeaders(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	headers := ExtractCustomHeadersFromMetadata(auth.Metadata)
	if len(headers) == 0 {
		return
	}
	cloned := make(map[string]any, len(headers)+2)
	for key, value := range headers {
		cloned[key] = value
	}
	auth.Metadata["headers"] = cloned
}

func codexProfileUserAgent(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if value := codexProfileAttributeHeader(auth.Attributes, "User-Agent"); value != "" {
		return value
	}
	if auth.Metadata != nil {
		if value, _ := auth.Metadata["user_agent"].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func codexProfileUserAgentProductVersion(userAgent string) (product string, version string, ok bool) {
	token := strings.TrimSpace(userAgent)
	if index := strings.IndexAny(token, " \t"); index >= 0 {
		token = token[:index]
	}
	product, version, ok = strings.Cut(token, "/")
	product = strings.TrimSpace(product)
	version = strings.TrimSpace(version)
	if !ok || product == "" || version == "" || !semver.IsValid("v"+version) {
		return "", "", false
	}
	return product, version, true
}

func codexProfileAttributeHeader(attributes map[string]string, name string) string {
	for key, value := range attributes {
		headerName, ok := strings.CutPrefix(key, "header:")
		if ok && strings.EqualFold(strings.TrimSpace(headerName), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func setCodexProfileUserAgent(auth *Auth, userAgent string) {
	if auth == nil || strings.TrimSpace(userAgent) == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["user_agent"] = strings.TrimSpace(userAgent)
	setCodexProfileHeader(auth, "User-Agent", userAgent)
}

func setCodexProfileHeader(auth *Auth, name string, value string) {
	if auth == nil || strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for key := range auth.Attributes {
		headerName, ok := strings.CutPrefix(key, "header:")
		if ok && strings.EqualFold(strings.TrimSpace(headerName), name) {
			delete(auth.Attributes, key)
		}
	}
	auth.Attributes["header:"+name] = strings.TrimSpace(value)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	headers, _ := auth.Metadata["headers"].(map[string]any)
	if headers == nil {
		headers = make(map[string]any)
		auth.Metadata["headers"] = headers
	}
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			delete(headers, key)
		}
	}
	headers[name] = strings.TrimSpace(value)
}
