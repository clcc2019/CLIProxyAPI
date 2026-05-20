package executor

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config) {
	headers := r.Header
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Connection", "Keep-Alive")
	apiKeyAuth := codexIsAPIKeyAuth(auth)
	requestKind := codexFinalUpstreamResponses
	if r.URL != nil {
		requestKind = codexFinalUpstreamRequestKindForURL(r.URL.String())
	}

	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	cfgUserAgent, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, ginHeaders, "X-Codex-Beta-Features", cfgBetaFeatures, "")
	misc.EnsureHeader(headers, ginHeaders, "Version", codexDefaultVersionHeader())
	misc.EnsureHeader(headers, ginHeaders, "X-OpenAI-Subagent", "")
	misc.EnsureHeader(headers, ginHeaders, "Traceparent", "")
	misc.EnsureHeader(headers, ginHeaders, "Tracestate", "")
	identity := codexResolvedIdentity(headers, ginHeaders, auth, cfg)
	headers.Set("User-Agent", identity.userAgent)
	sessionID := codexEnsureSessionHeaders(headers, ginHeaders, auth, codexSessionHeaderOptions{
		includeRequestID: requestKind != codexFinalUpstreamCompact,
	})
	if requestKind == codexFinalUpstreamCompact {
		misc.EnsureHeader(headers, ginHeaders, codexHeaderTurnMetadata, "")
		misc.EnsureHeader(headers, ginHeaders, codexHeaderTurnState, "")
	} else {
		codexEnsureTurnMetadataHeader(headers, ginHeaders, codexTurnMetadataDefaults{
			sessionID:    sessionID,
			threadSource: codexDefaultThreadSource,
			turnID:       uuid.NewString(),
			sandbox:      codexDefaultSandboxTag,
		})
		misc.EnsureHeader(headers, ginHeaders, codexHeaderTurnState, "")
	}
	codexEnsureResponsesIdentityHeaders(headers, ginHeaders)

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
	return strings.TrimSpace(buildinfo.Version)
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
