package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
var codexUsageUserAgent = misc.CodexCLIUserAgent

func (h *Handler) resolveCodexUsageAuth(c *gin.Context) (*coreauth.Auth, int, string) {
	if c == nil {
		return nil, http.StatusBadRequest, "request is required"
	}
	authIndex := strings.TrimSpace(firstNonEmptyQueryValue(c, "auth_index", "authIndex", "AuthIndex"))
	if authIndex != "" {
		if auth := h.authByIndex(authIndex); auth != nil {
			return auth, http.StatusOK, ""
		}
	}

	name := strings.TrimSpace(firstNonEmptyQueryValue(c, "name", "file", "filename", "fileName"))
	if name == "" && authIndex == "" {
		return nil, http.StatusBadRequest, "name or auth_index is required"
	}
	if name != "" {
		if auth := h.authByName(name); auth != nil {
			return auth, http.StatusOK, ""
		}
	}
	if name == "" {
		return nil, http.StatusNotFound, "auth file not found"
	}
	return h.codexUsageAuthFromDisk(name)
}

func (h *Handler) authByName(name string) *coreauth.Auth {
	name = strings.TrimSpace(name)
	if name == "" || h == nil {
		return nil
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return nil
	}
	if auth, ok := manager.GetByName(name); ok {
		return auth
	}
	return nil
}

func (h *Handler) codexUsageAuthFromDisk(name string) (*coreauth.Auth, int, string) {
	if h == nil {
		return nil, http.StatusNotFound, "auth file not found"
	}
	authDir := h.authDirSnapshot()
	if authDir == "" {
		return nil, http.StatusNotFound, "auth file not found"
	}
	data, normalizedName, status, message := readAuthFileByNameAt(authDir, name)
	if status != http.StatusOK {
		return nil, status, message
	}
	metadata, err := coreauth.DecodeAuthFileMetadata(data)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Sprintf("invalid auth file JSON: %v", err)
	}
	provider := strings.TrimSpace(valueAsString(metadata["type"]))
	if provider == "" {
		provider = strings.TrimSpace(valueAsString(metadata["provider"]))
	}
	path := filepath.Join(authDir, normalizedName)
	return &coreauth.Auth{
		ID:       normalizedName,
		Provider: provider,
		FileName: normalizedName,
		ProxyURL: codexAuthMetadataString(metadata, "proxy_url", "proxy-url", "proxyUrl"),
		Metadata: metadata,
		Attributes: map[string]string{
			"path": path,
		},
	}, http.StatusOK, ""
}

func (h *Handler) refreshCodexUsageAuthIfNeeded(ctx context.Context, auth *coreauth.Auth) *coreauth.Auth {
	if h == nil || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return auth
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return auth
	}
	if !authFileHasRefreshToken(auth) {
		return auth
	}
	accessToken := codexUsageAccessToken(auth)
	shouldRefresh := accessToken == ""
	if !shouldRefresh {
		if expiresAt, ok := auth.ExpirationTime(); ok && !expiresAt.IsZero() {
			shouldRefresh = time.Until(expiresAt) <= 2*time.Minute
		}
	}
	if !shouldRefresh {
		return auth
	}
	updated, err := manager.RefreshAuth(ctx, auth)
	if err != nil {
		log.WithError(err).WithField("auth_id", auth.ID).Debug("failed to refresh codex auth before usage request")
		return auth
	}
	if updated == nil {
		return auth
	}
	return updated
}

