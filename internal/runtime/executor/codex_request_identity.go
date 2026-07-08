package executor

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// codexUserAgent is the default User-Agent string used when no explicit
// client-, config-, or auth-file-provided value is available. It is built
// dynamically at startup by misc.BuildCodexUserAgent so the proxy emits a
// plausible fingerprint for the actual host OS/arch/terminal rather than a
// hard-coded Linux string.
var codexUserAgent = misc.CodexCLIUserAgent

const codexOriginator = misc.CodexCLIOriginator

type codexRequestIdentity struct {
	userAgent  string
	originator string
}

func codexIsAPIKeyAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	apiKey := strings.TrimSpace(auth.Attributes["api_key"])
	if apiKey == "" {
		return false
	}
	if authKind := codexAuthKindHint(auth); authKind != "" {
		switch authKind {
		case "apikey", "api_key":
			return true
		case "oauth", "chatgpt", "chatgpt_auth_tokens", "agent_identity":
			return false
		}
	}
	if accessToken := strings.TrimSpace(metadataString(auth.Metadata, "access_token", "accessToken")); accessToken != "" && (apiKey == accessToken || codexMetadataHasOAuthIdentity(auth.Metadata)) {
		return false
	}
	return true
}

func codexAuthKindHint(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if authKind := codexAuthKind(auth.Attributes["auth_kind"]); authKind != "" {
			return authKind
		}
	}
	return codexAuthKind(metadataString(auth.Metadata, "auth_kind", "authKind", "auth_mode", "authMode"))
}

func codexAuthKind(authKind string) string {
	authKind = strings.TrimSpace(authKind)
	if authKind == "" {
		return ""
	}
	switch {
	case strings.EqualFold(authKind, "apikey"):
		return "apikey"
	case strings.EqualFold(authKind, "api_key"):
		return "api_key"
	case strings.EqualFold(authKind, "oauth"):
		return "oauth"
	case strings.EqualFold(authKind, "chatgpt"):
		return "chatgpt"
	case strings.EqualFold(authKind, "chatgpt_auth_tokens"):
		return "chatgpt_auth_tokens"
	case strings.EqualFold(authKind, "agent_identity"):
		return "agent_identity"
	default:
		return strings.ToLower(authKind)
	}
}

func codexMetadataHasOAuthIdentity(metadata map[string]any) bool {
	return metadataString(
		metadata,
		"account_id",
		"accountId",
		"chatgpt_account_id",
		"chatgptAccountId",
		"email",
		"id_token",
		"idToken",
		"refresh_token",
		"refreshToken",
	) != ""
}

func codexResolvedUserAgent(target http.Header, source http.Header, auth *cliproxyauth.Auth, cfg *config.Config) string {
	return codexResolvedIdentity(target, source, auth, cfg).userAgent
}

func codexResolvedOriginator(target http.Header, source http.Header, auth *cliproxyauth.Auth) string {
	return codexResolvedIdentity(target, source, auth, nil).originator
}

func codexResolvedIdentity(target http.Header, source http.Header, auth *cliproxyauth.Auth, cfg *config.Config) codexRequestIdentity {
	userAgent, _ := codexHeaderDefaults(cfg, auth)
	return codexIdentity(target, source, auth, userAgent)
}

func codexIdentity(target http.Header, source http.Header, auth *cliproxyauth.Auth, userAgent string) codexRequestIdentity {
	identity := codexRequestIdentity{
		originator: codexResolvedOriginatorValue(target, source, auth),
	}
	userAgent = strings.TrimSpace(userAgent)
	authUserAgent := codexAuthUserAgent(auth)
	switch {
	case userAgent != "":
		identity.userAgent = userAgent
	case authUserAgent != "":
		identity.userAgent = authUserAgent
	default:
		if targetUserAgent := trimHeaderValue(target, "User-Agent"); targetUserAgent != "" {
			identity.userAgent = targetUserAgent
		} else if sourceUserAgent := trimHeaderValue(source, "User-Agent"); sourceUserAgent != "" {
			identity.userAgent = sourceUserAgent
		} else {
			identity.userAgent = misc.CodexCLIUserAgentWithOriginator(identity.originator)
		}
	}
	return identity
}

