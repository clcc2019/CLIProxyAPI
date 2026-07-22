package auth

import "strings"

const (
	CodexAuthKindOAuth         = "oauth"
	CodexAuthKindAgentIdentity = "agent_identity"
)

// CodexAuthKind resolves the active Codex authentication kind. An explicit
// auth_kind is authoritative so an auth file can retain inactive Agent
// Identity material while using its access token.
func CodexAuthKind(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if kind := normalizeCodexAuthKind(codexAuthMetadataString(auth.Metadata, "auth_kind", "authKind", "auth_mode", "authMode")); kind != "" {
		return kind
	}
	if auth.Attributes != nil {
		if kind := normalizeCodexAuthKind(auth.Attributes["auth_kind"]); kind != "" {
			return kind
		}
	}
	if CodexAgentIdentityAvailable(auth) {
		return CodexAuthKindAgentIdentity
	}
	if CodexAccessTokenAvailable(auth) {
		return CodexAuthKindOAuth
	}
	return ""
}

// CodexAuthUsesAgentIdentity reports whether Agent Identity is the active mode.
func CodexAuthUsesAgentIdentity(auth *Auth) bool {
	return CodexAuthKind(auth) == CodexAuthKindAgentIdentity
}

// CodexAgentIdentityAvailable reports whether reusable Agent Identity signing
// material is present, independently of the currently active auth kind.
func CodexAgentIdentityAvailable(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.managementSummaryHasAgentIdentity {
		return true
	}
	return codexAuthMetadataString(auth.Metadata, "agent_runtime_id", "agentRuntimeId", "agentRuntimeID") != "" &&
		codexAuthMetadataString(auth.Metadata, "agent_private_key", "agentPrivateKey") != ""
}

// CodexAccessTokenAvailable reports whether the auth contains a reusable
// ChatGPT access token, independently of the currently active auth kind.
func CodexAccessTokenAvailable(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.managementSummaryHasAccessToken {
		return true
	}
	if codexAuthMetadataString(auth.Metadata, "access_token", "accessToken") != "" {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["access_token"]) != ""
}

func normalizeCodexAuthKind(kind string) string {
	kind = strings.TrimSpace(kind)
	switch {
	case kind == "":
		return ""
	case strings.EqualFold(kind, "agent_identity"), strings.EqualFold(kind, "agentIdentity"):
		return CodexAuthKindAgentIdentity
	case strings.EqualFold(kind, "access_token"),
		strings.EqualFold(kind, "accessToken"),
		strings.EqualFold(kind, "oauth"),
		strings.EqualFold(kind, "chatgpt"),
		strings.EqualFold(kind, "chatgpt_auth_tokens"):
		return CodexAuthKindOAuth
	case strings.EqualFold(kind, "apikey"):
		return "apikey"
	case strings.EqualFold(kind, "api_key"):
		return "api_key"
	default:
		return strings.ToLower(kind)
	}
}

func codexAuthMetadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	for _, containerKey := range []string{"token", "tokens", "token_data", "tokenData"} {
		if nested, ok := metadata[containerKey].(map[string]any); ok {
			if value := codexAuthMetadataString(nested, keys...); value != "" {
				return value
			}
		}
	}
	return ""
}
