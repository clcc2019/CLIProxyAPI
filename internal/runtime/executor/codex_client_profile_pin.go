package executor

import (
	"context"
	"net/http"
	"strings"
	"sync"

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
	codexWireHeaderOpenAISubagent,
	codexWireHeaderOAIAttestation,
	"Traceparent",
	"Tracestate",
	misc.CodexResidencyHeader,
	codexWireHeaderOpenAIFedramp,
	codexWireHeaderResponsesAPIIncludeTimingMetrics,
}

type codexClientProfile struct {
	headers http.Header
}

type codexClientProfileKey struct {
	id   string
	auth *cliproxyauth.Auth
}

var (
	codexClientProfilesMu sync.RWMutex
	codexClientProfiles   = make(map[codexClientProfileKey]codexClientProfile)
)

func codexPinClientProfileFromFirstRequest(ctx context.Context, auth *cliproxyauth.Auth, target http.Header, source http.Header, cfg *config.Config) {
	if auth == nil || (target == nil && source == nil) {
		return
	}
	key, ok := codexClientProfileKeyForAuth(auth)
	if !ok {
		return
	}

	var (
		profileToPublish codexClientProfile
		publishProfile   bool
	)
	codexClientProfilesMu.Lock()
	if profile, exists := codexClientProfiles[key]; exists && len(profile.headers) > 0 {
		if updated, ok := codexPinnedClientVersionUpdate(profile, target, source); ok {
			codexClientProfiles[key] = updated
			profileToPublish = updated
			publishProfile = true
		}
	} else {
		delete(codexClientProfiles, key)
		if profile, exists := codexLegacyClientProfileFromAuth(auth); exists {
			if updated, ok := codexPinnedClientVersionUpdate(profile, target, source); ok {
				profile = updated
				profileToPublish = updated
				publishProfile = true
			}
			codexClientProfiles[key] = profile
		} else {
			profile := codexNewClientProfileFromRequest(auth, target, source, cfg)
			if len(profile.headers) > 0 {
				codexClientProfiles[key] = profile
				profileToPublish = profile
				publishProfile = true
			}
		}
	}
	codexClientProfilesMu.Unlock()

	if !publishProfile {
		return
	}
	// The execution auth is a snapshot that may share nested maps with the
	// manager-owned record. Publish a detached candidate instead of mutating it
	// in place, so the manager can merge and persist only this profile update.
	cliproxyauth.PublishAuthProfileUpdate(ctx, codexAuthWithPinnedClientProfile(auth, profileToPublish))
}

func codexAuthWithPinnedClientProfile(auth *cliproxyauth.Auth, profile codexClientProfile) *cliproxyauth.Auth {
	candidate := auth.Clone()
	if candidate == nil {
		return nil
	}
	if candidate.Metadata == nil {
		candidate.Metadata = make(map[string]any)
	}
	candidate.Metadata[codexClientProfilePinnedMetadataKey] = true

	for _, headerName := range codexAllClientProfileHeaders() {
		value := trimHeaderValue(profile.headers, headerName)
		if value == "" {
			continue
		}
		codexSetAuthProfileHeader(candidate, headerName, value)
		switch {
		case strings.EqualFold(headerName, "User-Agent"):
			candidate.Metadata["user_agent"] = value
		case strings.EqualFold(headerName, "Originator"):
			candidate.Metadata["originator"] = value
			candidate.Attributes["originator"] = value
		}
	}
	return candidate
}