func codexResolvedOriginatorValue(target http.Header, source http.Header, auth *cliproxyauth.Auth) string {
	if authOriginator := codexAuthOriginator(auth); authOriginator != "" {
		return authOriginator
	}
	if originator := trimHeaderValue(target, "Originator"); originator != "" {
		return originator
	}
	if originator := trimHeaderValue(source, "Originator"); originator != "" {
		return originator
	}
	return codexOriginator
}

func codexAuthOriginator(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		for _, key := range []string{"header:Originator", "originator"} {
			if originator := strings.TrimSpace(auth.Attributes[key]); originator != "" {
				return originator
			}
		}
	}
	if auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{"originator", "Originator"} {
		if originator, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(originator) != "" {
			return strings.TrimSpace(originator)
		}
	}
	return ""
}

type codexSessionHeaderOptions struct {
	includeRequestID bool
}

func codexEnsureSessionHeaders(target http.Header, source http.Header, auth *cliproxyauth.Auth, opts codexSessionHeaderOptions) string {
	if target == nil {
		return ""
	}
	conversationID := firstNonEmptyHeaderValue(target, source, "Conversation_id")
	threadID := firstNonEmptyHeaderValue(target, source, codexHeaderThreadID)
	if threadID == "" {
		threadID = firstNonEmptyHeaderValue(target, source, codexHeaderOfficialThreadID)
	}
	if threadID == "" {
		threadID = firstNonEmptyHeaderValue(target, source, "X-Thread-ID")
	}
	if threadID == "" {
		threadID = conversationID
	}
	sessionID := firstNonEmptyHeaderValue(target, source, "Session_id")
	if sessionID == "" {
		sessionID = firstNonEmptyHeaderValue(target, source, codexHeaderOfficialSessionID)
	}
	if sessionID == "" {
		sessionID = firstNonEmptyHeaderValue(target, source, "X-Session-ID")
	}
	if sessionID == "" {
		sessionID = conversationID
	}
	if sessionID == "" {
		sessionID = threadID
	}
	if sessionID == "" {
		sessionID = codexTurnMetadataSessionID(target, source)
	}
	if sessionID == "" {
		if apiKey, _ := codexCreds(auth); strings.TrimSpace(apiKey) != "" {
			sessionID = helps.CachedSessionID(apiKey)
		} else {
			sessionID = uuid.NewString()
		}
	}
	codexSetSingleHeaderValue(target, "Session_id", sessionID)
	codexSetSingleHeaderValue(target, codexHeaderOfficialSessionID, sessionID)
	if threadID == "" {
		threadID = sessionID
	}
	if threadID != "" {
		codexSetSingleHeaderValue(target, codexHeaderThreadID, threadID)
		codexSetSingleHeaderValue(target, codexHeaderOfficialThreadID, threadID)
	}

	requestID := firstNonEmptyHeaderValue(target, source, "X-Client-Request-Id")
	if opts.includeRequestID && requestID == "" {
		requestID = threadID
	}
	if opts.includeRequestID && requestID == "" {
		requestID = conversationID
	}
	if opts.includeRequestID && requestID == "" {
		requestID = sessionID
	}
	if requestID != "" {
		codexSetSingleHeaderValue(target, "X-Client-Request-Id", requestID)
	} else {
		target.Del("X-Client-Request-Id")
	}
	target.Del("Conversation_id")
	return sessionID
}

func firstNonEmptyHeaderValue(target http.Header, source http.Header, key string) string {
	if target != nil {
		if value := strings.TrimSpace(target.Get(key)); value != "" {
			return value
		}
	}
	if source != nil {
		if value := strings.TrimSpace(source.Get(key)); value != "" {
			return value
		}
	}
	return ""
}
