package auth

import (
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
)

// ApplyCodexMetadataFromMetadata normalizes the minimal runtime fields needed
// by Codex auth records. It prefers explicit top-level metadata and only falls
// back to parsing id_token when those fields are absent.
func ApplyCodexMetadataFromMetadata(auth *Auth) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || len(auth.Metadata) == 0 {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}

	email := strings.TrimSpace(metadataString(auth.Metadata, "email"))
	accountID := strings.TrimSpace(metadataString(auth.Metadata, "account_id"))
	planType := strings.TrimSpace(metadataString(auth.Metadata, "plan_type"))

	if email == "" || accountID == "" || planType == "" {
		if claims := parseCodexMetadataIDToken(auth.Metadata); claims != nil {
			if email == "" {
				email = strings.TrimSpace(claims.GetUserEmail())
				if email != "" {
					auth.Metadata["email"] = email
				}
			}
			if accountID == "" {
				accountID = strings.TrimSpace(claims.GetAccountID())
				if accountID != "" {
					auth.Metadata["account_id"] = accountID
				}
			}
			if planType == "" {
				planType = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
				if planType != "" {
					auth.Metadata["plan_type"] = planType
				}
			}
		}
	}

	if email != "" {
		auth.Attributes["email"] = email
	}
	if accountID != "" {
		auth.Attributes["account_id"] = accountID
	}
	if planType != "" {
		auth.Attributes["plan_type"] = planType
	}
}

func parseCodexMetadataIDToken(metadata map[string]any) *codexauth.JWTClaims {
	if len(metadata) == 0 {
		return nil
	}
	idToken := strings.TrimSpace(metadataString(metadata, "id_token"))
	if idToken == "" {
		return nil
	}
	claims, err := codexauth.ParseJWTToken(idToken)
	if err != nil {
		return nil
	}
	return claims
}

func codexAccessTokenExpirationTime(auth *Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	token := strings.TrimSpace(metadataString(auth.Metadata, "access_token"))
	if token == "" {
		token = strings.TrimSpace(metadataString(auth.Metadata, "accessToken"))
	}
	if token == "" && auth.Attributes != nil {
		token = strings.TrimSpace(auth.Attributes["api_key"])
	}
	if token == "" {
		return time.Time{}, false
	}
	claims, err := codexauth.ParseJWTToken(token)
	if err != nil {
		return time.Time{}, false
	}
	return claims.ExpirationTime()
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return value
	default:
		return ""
	}
}