func (h *Handler) fetchCodexUsage(ctx context.Context, auth *coreauth.Auth) (gin.H, int, error) {
	accessToken := codexUsageAccessToken(auth)
	if accessToken == "" && !codexUsageIsAgentIdentity(auth) {
		return nil, 0, fmt.Errorf("codex access_token missing")
	}
	accountID := resolveCodexUsageAccountID(auth, accessToken)
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := h.acquireCodexUpstreamSlot(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	if status, outage := h.activeCodexUsageOutage(auth, time.Now()); outage {
		return nil, status, fmt.Errorf("codex usage upstream temporarily unavailable (shared outage cooldown)")
	}

	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 20 * time.Second}
	if h != nil {
		client.Transport = h.codexUsageTransport(auth)
	}
	maxRetries := h.codexUsageRequestRetryLimit(auth)
	for attempt := 0; ; attempt++ {
		payload, status, err := h.doCodexUsageRequest(requestCtx, client, auth, accessToken, accountID)
		if err == nil {
			h.clearCodexUsageOutage(auth)
			return payload, status, nil
		}
		if ctx.Err() == nil && codexUsageTransientFailure(status, err) {
			h.markCodexUsageOutage(auth, status, time.Now())
		}
		if attempt >= maxRetries || !codexUsageShouldRetry(requestCtx, status, err) {
			return nil, status, err
		}
		log.WithError(err).WithFields(log.Fields{
			"attempt": attempt + 1,
			"max":     maxRetries,
			"status":  status,
		}).Debug("retrying codex usage request after transient failure")
		if errSleep := codexUsageSleepBeforeRetry(requestCtx, attempt+1); errSleep != nil {
			return nil, 0, errSleep
		}
	}
}

func (h *Handler) syncCodexUsageQuotaCooldown(ctx context.Context, auth *coreauth.Auth, payload gin.H) {
	if h == nil || auth == nil || strings.TrimSpace(auth.ID) == "" || len(payload) == 0 {
		return
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return
	}
	recoverAt, exhausted, observed := codexUsageQuotaState(payload, time.Now())
	if !observed {
		// A partial or malformed usage response is not evidence that an existing
		// cooldown has recovered. Keep its state until a usable main window is
		// returned instead of making the credential selectable prematurely.
		return
	}
	if !exhausted {
		// The usage endpoint is authoritative for auth-wide Codex limits. A
		// previously persisted cooldown must be cleared when it now reports
		// headroom; otherwise the auth remains labelled "quota exhausted" even
		// though its credential file has usable quota again.
		manager.ClearAuthQuotaCooldownFromUsage(ctx, auth.ID)
		return
	}
	manager.MarkAuthQuotaCooldown(ctx, auth.ID, recoverAt)
}

// codexUsageQuotaRecoverAt reports whether the credential's main Codex quota is
// exhausted, and when it recovers.
//
// Only the primary rate_limit windows count. additional_rate_limits describe
// per-feature metering (code review and friends): exhausting one of those does
// not stop the credential from serving normal Codex traffic, so folding them in
// here blocked credentials that still had quota. Free-plan credentials only get
// a weekly window upstream, and their primary_window can read 100% while the
// weekly window still has room, so it is skipped for that plan.
func codexUsageQuotaRecoverAt(payload gin.H, now time.Time) (time.Time, bool) {
	recoverAt, exhausted, _ := codexUsageQuotaState(payload, now)
	return recoverAt, exhausted
}

// codexUsageQuotaState reports a main quota state only when at least one
// applicable rate-limit window contains a numeric used_percent. The observed
// result lets callers distinguish confirmed headroom from an incomplete usage
// payload, so a failed parse cannot clear a live cooldown.
func codexUsageQuotaState(payload gin.H, now time.Time) (time.Time, bool, bool) {
	if len(payload) == 0 {
		return time.Time{}, false, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	rateLimit, ok := codexUsageWindowMap(payload["rate_limit"])
	if !ok {
		return time.Time{}, false, false
	}

	windowKeys := []string{"primary_window", "secondary_window"}
	if codexUsagePayloadIsFreePlan(payload) {
		windowKeys = []string{"secondary_window"}
	}

	var latestReset time.Time
	exhaustedWithoutReset := false
	observed := false
	for _, key := range windowKeys {
		window, ok := codexUsageWindowMap(rateLimit[key])
		if !ok {
			continue
		}
		usedPercent, hasUsage := numberFromAny(window["used_percent"])
		if !hasUsage {
			continue
		}
		observed = true
		if usedPercent < 100 {
			continue
		}
		resetAt, hasReset := codexUsageWindowResetAtOrAfter(window, now)
		if !hasReset {
			exhaustedWithoutReset = true
			continue
		}
		if resetAt.After(now) && resetAt.After(latestReset) {
			latestReset = resetAt
		}
	}
	if !latestReset.IsZero() {
		return latestReset, true, true
	}
	if exhaustedWithoutReset {
		return time.Time{}, true, true
	}
	return time.Time{}, false, observed
}

func codexUsagePayloadIsFreePlan(payload gin.H) bool {
	for _, key := range []string{"plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"} {
		if value := strings.TrimSpace(valueAsString(payload[key])); value != "" {
			return strings.EqualFold(value, "free")
		}
	}
	return false
}

func codexUsageWindowMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, len(typed) > 0
	case gin.H:
		return map[string]any(typed), len(typed) > 0
	default:
		return nil, false
	}
}