func codexNewClientProfileFromRequest(auth *cliproxyauth.Auth, target http.Header, source http.Header, cfg *config.Config) codexClientProfile {
	profile := codexClientProfile{headers: make(http.Header, len(codexPinnedClientProfileHeaders)+2)}
	legacyGeneratedProfile := codexGeneratedDefaultUserAgent(codexAuthUserAgent(auth))

	// Resolve the legacy originator before replacing its generated User-Agent.
	// codexGeneratedDefaultOriginator uses the current User-Agent to distinguish
	// an old generated profile from an explicit codex_cli_rs profile.
	if value := firstNonEmptyHeaderValue(target, source, "Originator"); value != "" {
		current := codexAuthOriginator(auth)
		if current == "" || codexGeneratedDefaultOriginator(auth, current) {
			codexSetHeaderCasePreserved(profile.headers, "Originator", value)
		}
	}
	if value := firstNonEmptyHeaderValue(target, source, "User-Agent"); value != "" {
		current := codexAuthUserAgent(auth)
		if current == "" || codexGeneratedDefaultUserAgent(current) {
			codexSetHeaderCasePreserved(profile.headers, "User-Agent", value)
		}
	}
	for _, headerName := range codexPinnedClientProfileHeaders {
		if codexClientProfileAuthHeaderFixed(auth, headerName) && !(legacyGeneratedProfile && strings.EqualFold(headerName, "Version")) {
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
		codexSetHeaderCasePreserved(profile.headers, headerName, value)
	}
	return profile
}

func codexPinnedClientVersionUpdate(profile codexClientProfile, target http.Header, source http.Header) (codexClientProfile, bool) {
	currentProduct, currentVersion, okCurrent := codexUserAgentProductVersion(trimHeaderValue(profile.headers, "User-Agent"))
	candidateUserAgent := firstNonEmptyHeaderValue(target, source, "User-Agent")
	candidateProduct, candidateVersion, okCandidate := codexUserAgentProductVersion(candidateUserAgent)
	if !okCurrent || !okCandidate || !strings.EqualFold(currentProduct, candidateProduct) {
		return codexClientProfile{}, false
	}
	cmp, comparable := codexCompareVersions(candidateVersion, currentVersion)
	if !comparable || cmp <= 0 {
		return codexClientProfile{}, false
	}
	if headerVersion := firstNonEmptyHeaderValue(target, source, "Version"); headerVersion != "" {
		if versionCmp, valid := codexCompareVersions(headerVersion, candidateVersion); !valid || versionCmp != 0 {
			return codexClientProfile{}, false
		}
	}
	updated := codexClientProfile{headers: profile.headers.Clone()}
	if updated.headers == nil {
		updated.headers = make(http.Header, 2)
	}
	codexSetHeaderCasePreserved(updated.headers, "User-Agent", candidateUserAgent)
	codexSetHeaderCasePreserved(updated.headers, "Version", candidateVersion)
	return updated, true
}

func codexGeneratedDefaultOriginator(auth *cliproxyauth.Auth, originator string) bool {
	originator = strings.TrimSpace(originator)
	if originator == "" {
		return false
	}
	if !strings.EqualFold(originator, misc.CodexDefaultOriginator) {
		return false
	}
	userAgent := codexAuthUserAgent(auth)
	return userAgent == "" || codexGeneratedDefaultUserAgent(userAgent)
}

func codexGeneratedDefaultUserAgent(userAgent string) bool {
	product, version, ok := codexUserAgentProductVersion(userAgent)
	if !ok || !strings.EqualFold(product, misc.CodexDefaultOriginator) {
		return false
	}

	// New auth records no longer persist generated defaults. Treat only an older
	// codex_cli_rs version as a legacy generated value; a current/newer value may
	// have been explicitly configured and must remain pinned.
	cmp, ok := codexCompareVersions(version, codexDefaultVersionHeader())
	return ok && cmp < 0
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
	_, ok := codexPinnedClientProfileForAuth(auth)
	return ok
}

func codexClientProfileSourceHeaders(auth *cliproxyauth.Auth, source http.Header) http.Header {
	if profile, ok := codexPinnedClientProfileForAuth(auth); ok {
		return profile.headers.Clone()
	}
	return source
}

func codexPreparePinnedClientProfileHeaders(headers http.Header, auth *cliproxyauth.Auth) {
	if headers == nil {
		return
	}
	profile, ok := codexPinnedClientProfileForAuth(auth)
	if !ok {
		return
	}

	for existingKey := range headers {
		if !codexPinnedClientProfileHeaderName(existingKey) || codexClientProfileAuthHeaderFixed(auth, existingKey) {
			continue
		}
		if trimHeaderValue(profile.headers, existingKey) == "" {
			delete(headers, existingKey)
		}
	}
	for _, headerName := range codexAllClientProfileHeaders() {
		if codexClientProfileAuthHeaderFixed(auth, headerName) {
			continue
		}
		if value := trimHeaderValue(profile.headers, headerName); value != "" {
			codexSetHeaderCasePreserved(headers, headerName, value)
		}
	}
}

func codexPinnedClientProfileForAuth(auth *cliproxyauth.Auth) (codexClientProfile, bool) {
	key, ok := codexClientProfileKeyForAuth(auth)
	if !ok {
		return codexClientProfile{}, false
	}

	codexClientProfilesMu.RLock()
	profile, exists := codexClientProfiles[key]
	codexClientProfilesMu.RUnlock()
	if exists && len(profile.headers) > 0 {
		return codexCloneClientProfile(profile), true
	}

	legacyProfile, exists := codexLegacyClientProfileFromAuth(auth)
	if !exists {
		return codexClientProfile{}, false
	}

	codexClientProfilesMu.Lock()
	defer codexClientProfilesMu.Unlock()
	if profile, exists = codexClientProfiles[key]; exists {
		if len(profile.headers) > 0 {
			return codexCloneClientProfile(profile), true
		}
		delete(codexClientProfiles, key)
	}
	codexClientProfiles[key] = legacyProfile
	return codexCloneClientProfile(legacyProfile), true
}

func codexLegacyClientProfileFromAuth(auth *cliproxyauth.Auth) (codexClientProfile, bool) {
	if auth == nil || len(auth.Metadata) == 0 {
		return codexClientProfile{}, false
	}
	pinned, _ := auth.Metadata[codexClientProfilePinnedMetadataKey].(bool)
	if !pinned {
		return codexClientProfile{}, false
	}

	profile := codexClientProfile{headers: make(http.Header, len(codexPinnedClientProfileHeaders)+2)}
	if value := codexAuthUserAgent(auth); value != "" {
		codexSetHeaderCasePreserved(profile.headers, "User-Agent", value)
	}
	if value := codexAuthOriginator(auth); value != "" {
		codexSetHeaderCasePreserved(profile.headers, "Originator", value)
	}
	for _, headerName := range codexPinnedClientProfileHeaders {
		if value := codexAuthHeaderValue(auth, headerName); value != "" {
			codexSetHeaderCasePreserved(profile.headers, headerName, value)
		}
	}
	// Agent Identity imports can intentionally carry the pinned marker before
	// any client feature has been observed. That is not a usable profile yet:
	// treating it as one would permanently skip first-request collection and
	// leave management UI with only {"pinned": true}.
	if len(profile.headers) == 0 {
		return codexClientProfile{}, false
	}
	return profile, true
}

func codexCloneClientProfile(profile codexClientProfile) codexClientProfile {
	return codexClientProfile{headers: profile.headers.Clone()}
}

func codexClientProfileKeyForAuth(auth *cliproxyauth.Auth) (codexClientProfileKey, bool) {
	if auth == nil {
		return codexClientProfileKey{}, false
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return codexClientProfileKey{id: id}, true
	}
	return codexClientProfileKey{auth: auth}, true
}

func codexAllClientProfileHeaders() []string {
	headers := make([]string, 0, len(codexPinnedClientProfileHeaders)+2)
	headers = append(headers, "User-Agent", "Originator")
	headers = append(headers, codexPinnedClientProfileHeaders...)
	return headers
}

func codexPinnedClientProfileHeaderName(name string) bool {
	if strings.EqualFold(name, "User-Agent") || strings.EqualFold(name, "Originator") {
		return true
	}
	for _, headerName := range codexPinnedClientProfileHeaders {
		if strings.EqualFold(name, headerName) {
			return true
		}
	}
	return false
}

func codexClientProfileAuthHeaderFixed(auth *cliproxyauth.Auth, name string) bool {
	name = strings.TrimSpace(name)
	if auth == nil || name == "" {
		return false
	}
	if codexLegacyClientProfilePinned(auth) && codexPinnedClientProfileHeaderName(name) {
		return false
	}
	legacyGeneratedUserAgent := codexGeneratedDefaultUserAgent(codexAuthUserAgent(auth))
	switch {
	case strings.EqualFold(name, "User-Agent"):
		if legacyGeneratedUserAgent {
			return false
		}
	case strings.EqualFold(name, "Originator"):
		if codexGeneratedDefaultOriginator(auth, codexAuthOriginator(auth)) {
			return false
		}
	case strings.EqualFold(name, "Version"):
		if legacyGeneratedUserAgent {
			return false
		}
	}
	return codexAuthHeaderFixed(auth, name)
}

func codexClientProfileCustomHeaderAttrs(auth *cliproxyauth.Auth) map[string]string {
	if auth == nil || len(auth.Attributes) == 0 {
		return nil
	}
	if !codexClientProfilePinned(auth) {
		return auth.Attributes
	}
	filtered := make(map[string]string, len(auth.Attributes))
	for key, value := range auth.Attributes {
		headerName, ok := strings.CutPrefix(key, "header:")
		if ok && codexPinnedClientProfileHeaderName(headerName) && !codexClientProfileAuthHeaderFixed(auth, headerName) {
			continue
		}
		filtered[key] = value
	}
	return filtered
}

func codexResetClientProfilesForTest() {
	codexClientProfilesMu.Lock()
	defer codexClientProfilesMu.Unlock()
	codexClientProfiles = make(map[codexClientProfileKey]codexClientProfile)
}

func codexLegacyClientProfilePinned(auth *cliproxyauth.Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	pinned, _ := auth.Metadata[codexClientProfilePinnedMetadataKey].(bool)
	return pinned
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
	headers := make(map[string]any)
	switch existing := auth.Metadata["headers"].(type) {
	case map[string]any:
		for key, existingValue := range existing {
			headers[key] = existingValue
		}
	case map[string]string:
		for key, existingValue := range existing {
			if strings.TrimSpace(key) != "" && strings.TrimSpace(existingValue) != "" {
				headers[key] = strings.TrimSpace(existingValue)
			}
		}
	}
	// Auth.Clone intentionally only clones the metadata map's first level. Make
	// a fresh headers map before changing it so a published candidate cannot
	// mutate the execution snapshot (or the manager record) through a shared
	// nested map.
	auth.Metadata["headers"] = headers
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			delete(headers, key)
		}
	}
	headers[name] = value
}

func codexSetAuthProfileHeader(auth *cliproxyauth.Auth, name string, value string) {
	if auth == nil {
		return
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" || value == "" {
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
	auth.Attributes["header:"+name] = value
	codexSetAuthMetadataHeader(auth, name, value)
}
