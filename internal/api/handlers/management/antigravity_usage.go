package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const antigravityUsageCacheMaxAge = 10 * time.Minute

// GetAntigravityUsage returns the Google One AI credit bucket reported by
// Antigravity's loadCodeAssist control-plane endpoint. The response mirrors
// the Codex management shape and includes the current auth-file projection.
func (h *Handler) GetAntigravityUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	auth, status, message := h.resolveCodexUsageAuth(c)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"error": message})
		return
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file is not an Antigravity credential"})
		return
	}

	ctx := c.Request.Context()
	auth = h.refreshAntigravityUsageAuthIfNeeded(ctx, auth)
	refresh := antigravityUsageRefreshRequested(c)

	var snapshot runtimeexecutor.AntigravityCreditsSnapshot
	if !refresh {
		if hint, ok := coreauth.GetAntigravityCreditsHint(auth.ID); ok && antigravityCreditsHintFresh(hint) {
			snapshot = antigravityCreditsSnapshotFromHint(hint)
		}
	}
	if !snapshot.Known {
		cfg := readConfigValue(h, func(cfg *config.Config) *config.Config { return cfg })
		fresh, errFetch := runtimeexecutor.FetchAntigravityCredits(ctx, cfg, auth, "")
		if errFetch != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to fetch Antigravity credits: %v", errFetch)})
			return
		}
		snapshot = fresh
		coreauth.SetAntigravityCreditsHint(auth.ID, antigravityCreditsHintFromSnapshot(snapshot))
	}

	if manager := h.authManagerSnapshot(); manager != nil {
		if latest, ok := manager.GetByID(auth.ID); ok && latest != nil {
			auth = latest
		}
	}
	payload := gin.H{
		"credits":             antigravityCreditsPayload(snapshot),
		"antigravity_credits": antigravityCreditsPayload(snapshot),
	}
	if entry := h.buildAuthFileEntry(auth); entry != nil {
		payload["auth_file"] = entry
		payload["authFile"] = entry
	}
	c.JSON(http.StatusOK, payload)
}

func antigravityUsageRefreshRequested(c *gin.Context) bool {
	if c == nil {
		return false
	}
	mode := firstNonEmptyQueryValue(c, "antigravity_usage", "antigravityUsage", "mode")
	return isRefreshQueryValue(mode) || isRefreshQueryValue(c.Query("refresh"))
}

func antigravityCreditsHintFresh(hint coreauth.AntigravityCreditsHint) bool {
	if !hint.Known {
		return false
	}
	if hint.UpdatedAt.IsZero() {
		return false
	}
	return time.Since(hint.UpdatedAt) <= antigravityUsageCacheMaxAge
}

func antigravityCreditsSnapshotFromHint(hint coreauth.AntigravityCreditsHint) runtimeexecutor.AntigravityCreditsSnapshot {
	return runtimeexecutor.AntigravityCreditsSnapshot{
		Known:           hint.Known,
		Available:       hint.Available,
		CreditType:      hint.CreditType,
		CreditAmount:    hint.CreditAmount,
		MinCreditAmount: hint.MinCreditAmount,
		PaidTierID:      hint.PaidTierID,
		UpdatedAt:       hint.UpdatedAt,
	}
}

func antigravityCreditsHintFromSnapshot(snapshot runtimeexecutor.AntigravityCreditsSnapshot) coreauth.AntigravityCreditsHint {
	return coreauth.AntigravityCreditsHint{
		Known:           snapshot.Known,
		Available:       snapshot.Available,
		CreditType:      snapshot.CreditType,
		CreditAmount:    snapshot.CreditAmount,
		MinCreditAmount: snapshot.MinCreditAmount,
		PaidTierID:      snapshot.PaidTierID,
		UpdatedAt:       snapshot.UpdatedAt,
	}
}

func antigravityCreditsPayload(snapshot runtimeexecutor.AntigravityCreditsSnapshot) gin.H {
	payload := gin.H{
		"known":                           snapshot.Known,
		"available":                       snapshot.Available,
		"credit_amount":                   snapshot.CreditAmount,
		"minimum_credit_amount_for_usage": snapshot.MinCreditAmount,
		"minimumCreditAmountForUsage":     snapshot.MinCreditAmount,
	}
	if creditType := strings.TrimSpace(snapshot.CreditType); creditType != "" {
		payload["credit_type"] = creditType
		payload["creditType"] = creditType
	}
	if paidTierID := strings.TrimSpace(snapshot.PaidTierID); paidTierID != "" {
		payload["paid_tier_id"] = paidTierID
		payload["paidTierId"] = paidTierID
	}
	if !snapshot.UpdatedAt.IsZero() {
		payload["updated_at"] = snapshot.UpdatedAt
		payload["updatedAt"] = snapshot.UpdatedAt
	}
	return payload
}

func (h *Handler) refreshAntigravityUsageAuthIfNeeded(ctx context.Context, auth *coreauth.Auth) *coreauth.Auth {
	if h == nil || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		return auth
	}
	manager := h.authManagerSnapshot()
	if manager == nil || !authFileHasRefreshToken(auth) {
		return auth
	}

	accessToken := antigravityUsageAccessToken(auth)
	shouldRefresh := accessToken == ""
	if !shouldRefresh {
		if expiresAt, ok := auth.ExpirationTime(); ok && !expiresAt.IsZero() {
			shouldRefresh = time.Until(expiresAt) <= 2*time.Minute
		}
	}
	if !shouldRefresh {
		return auth
	}
	updated, errRefresh := manager.RefreshAuth(ctx, auth)
	if errRefresh != nil || updated == nil {
		return auth
	}
	return updated
}

func antigravityUsageAccessToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{"access_token", "accessToken"} {
		if auth.Metadata != nil {
			if token := strings.TrimSpace(valueAsString(auth.Metadata[key])); token != "" {
				return token
			}
		}
	}
	if auth.Attributes != nil {
		for _, key := range []string{"access_token", "accessToken"} {
			if token := strings.TrimSpace(auth.Attributes[key]); token != "" {
				return token
			}
		}
	}
	return ""
}

func antigravityCreditsEntry(auth *coreauth.Auth) gin.H {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		return nil
	}
	hint, ok := coreauth.GetAntigravityCreditsHint(auth.ID)
	if !ok || !hint.Known {
		return nil
	}
	return antigravityCreditsPayload(antigravityCreditsSnapshotFromHint(hint))
}