func codexUsageWindowResetAt(window map[string]any) (time.Time, bool) {
	if len(window) == 0 {
		return time.Time{}, false
	}
	raw, ok := window["reset_at"]
	if !ok {
		raw = window["resets_at"]
	}
	seconds, ok := numberFromAny(raw)
	if !ok || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0), true
}

func numberFromAny(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func (h *Handler) doCodexUsageRequest(ctx context.Context, client *http.Client, auth *coreauth.Auth, accessToken, accountID string) (gin.H, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return nil, 0, err
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codexUsageRequestUserAgent(h, auth))
	if codexUsageFedramp(auth) {
		req.Header.Set("X-OpenAI-Fedramp", "true")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	var resp *http.Response
	if codexUsageIsAgentIdentity(auth) {
		manager := h.authManagerSnapshot()
		if manager == nil {
			return nil, 0, fmt.Errorf("codex agent identity requires auth manager")
		}
		resp, err = manager.HttpRequest(ctx, auth, req)
	} else {
		resp, err = client.Do(req)
	}
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("codex usage request failed with status %d: %s", resp.StatusCode, truncateForLog(string(util.RedactSensitiveLogBytes(body)), 200))
	}
	payload := gin.H{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to decode codex usage response: %w", err)
	}
	return payload, resp.StatusCode, nil
}

func codexUsageAccessToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if token := codexAuthMetadataString(auth.Metadata, "access_token", "accessToken"); token != "" {
		return token
	}
	return strings.TrimSpace(authAttribute(auth, "access_token"))
}

func codexUsageIsAgentIdentity(auth *coreauth.Auth) bool {
	return coreauth.CodexAuthUsesAgentIdentity(auth)
}

func resolveCodexUsageAccountID(auth *coreauth.Auth, accessToken string) string {
	if auth != nil {
		if accountID := codexAuthMetadataString(auth.Metadata, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId"); accountID != "" {
			return accountID
		}
		for _, key := range []string{"chatgpt_account_id", "chatgptAccountId", "account_id", "accountId"} {
			if value := strings.TrimSpace(authAttribute(auth, key)); value != "" {
				return value
			}
		}
		if idToken := codexAuthMetadataString(auth.Metadata, "id_token", "idToken"); idToken != "" {
			if claims := parseCodexJWTClaims(idToken); claims != nil {
				if accountID := strings.TrimSpace(claims.GetAccountID()); accountID != "" {
					return accountID
				}
			}
		}
	}
	if claims := parseCodexJWTClaims(accessToken); claims != nil {
		return strings.TrimSpace(claims.GetAccountID())
	}
	return ""
}

func codexUsageRequestUserAgent(h *Handler, auth *coreauth.Auth) string {
	userAgent := strings.TrimSpace(readConfigValue(h, func(cfg *config.Config) string {
		return cfg.CodexHeaderDefaults.UserAgent
	}))
	if userAgent != "" {
		return userAgent
	}
	if userAgent := authFileUserAgent(auth); userAgent != "" {
		return userAgent
	}
	return codexUsageUserAgent
}

func codexUsageFedramp(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	for _, key := range []string{"fedramp", "openai_fedramp", "x_openai_fedramp", "X-OpenAI-Fedramp"} {
		if raw := strings.TrimSpace(authAttribute(auth, key)); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			return err == nil && parsed
		}
		if auth.Metadata != nil {
			if parsed, ok := boolLikeValue(auth.Metadata[key]); ok {
				return parsed
			}
		}
	}
	return false
}

