package executor

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config) {
	headers := r.Header
	headers.Set("Content-Type", "application/json")
	if token = strings.TrimSpace(token); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	} else {
		headers.Del("Authorization")
	}
	apiKeyAuth := codexIsAPIKeyAuth(auth)
	requestKind := codexFinalUpstreamResponses
	if r.URL != nil {
		requestKind = codexFinalUpstreamRequestKindForURL(r.URL.String())
	}

	ginHeaders := codexGinHeadersFromContext(r.Context())
	cfgUserAgent, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, ginHeaders, "X-Codex-Beta-Features", cfgBetaFeatures, "")
	codexEnsureVersionHeader(headers, ginHeaders)
	misc.EnsureHeader(headers, ginHeaders, "X-OpenAI-Subagent", "")
	misc.EnsureHeader(headers, ginHeaders, codexHeaderOAIAttestation, "")
	misc.EnsureHeader(headers, ginHeaders, "Traceparent", "")
	misc.EnsureHeader(headers, ginHeaders, "Tracestate", "")
	identity := codexResolvedIdentity(headers, ginHeaders, auth, cfg)
	headers.Set("User-Agent", identity.userAgent)
	sessionID := codexEnsureSessionHeaders(headers, ginHeaders, auth, codexSessionHeaderOptions{
		includeRequestID: requestKind != codexFinalUpstreamCompact,
	})
	codexEnsureResponsesIdentityHeaders(headers, ginHeaders)
	if requestKind == codexFinalUpstreamCompact {
		codexEnsureCompactTurnMetadataHeader(headers, ginHeaders, codexTurnMetadataDefaults{
			sessionID: sessionID,
			threadID:  strings.TrimSpace(headers.Get(codexHeaderThreadID)),
			turnID:    uuid.NewString(),
			sandbox:   codexDefaultSandboxTag,
			windowID:  strings.TrimSpace(headers.Get(codexHeaderWindowID)),
		})
		misc.EnsureHeader(headers, ginHeaders, codexHeaderTurnState, "")
	} else {
		codexEnsureTurnMetadataHeader(headers, ginHeaders, codexTurnMetadataDefaults{
			requestKind: codexTurnRequestKind,
			sessionID:   sessionID,
			threadID:    strings.TrimSpace(headers.Get(codexHeaderThreadID)),
			turnID:      uuid.NewString(),
			sandbox:     codexDefaultSandboxTag,
			windowID:    strings.TrimSpace(headers.Get(codexHeaderWindowID)),
		})
		misc.EnsureHeader(headers, ginHeaders, codexHeaderTurnState, "")
	}

	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}

	headers.Set("Originator", identity.originator)
	// Residency precedence: inbound gin header > cfg default. Avoid the
	// unnecessary target re-check from the previous implementation; we always
	// enter this block with a freshly applied `Originator` and never set the
	// residency header earlier, so target.Get is guaranteed empty here.
	if residency := trimHeaderValue(ginHeaders, misc.CodexResidencyHeader); residency != "" {
		headers.Set(misc.CodexResidencyHeader, residency)
	} else if residency := codexResidencyFor(cfg); residency != "" {
		headers.Set(misc.CodexResidencyHeader, residency)
	}
	if !apiKeyAuth && auth != nil && auth.Metadata != nil {
		if accountID, ok := auth.Metadata["account_id"].(string); ok {
			if trimmed := strings.TrimSpace(accountID); trimmed != "" {
				headers.Set("Chatgpt-Account-Id", trimmed)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	if cfgUserAgent != "" {
		headers.Set("User-Agent", cfgUserAgent)
	}
}

// trimHeaderValue returns the TrimSpace'd value for a header key without
// panicking on a nil http.Header. Having this helper avoids the
// `if h != nil { strings.TrimSpace(h.Get(k)) }` pattern repeated across the
// Codex request preparation code.
func trimHeaderValue(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	return strings.TrimSpace(h.Get(key))
}

func codexDefaultVersionHeader() string {
	return misc.CodexCLIVersion
}

func codexEnsureVersionHeader(target http.Header, source http.Header) {
	if target == nil {
		return
	}
	version := trimHeaderValue(source, "Version")
	if version == "" {
		version = trimHeaderValue(target, "Version")
	}
	if codexVersionAtLeast(version, codexDefaultVersionHeader()) {
		target.Set("Version", version)
		return
	}
	target.Set("Version", codexDefaultVersionHeader())
}

type codexParsedVersion struct {
	major      int
	minor      int
	patch      int
	prerelease string
}

func codexVersionAtLeast(version, minimum string) bool {
	cmp, ok := codexCompareVersions(version, minimum)
	return ok && cmp >= 0
}

func codexCompareVersions(left, right string) (int, bool) {
	leftVersion, okLeft := codexParseVersion(left)
	rightVersion, okRight := codexParseVersion(right)
	if !okLeft || !okRight {
		return 0, false
	}
	switch {
	case leftVersion.major != rightVersion.major:
		return codexCompareInt(leftVersion.major, rightVersion.major), true
	case leftVersion.minor != rightVersion.minor:
		return codexCompareInt(leftVersion.minor, rightVersion.minor), true
	case leftVersion.patch != rightVersion.patch:
		return codexCompareInt(leftVersion.patch, rightVersion.patch), true
	case leftVersion.prerelease != "" && rightVersion.prerelease == "":
		return -1, true
	case leftVersion.prerelease == "" && rightVersion.prerelease != "":
		return 1, true
	default:
		return codexComparePrerelease(leftVersion.prerelease, rightVersion.prerelease), true
	}
}

func codexParseVersion(version string) (codexParsedVersion, bool) {
	version = strings.TrimSpace(version)
	if version == "" {
		return codexParsedVersion{}, false
	}
	if idx := strings.IndexByte(version, '+'); idx >= 0 {
		version = version[:idx]
	}
	parsed := codexParsedVersion{}
	if idx := strings.IndexByte(version, '-'); idx >= 0 {
		parsed.prerelease = version[idx+1:]
		if parsed.prerelease == "" {
			return codexParsedVersion{}, false
		}
		version = version[:idx]
	}
	majorText, rest, ok := strings.Cut(version, ".")
	if !ok {
		return codexParsedVersion{}, false
	}
	minorText, patchText, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(patchText, ".") {
		return codexParsedVersion{}, false
	}

	major, ok := codexParseVersionPart(majorText)
	if !ok {
		return codexParsedVersion{}, false
	}
	minor, ok := codexParseVersionPart(minorText)
	if !ok {
		return codexParsedVersion{}, false
	}
	patch, ok := codexParseVersionPart(patchText)
	if !ok {
		return codexParsedVersion{}, false
	}
	parsed.major = major
	parsed.minor = minor
	parsed.patch = patch
	return parsed, true
}

func codexParseVersionPart(part string) (int, bool) {
	if part == "" {
		return 0, false
	}
	value, err := strconv.Atoi(part)
	return value, err == nil
}

func codexComparePrerelease(left, right string) int {
	for {
		leftPart, leftRest, leftHasRest := strings.Cut(left, ".")
		rightPart, rightRest, rightHasRest := strings.Cut(right, ".")
		if cmp := codexComparePrereleasePart(leftPart, rightPart); cmp != 0 {
			return cmp
		}
		switch {
		case leftHasRest && rightHasRest:
			left = leftRest
			right = rightRest
		case leftHasRest:
			return 1
		case rightHasRest:
			return -1
		default:
			return 0
		}
	}
}

func codexComparePrereleasePart(left, right string) int {
	leftNumber, leftNumeric := codexParseVersionPart(left)
	rightNumber, rightNumeric := codexParseVersionPart(right)
	switch {
	case leftNumeric && rightNumeric:
		return codexCompareInt(leftNumber, rightNumber)
	case leftNumeric:
		return -1
	case rightNumeric:
		return 1
	case left > right:
		return 1
	case left < right:
		return -1
	default:
		return 0
	}
}

func codexCompareInt(left, right int) int {
	switch {
	case left > right:
		return 1
	case left < right:
		return -1
	default:
		return 0
	}
}

// codexOriginatorFor resolves the originator value for the given config,
// honouring config > env > built-in default.
func codexOriginatorFor(cfg *config.Config) string {
	configured := ""
	if cfg != nil {
		configured = cfg.CodexHeaderDefaults.Originator
	}
	return misc.ResolveCodexOriginator(configured)
}

// codexResidencyFor resolves the residency header value; empty means "do not
// send" (matches codex-rs behaviour).
func codexResidencyFor(cfg *config.Config) string {
	configured := ""
	if cfg != nil {
		configured = cfg.CodexHeaderDefaults.Residency
	}
	return misc.ResolveCodexResidency(configured)
}

func codexAuthUserAgent(auth *cliproxyauth.Auth) string {
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
	if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
		return strings.TrimSpace(ua)
	}
	if ua, ok := auth.Metadata["user-agent"].(string); ok && strings.TrimSpace(ua) != "" {
		return strings.TrimSpace(ua)
	}
	return ""
}
