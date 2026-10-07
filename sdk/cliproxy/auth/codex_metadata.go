package auth

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
)

const codexSubscriptionExpirySkew = time.Hour

// ApplyCodexMetadataFromMetadata normalizes the minimal runtime fields needed
// by Codex auth records. It prefers explicit top-level metadata, falls back to
// id_token claims when those fields are absent, and derives the effective model
// plan from the subscription end time when a paid plan has expired.
func ApplyCodexMetadataFromMetadata(auth *Auth) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || len(auth.Metadata) == 0 {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}

	email := strings.TrimSpace(metadataString(auth.Metadata, "email"))
	accountID := strings.TrimSpace(metadataString(auth.Metadata, "account_id"))
	planType := strings.TrimSpace(firstMetadataString(auth.Metadata, "plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"))
	authKind := strings.TrimSpace(metadataString(auth.Metadata, "auth_kind"))
	claims := parseCodexMetadataIDToken(auth.Metadata)

	if claims != nil {
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
			// JWTs can retain an empty plan claim. Codex's auth parser treats
			// that case as free so model registration cannot fall through to
			// the paid default catalog.
			planType = strings.TrimSpace(claims.GetPlanType())
			if planType != "" {
				auth.Metadata["plan_type"] = planType
			}
		}
	} else if planType == "" && strings.TrimSpace(metadataString(auth.Metadata, "id_token")) != "" {
		// Match the file synthesizer behavior for a malformed or otherwise
		// unreadable id_token: an unknown token plan is treated conservatively
		// as free until a valid token or explicit plan is available.
		planType = codexauth.DefaultPlanType
		auth.Metadata["plan_type"] = planType
	}

	if email != "" {
		auth.Attributes["email"] = email
	}
	if accountID != "" {
		auth.Attributes["account_id"] = accountID
	}
	if planType != "" {
		if isPaidCodexPlan(planType) {
			if activeUntil, ok := codexSubscriptionActiveUntil(auth.Metadata, claims); ok && time.Now().After(activeUntil.Add(codexSubscriptionExpirySkew)) {
				planType = "free"
			}
		}
		auth.Attributes["plan_type"] = planType
	}
	if authKind != "" {
		auth.Attributes["auth_kind"] = authKind
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

func isPaidCodexPlan(planType string) bool {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "plus", "team", "business", "go", "pro":
		return true
	default:
		return false
	}
}

func codexSubscriptionActiveUntil(metadata map[string]any, claims *codexauth.JWTClaims) (time.Time, bool) {
	if claims != nil {
		if activeUntil, ok := parseCodexSubscriptionActiveUntil(claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil); ok {
			return activeUntil, true
		}
	}

	for _, key := range []string{
		"chatgpt_subscription_active_until",
		"chatgptSubscriptionActiveUntil",
		"subscription_expires_at",
		"subscriptionExpiresAt",
		"subscription_active_until",
		"subscriptionActiveUntil",
	} {
		if value, ok := metadata[key]; ok {
			if activeUntil, parsed := parseCodexSubscriptionActiveUntil(value); parsed {
				return activeUntil, true
			}
		}
	}

	for _, containerKey := range []string{"account", "entitlement", "subscription", "providerSpecificData"} {
		container, ok := metadata[containerKey].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{
			"chatgpt_subscription_active_until",
			"chatgptSubscriptionActiveUntil",
			"subscription_expires_at",
			"subscriptionExpiresAt",
			"subscription_active_until",
			"subscriptionActiveUntil",
			"expires_at",
			"expiresAt",
			"current_period_end",
			"currentPeriodEnd",
			"period_end",
			"periodEnd",
		} {
			if value, ok := container[key]; ok {
				if activeUntil, parsed := parseCodexSubscriptionActiveUntil(value); parsed {
					return activeUntil, true
				}
			}
		}
	}
	return time.Time{}, false
}

func parseCodexSubscriptionActiveUntil(value any) (time.Time, bool) {
	switch value := value.(type) {
	case time.Time:
		if !value.IsZero() {
			return value.UTC(), true
		}
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return time.Time{}, false
		}
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.999999999",
			"2006-01-02T15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.UTC(), true
			}
		}
		if numeric, err := strconv.ParseFloat(value, 64); err == nil {
			return parseCodexUnixTimestamp(numeric)
		}
	case json.Number:
		numeric, err := value.Float64()
		if err == nil {
			return parseCodexUnixTimestamp(numeric)
		}
	case float64:
		return parseCodexUnixTimestamp(value)
	case float32:
		return parseCodexUnixTimestamp(float64(value))
	case int:
		return parseCodexUnixTimestamp(float64(value))
	case int8:
		return parseCodexUnixTimestamp(float64(value))
	case int16:
		return parseCodexUnixTimestamp(float64(value))
	case int32:
		return parseCodexUnixTimestamp(float64(value))
	case int64:
		return parseCodexUnixTimestamp(float64(value))
	case uint:
		return parseCodexUnixTimestamp(float64(value))
	case uint8:
		return parseCodexUnixTimestamp(float64(value))
	case uint16:
		return parseCodexUnixTimestamp(float64(value))
	case uint32:
		return parseCodexUnixTimestamp(float64(value))
	case uint64:
		return parseCodexUnixTimestamp(float64(value))
	}
	return time.Time{}, false
}

func parseCodexUnixTimestamp(value float64) (time.Time, bool) {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}, false
	}
	if value > 1e11 {
		value /= 1000
	}
	return time.Unix(int64(value), 0).UTC(), true
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