func boolLikeValue(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return parsed, err == nil
	default:
		return false, false
	}
}

func mergeCodexUsageLocalFields(payload gin.H, auth *coreauth.Auth) {
	if payload == nil || auth == nil {
		return
	}
	accountID := resolveCodexUsageAccountID(auth, codexUsageAccessToken(auth))
	setCodexUsageFieldIfMissing(payload, "account_id", accountID)
	setCodexUsageFieldIfMissing(payload, "chatgpt_account_id", accountID)
	setCodexUsageFieldIfMissing(payload, "email", authEmail(auth))
	setCodexUsageFieldIfMissing(payload, "plan_type", codexUsagePlanType(auth))
	if until, ok := codexSubscriptionUntilValue(auth.Metadata); ok {
		setCodexUsageFieldIfMissing(payload, "subscription_expires_at", until)
		setCodexUsageFieldIfMissing(payload, "chatgpt_subscription_active_until", until)
	}
	if start, ok := codexSubscriptionDisplayActiveStartValue(auth.Metadata); ok {
		setCodexUsageFieldIfMissing(payload, "chatgpt_subscription_active_start", start)
		setCodexUsageFieldIfMissing(payload, "subscription_active_start", start)
	}
	if days, ok := codexSubscriptionDisplayActiveDaysValue(auth.Metadata); ok {
		setCodexUsageFieldIfMissing(payload, "subscription_active_days", days)
	}
	mergeCodexUsageJWTFields(payload, codexAuthMetadataString(auth.Metadata, "id_token", "idToken"))
	mergeCodexUsageJWTFields(payload, codexUsageAccessToken(auth))
}

func mergeCodexUsageJWTFields(payload gin.H, token string) {
	claims := parseCodexJWTClaims(token)
	if claims == nil {
		return
	}
	setCodexUsageFieldIfMissing(payload, "account_id", strings.TrimSpace(claims.GetAccountID()))
	setCodexUsageFieldIfMissing(payload, "chatgpt_account_id", strings.TrimSpace(claims.GetAccountID()))
	setCodexUsageFieldIfMissing(payload, "email", codexClaimsEmail(claims))
	setCodexUsageFieldIfMissing(payload, "plan_type", codexClaimsPlanType(claims))
	setCodexUsageFieldIfMissing(payload, "chatgpt_plan_type", codexClaimsPlanType(claims))
	if value, ok := normalizeCodexSubscriptionUntilValue(claims.CodexAuthInfo.ChatgptSubscriptionActiveStart); ok {
		setCodexUsageFieldIfMissing(payload, "chatgpt_subscription_active_start", value)
	}
	if value, ok := normalizeCodexSubscriptionUntilValue(claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil); ok {
		setCodexUsageFieldIfMissing(payload, "chatgpt_subscription_active_until", value)
		setCodexUsageFieldIfMissing(payload, "subscription_expires_at", value)
	}
}

func codexUsagePlanType(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if planType := codexAuthMetadataString(auth.Metadata, "plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"); planType != "" {
		return planType
	}
	for _, key := range []string{"plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"} {
		if value := strings.TrimSpace(authAttribute(auth, key)); value != "" {
			return value
		}
	}
	for _, token := range []string{codexAuthMetadataString(auth.Metadata, "id_token", "idToken"), codexUsageAccessToken(auth)} {
		if claims := parseCodexJWTClaims(token); claims != nil {
			if planType := codexClaimsPlanType(claims); planType != "" {
				return planType
			}
		}
	}
	return ""
}

func codexUsageMetadataFirstValue(metadata map[string]any, keys ...string) any {
	if len(metadata) == 0 {
		return nil
	}
	for _, key := range keys {
		if value, ok := metadata[key]; ok && !authFilePreviewEmptyValue(value) {
			return value
		}
	}
	return nil
}

func setCodexUsageFieldIfMissing(payload gin.H, key string, value any) {
	if payload == nil || key == "" || authFilePreviewEmptyValue(value) {
		return
	}
	if existing, ok := payload[key]; ok && !authFilePreviewEmptyValue(existing) {
		return
	}
	payload[key] = value
}
