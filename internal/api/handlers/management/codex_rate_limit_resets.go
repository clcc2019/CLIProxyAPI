package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var codexRateLimitResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
var codexRateLimitResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

type codexRateLimitResetConsumeResponse struct {
	Code         string `json:"code"`
	WindowsReset int    `json:"windows_reset"`
}

// GetCodexRateLimitResetCredits returns the reset credit balance embedded in
// the official Codex /wham/usage response.
func (h *Handler) GetCodexRateLimitResetCredits(c *gin.Context) {
	auth, ok := h.resolveCodexRateLimitResetAuth(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	auth = h.refreshCodexUsageAuthIfNeeded(ctx, auth)
	type detailsResult struct {
		payload gin.H
		err     error
	}
	detailsCh := make(chan detailsResult, 1)
	go func(authSnapshot *coreauth.Auth) {
		details, _, detailsErr := h.fetchCodexRateLimitResetCreditDetails(ctx, authSnapshot)
		detailsCh <- detailsResult{payload: details, err: detailsErr}
	}(auth)
	usageOpts := parseCodexUsageRequestOptions(c)
	payload, upstreamStatus, err := h.fetchCodexUsageWithCache(ctx, auth, usageOpts)
	if err != nil {
		if codexUsageTransientFailure(upstreamStatus, err) {
			payload = codexUsageUnavailablePayload(err, upstreamStatus)
		} else {
			if upstreamStatus > 0 {
				c.JSON(upstreamStatus, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
	}
	// /wham/usage only contains the balance. The official client additionally
	// reads the detail endpoint to obtain each reset credit's expires_at value.
	// Keep this best-effort so a detail outage does not hide the known balance.
	details := <-detailsCh
	if details.err == nil {
		mergeCodexRateLimitResetCreditDetails(payload, details.payload)
	} else {
		log.WithError(details.err).WithField("auth_id", auth.ID).Debug("failed to fetch codex rate limit reset credit details")
	}
	c.JSON(http.StatusOK, h.codexRateLimitResetCreditsPayload(auth, payload))
}

func mergeCodexRateLimitResetCreditDetails(usage gin.H, details gin.H) {
	if usage == nil || len(details) == 0 {
		return
	}
	merged := gin.H{}
	if existing, ok := codexUsageWindowMap(usage["rate_limit_reset_credits"]); ok {
		for key, value := range existing {
			merged[key] = value
		}
	}
	for key, value := range details {
		if key == "available_count" {
			if _, ok := numberFromAny(value); !ok {
				continue
			}
		}
		merged[key] = value
	}
	if len(merged) > 0 {
		usage["rate_limit_reset_credits"] = merged
	}
}

func (h *Handler) fetchCodexRateLimitResetCreditDetails(ctx context.Context, auth *coreauth.Auth) (gin.H, int, error) {
	accessToken := codexUsageAccessToken(auth)
	if accessToken == "" {
		return nil, 0, fmt.Errorf("codex access_token missing")
	}
	accountID := resolveCodexUsageAccountID(auth, accessToken)
	if accountID == "" {
		return nil, 0, fmt.Errorf("codex chatgpt account id missing")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := h.acquireCodexUpstreamSlot(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer release()

	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, codexRateLimitResetCreditsURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codexUsageRequestUserAgent(h, auth))
	if codexUsageFedramp(auth) {
		req.Header.Set("X-OpenAI-Fedramp", "true")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	if h != nil {
		client.Transport = h.codexUsageTransport(auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.StatusCode, fmt.Errorf("codex reset credit details request failed with status %d: %s", resp.StatusCode, truncateForLog(string(body), 200))
	}
	details := gin.H{}
	if err = json.Unmarshal(body, &details); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to decode codex reset credit details response: %w", err)
	}
	return details, resp.StatusCode, nil
}

// ConsumeCodexRateLimitResetCredit redeems one Codex reset credit through the
// same endpoint used by the official client.
func (h *Handler) ConsumeCodexRateLimitResetCredit(c *gin.Context) {
	auth, ok := h.resolveCodexRateLimitResetAuth(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	auth = h.refreshCodexUsageAuthIfNeeded(ctx, auth)
	redeemRequestID, err := codexRateLimitResetRedeemRequestID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	consume, upstreamStatus, err := h.consumeCodexRateLimitResetCredit(ctx, auth, redeemRequestID)
	if err != nil {
		if upstreamStatus > 0 {
			c.JSON(upstreamStatus, gin.H{"error": err.Error(), "redeem_request_id": redeemRequestID})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "redeem_request_id": redeemRequestID})
		return
	}

	response := gin.H{
		"consume":           consume,
		"code":              consume.Code,
		"windows_reset":     consume.WindowsReset,
		"redeem_request_id": redeemRequestID,
	}
	if manager := h.authManagerSnapshot(); codexRateLimitResetConsumeClearsCooldown(consume.Code) && manager != nil {
		response["local_quota_cooldown_cleared"] = manager.ClearAuthQuotaCooldown(ctx, auth.ID)
	}

	if codexRateLimitResetConsumeRefreshesUsage(consume.Code) {
		if usage, refreshStatus, refreshErr := h.fetchCodexUsageWithCache(ctx, auth, codexUsageRequestOptions{force: true, ttl: codexUsageCacheDefaultTTL}); refreshErr == nil {
			creditsPayload := h.codexRateLimitResetCreditsPayload(auth, usage)
			response["rate_limit_reset_credits"] = creditsPayload["rate_limit_reset_credits"]
			response["available_count"] = creditsPayload["available_count"]
			response["auth_file"] = creditsPayload["auth_file"]
			response["authFile"] = creditsPayload["authFile"]
		} else {
			if codexUsageTransientFailure(refreshStatus, refreshErr) {
				response["usage_refresh_error"] = codexUsageFailureMessage(refreshStatus)
				if refreshStatus > 0 {
					response["codex_usage_upstream_status"] = refreshStatus
				}
			} else {
				response["usage_refresh_error"] = refreshErr.Error()
			}
		}
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) resolveCodexRateLimitResetAuth(c *gin.Context) (*coreauth.Auth, bool) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return nil, false
	}
	auth, status, message := h.resolveCodexUsageAuth(c)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file is not a Codex credential"})
		return nil, false
	}
	return auth, true
}

func (h *Handler) codexRateLimitResetCreditsPayload(auth *coreauth.Auth, usage gin.H) gin.H {
	credits, available := codexRateLimitResetCreditsFromUsage(usage)
	payload := gin.H{
		"rate_limit_reset_credits": credits,
		"available_count":          available,
	}
	if manager := h.authManagerSnapshot(); manager != nil && auth != nil {
		if latest, ok := manager.GetByID(auth.ID); ok && latest != nil {
			auth = latest
		}
	}
	if h != nil {
		if entry := h.buildAuthFileEntry(auth); entry != nil {
			payload["auth_file"] = entry
			payload["authFile"] = entry
		}
	}
	for _, key := range []string{
		"codex_usage_unavailable",
		"codex_usage_stale",
		"codex_usage_cache",
		"codex_usage_upstream_status",
		"codex_usage_error",
	} {
		if value, ok := usage[key]; ok {
			payload[key] = value
		}
	}
	return payload
}

func codexRateLimitResetCreditsFromUsage(usage gin.H) (gin.H, int64) {
	credits := gin.H{"available_count": int64(0)}
	if len(usage) == 0 {
		return credits, 0
	}
	raw, ok := usage["rate_limit_reset_credits"]
	if !ok {
		return credits, 0
	}
	switch typed := raw.(type) {
	case gin.H:
		credits = cloneGinH(typed)
	case map[string]any:
		credits = cloneGinH(gin.H(typed))
	default:
		return credits, 0
	}
	available, _ := numberFromAny(credits["available_count"])
	count := int64(available)
	credits["available_count"] = count
	return credits, count
}

func codexRateLimitResetRedeemRequestID(c *gin.Context) (string, error) {
	var body struct {
		RedeemRequestID string `json:"redeem_request_id"`
		IdempotencyKey  string `json:"idempotency_key"`
	}
	if c != nil && c.Request != nil && c.Request.Body != nil {
		decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20))
		if err := decoder.Decode(&body); err != nil && err != io.EOF {
			return "", fmt.Errorf("invalid JSON body: %w", err)
		}
	}
	id := strings.TrimSpace(body.RedeemRequestID)
	if id == "" {
		id = strings.TrimSpace(body.IdempotencyKey)
	}
	if id == "" {
		id = uuid.NewString()
	}
	return id, nil
}

func (h *Handler) consumeCodexRateLimitResetCredit(ctx context.Context, auth *coreauth.Auth, redeemRequestID string) (codexRateLimitResetConsumeResponse, int, error) {
	accessToken := codexUsageAccessToken(auth)
	if accessToken == "" {
		return codexRateLimitResetConsumeResponse{}, 0, fmt.Errorf("codex access_token missing")
	}
	accountID := resolveCodexUsageAccountID(auth, accessToken)
	if accountID == "" {
		return codexRateLimitResetConsumeResponse{}, 0, fmt.Errorf("codex chatgpt account id missing")
	}
	if strings.TrimSpace(redeemRequestID) == "" {
		return codexRateLimitResetConsumeResponse{}, 0, fmt.Errorf("redeem_request_id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := h.acquireCodexUpstreamSlot(ctx)
	if err != nil {
		return codexRateLimitResetConsumeResponse{}, 0, err
	}
	defer release()

	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 20 * time.Second}
	if h != nil {
		client.Transport = h.codexUsageTransport(auth)
	}
	body, _ := json.Marshal(gin.H{"redeem_request_id": redeemRequestID})
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, codexRateLimitResetCreditsConsumeURL, bytes.NewReader(body))
	if err != nil {
		return codexRateLimitResetConsumeResponse{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexUsageRequestUserAgent(h, auth))
	if codexUsageFedramp(auth) {
		req.Header.Set("X-OpenAI-Fedramp", "true")
	}
	resp, err := client.Do(req)
	if err != nil {
		return codexRateLimitResetConsumeResponse{}, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return codexRateLimitResetConsumeResponse{}, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return codexRateLimitResetConsumeResponse{}, resp.StatusCode, fmt.Errorf("codex rate-limit reset consume request failed with status %d: %s", resp.StatusCode, truncateForLog(string(respBody), 200))
	}
	var consume codexRateLimitResetConsumeResponse
	if err := json.Unmarshal(respBody, &consume); err != nil {
		return codexRateLimitResetConsumeResponse{}, resp.StatusCode, fmt.Errorf("failed to decode codex rate-limit reset consume response: %w", err)
	}
	consume.Code = strings.TrimSpace(consume.Code)
	return consume, resp.StatusCode, nil
}

func codexRateLimitResetConsumeClearsCooldown(code string) bool {
	code = strings.TrimSpace(code)
	return code == "reset" || code == "already_redeemed"
}

func codexRateLimitResetConsumeRefreshesUsage(code string) bool {
	code = strings.TrimSpace(code)
	return code == "reset" || code == "already_redeemed" || code == "no_credit" || code == "nothing_to_reset"
}
