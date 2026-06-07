package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var codexAccountsCheckURL = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27"

const codexAccountsCheckUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

type codexSubscriptionCacheEntry struct {
	info      codexAccountSubscriptionInfo
	found     bool
	expiresAt time.Time
}

type codexAccountSubscriptionInfo struct {
	PlanType                string
	Email                   string
	SubscriptionExpiresAt   string
	SubscriptionActiveStart string
}

var codexSubscriptionCache sync.Map

type codexSubscriptionListMode int

const (
	codexSubscriptionListCache codexSubscriptionListMode = iota
	codexSubscriptionListRefresh
	codexSubscriptionListSkip
)

func codexSubscriptionListModeFromRequest(c *gin.Context) codexSubscriptionListMode {
	mode := strings.TrimSpace(firstNonEmptyQueryValue(c, "codex_subscription", "codexSubscription"))
	if mode == "" {
		return codexSubscriptionListCache
	}
	if isRefreshQueryValue(mode) {
		return codexSubscriptionListRefresh
	}
	if isSkipQueryValue(mode) {
		return codexSubscriptionListSkip
	}
	return codexSubscriptionListCache
}

func authFileHasRefreshToken(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if authFileMetadataHasRefreshToken(auth.Metadata) {
		return true
	}
	return strings.TrimSpace(authAttribute(auth, "refresh_token")) != "" || strings.TrimSpace(authAttribute(auth, "refreshToken")) != ""
}

func applyCodexSubscriptionSnapshot(entry gin.H, auth *coreauth.Auth) {
	if entry == nil || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	if planType := codexUsagePlanType(auth); planType != "" {
		entry["plan_type"] = planType
		entry["chatgpt_plan_type"] = planType
	}
	if until, ok := codexSubscriptionUntilValue(auth.Metadata); ok {
		entry["subscription_expires_at"] = until
		entry["chatgpt_subscription_active_until"] = until
	}
	if start, ok := codexSubscriptionDisplayActiveStartValue(auth.Metadata); ok {
		entry["chatgpt_subscription_active_start"] = start
		entry["subscription_active_start"] = start
	}
	if days, ok := codexSubscriptionDisplayActiveDaysValue(auth.Metadata); ok {
		entry["subscription_active_days"] = days
	}
}

func applyCodexSubscriptionSnapshotSummary(entry gin.H, auth *coreauth.Auth) {
	if entry == nil || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	if planType := codexSubscriptionMetadataPlanType(auth.Metadata); planType != "" {
		entry["plan_type"] = planType
		entry["chatgpt_plan_type"] = planType
	}
	if until, ok := codexSubscriptionUntilValue(auth.Metadata); ok {
		entry["subscription_expires_at"] = until
		entry["chatgpt_subscription_active_until"] = until
	}
	if start, ok := codexSubscriptionDisplayActiveStartValue(auth.Metadata); ok {
		entry["chatgpt_subscription_active_start"] = start
		entry["subscription_active_start"] = start
	}
	if days, ok := codexSubscriptionDisplayActiveDaysValue(auth.Metadata); ok {
		entry["subscription_active_days"] = days
	}
}

func codexSubscriptionMetadataPlanType(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"} {
		if planType := strings.TrimSpace(valueAsString(metadata[key])); planType != "" {
			return planType
		}
	}
	return ""
}

func authFileMetadataHasRefreshToken(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	for _, key := range []string{"refresh_token", "refreshToken"} {
		if value, ok := metadata[key]; ok && strings.TrimSpace(valueAsString(value)) != "" {
			return true
		}
	}
	for _, containerKey := range []string{"token", "tokens", "token_data", "tokenData"} {
		container, ok := metadata[containerKey]
		if !ok || container == nil {
			continue
		}
		switch nested := container.(type) {
		case map[string]any:
			if authFileMetadataHasRefreshToken(nested) {
				return true
			}
		case map[string]string:
			if strings.TrimSpace(nested["refresh_token"]) != "" || strings.TrimSpace(nested["refreshToken"]) != "" {
				return true
			}
		}
	}
	return false
}

func codexSubscriptionUntilValue(metadata map[string]any) (any, bool) {
	if len(metadata) == 0 {
		return nil, false
	}
	if value, ok := codexSubscriptionUntilValueFromKeys(metadata, []string{
		"subscription_expires_at",
		"subscriptionExpiresAt",
		"chatgpt_subscription_active_until",
		"chatgptSubscriptionActiveUntil",
	}); ok {
		return value, true
	}

	for _, candidate := range []struct {
		container string
		keys      []string
	}{
		{container: "account", keys: []string{"subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil", "subscription_active_until", "subscriptionActiveUntil"}},
		{container: "entitlement", keys: []string{"subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil", "expires_at", "expiresAt", "current_period_end", "currentPeriodEnd", "period_end", "periodEnd"}},
		{container: "subscription", keys: []string{"subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil", "expires_at", "expiresAt", "current_period_end", "currentPeriodEnd", "period_end", "periodEnd"}},
		{container: "providerSpecificData", keys: []string{"subscription_expires_at", "subscriptionExpiresAt", "chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil"}},
	} {
		nested, ok := metadata[candidate.container].(map[string]any)
		if !ok || len(nested) == 0 {
			continue
		}
		if value, ok := codexSubscriptionUntilValueFromKeys(nested, candidate.keys); ok {
			return value, true
		}
	}
	return nil, false
}

func codexSubscriptionActiveStartValue(metadata map[string]any) (any, bool) {
	if len(metadata) == 0 {
		return nil, false
	}
	if value, ok := codexSubscriptionUntilValueFromKeys(metadata, codexSubscriptionActiveStartKeys()); ok {
		return value, true
	}

	for _, candidate := range []struct {
		container string
		keys      []string
	}{
		{container: "account", keys: codexSubscriptionActiveStartKeys()},
		{container: "entitlement", keys: codexSubscriptionActiveStartKeys()},
		{container: "subscription", keys: codexSubscriptionActiveStartKeys()},
		{container: "providerSpecificData", keys: codexSubscriptionActiveStartKeys()},
	} {
		nested, ok := metadata[candidate.container].(map[string]any)
		if !ok || len(nested) == 0 {
			continue
		}
		if value, ok := codexSubscriptionUntilValueFromKeys(nested, candidate.keys); ok {
			return value, true
		}
	}
	return nil, false
}

func codexSubscriptionActiveStartKeys() []string {
	return []string{
		"chatgpt_subscription_active_start",
		"chatgptSubscriptionActiveStart",
		"subscription_active_start",
		"subscriptionActiveStart",
		"subscription_started_at",
		"subscriptionStartedAt",
		"subscription_start_date",
		"subscriptionStartDate",
		"current_period_start",
		"currentPeriodStart",
		"period_start",
		"periodStart",
		"started_at",
		"startedAt",
		"starts_at",
		"startsAt",
	}
}

func codexSubscriptionActiveDaysValue(metadata map[string]any) (int, bool) {
	for _, key := range []string{"subscription_active_days", "subscriptionActiveDays"} {
		if days, ok := normalizeNonNegativeInt(metadata[key]); ok {
			return days, true
		}
	}
	start, ok := codexSubscriptionActiveStartValue(metadata)
	if !ok {
		return 0, false
	}
	startMs := authFileListTimestampMs(start)
	if startMs <= 0 {
		return 0, false
	}
	days := int(time.Since(time.UnixMilli(startMs)).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return days, true
}

func codexSubscriptionDisplayActiveStartValue(metadata map[string]any) (any, bool) {
	if start, ok := codexSubscriptionActiveStartValue(metadata); ok {
		return start, true
	}
	if until, ok := codexSubscriptionUntilValue(metadata); ok {
		return deriveCodexSubscriptionActiveStartFromUntilValue(until)
	}
	return nil, false
}

func codexSubscriptionDisplayActiveDaysValue(metadata map[string]any) (int, bool) {
	if days, ok := codexSubscriptionActiveDaysValue(metadata); ok {
		return days, true
	}
	start, ok := codexSubscriptionDisplayActiveStartValue(metadata)
	if !ok {
		return 0, false
	}
	startMs := authFileListTimestampMs(start)
	if startMs <= 0 {
		return 0, false
	}
	days := int(time.Since(time.UnixMilli(startMs)).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return days, true
}

func deriveCodexSubscriptionActiveStartFromUntilValue(value any) (string, bool) {
	untilMs := authFileListTimestampMs(value)
	if untilMs <= 0 {
		return "", false
	}
	until := time.UnixMilli(untilMs).UTC()
	start := addMonthsClampedUTC(until, -1)
	if !start.Before(until) {
		return "", false
	}
	return start.Format(time.RFC3339), true
}

func addMonthsClampedUTC(t time.Time, months int) time.Time {
	t = t.UTC()
	year, month, day := t.Date()
	monthIndex := int(month) - 1 + months
	targetYear := year + monthIndex/12
	targetMonthIndex := monthIndex % 12
	if targetMonthIndex < 0 {
		targetMonthIndex += 12
		targetYear--
	}
	targetMonth := time.Month(targetMonthIndex + 1)
	lastDay := time.Date(targetYear, targetMonth+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > lastDay {
		day = lastDay
	}
	hour, minute, second := t.Clock()
	return time.Date(targetYear, targetMonth, day, hour, minute, second, t.Nanosecond(), time.UTC)
}

func normalizeNonNegativeInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, v >= 0
	case int64:
		if v < 0 {
			return 0, false
		}
		return int(v), true
	case float64:
		if v < 0 {
			return 0, false
		}
		return int(v), true
	case json.Number:
		parsed, err := v.Int64()
		if err != nil || parsed < 0 {
			return 0, false
		}
		return int(parsed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || parsed < 0 {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func codexSubscriptionUntilValueFromKeys(metadata map[string]any, keys []string) (any, bool) {
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if normalized, okNormalize := normalizeCodexSubscriptionUntilValue(value); okNormalize {
				return normalized, true
			}
		}
	}
	return nil, false
}

func normalizeCodexSubscriptionUntilValue(value any) (any, bool) {
	if value == nil {
		return nil, false
	}
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, false
		}
		return trimmed, true
	case json.Number:
		trimmed := strings.TrimSpace(v.String())
		if trimmed == "" {
			return nil, false
		}
		return trimmed, true
	case int:
		return v, true
	case int64:
		return v, true
	case float64:
		return v, true
	case time.Time:
		if v.IsZero() {
			return nil, false
		}
		return v.UTC().Format(time.RFC3339), true
	default:
		return nil, false
	}
}

func applyCodexSubscriptionFromClaims(entry gin.H, claims gin.H) {
	if entry == nil || len(claims) == 0 {
		return
	}
	if current, ok := entry["subscription_expires_at"]; ok {
		if _, okNormalize := normalizeCodexSubscriptionUntilValue(current); okNormalize {
			return
		}
	}
	for _, key := range []string{
		"subscription_expires_at",
		"subscriptionExpiresAt",
		"chatgpt_subscription_active_until",
		"chatgptSubscriptionActiveUntil",
	} {
		if value, ok := claims[key]; ok {
			if normalized, okNormalize := normalizeCodexSubscriptionUntilValue(value); okNormalize {
				entry["subscription_expires_at"] = normalized
				return
			}
		}
	}
}

func (h *Handler) enrichCodexSubscriptionInfo(ctx context.Context, auth *coreauth.Auth, mode codexSubscriptionListMode) *coreauth.Auth {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return auth
	}
	if until, ok := codexSubscriptionUntilValue(auth.Metadata); ok {
		updated, changed := ensureCodexSubscriptionSnapshotMetadata(auth, until)
		if mode == codexSubscriptionListRefresh && changed {
			h.persistCodexSubscriptionBackfill(ctx, updated)
		}
		if mode != codexSubscriptionListRefresh || codexSubscriptionHasActiveDuration(updated.Metadata) {
			return updated
		}
		auth = updated
	}
	if mode == codexSubscriptionListSkip {
		return auth
	}
	accessToken := codexAuthMetadataString(auth.Metadata, "access_token", "accessToken")
	if accessToken == "" {
		accessToken = strings.TrimSpace(authAttribute(auth, "access_token"))
	}
	proxyURL := h.codexSubscriptionProxyURL(auth)
	cacheKey := ""
	if accessToken != "" {
		cacheKey = codexSubscriptionCacheKey(accessToken, proxyURL)
	}
	now := time.Now()
	if mode != codexSubscriptionListRefresh {
		if cached, ok := h.loadCodexSubscriptionCache(ctx, auth, cacheKey, now); ok {
			if cached.found {
				return applyCodexAccountSubscriptionInfo(auth, cached.info)
			}
			return auth
		}
	}
	if mode == codexSubscriptionListCache || accessToken == "" {
		return auth
	}

	orgID := resolveCodexAccountCheckOrgID(auth, accessToken)
	info, err := h.fetchCodexAccountSubscriptionInfo(ctx, accessToken, proxyURL, orgID)
	if err != nil {
		log.WithError(err).Debug("failed to fetch codex subscription info")
		h.storeCodexSubscriptionCache(ctx, auth, cacheKey, codexSubscriptionCacheEntry{found: false, expiresAt: now.Add(10 * time.Minute)})
		return auth
	}
	if info == nil || !info.hasData() {
		h.storeCodexSubscriptionCache(ctx, auth, cacheKey, codexSubscriptionCacheEntry{found: false, expiresAt: now.Add(30 * time.Minute)})
		return auth
	}
	h.storeCodexSubscriptionCache(ctx, auth, cacheKey, codexSubscriptionCacheEntry{info: *info, found: true, expiresAt: now.Add(6 * time.Hour)})
	updated := applyCodexAccountSubscriptionInfo(auth, *info)
	if mode == codexSubscriptionListRefresh && codexSubscriptionBackfillShouldPersist(auth, updated) {
		h.persistCodexSubscriptionBackfill(ctx, updated)
	}
	return updated
}

func codexSubscriptionHasActiveDuration(metadata map[string]any) bool {
	if _, ok := codexSubscriptionActiveDaysValue(metadata); ok {
		return true
	}
	_, ok := codexSubscriptionActiveStartValue(metadata)
	return ok
}

func ensureCodexSubscriptionSnapshotMetadata(auth *coreauth.Auth, value any) (*coreauth.Auth, bool) {
	if auth == nil {
		return auth, false
	}
	normalized, ok := normalizeCodexSubscriptionUntilValue(value)
	if !ok {
		return auth, false
	}
	normalizedString := strings.TrimSpace(valueAsString(normalized))
	if normalizedString == "" {
		return auth, false
	}
	changed := false
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	for _, key := range []string{"subscription_expires_at", "chatgpt_subscription_active_until"} {
		if existing, ok := normalizeCodexSubscriptionUntilValue(updated.Metadata[key]); !ok || strings.TrimSpace(valueAsString(existing)) != normalizedString {
			updated.Metadata[key] = normalizedString
			changed = true
		}
	}
	if start, ok := codexSubscriptionActiveStartValue(auth.Metadata); ok {
		startString := strings.TrimSpace(valueAsString(start))
		if startString != "" {
			for _, key := range []string{"chatgpt_subscription_active_start", "subscription_active_start"} {
				if existing, ok := normalizeCodexSubscriptionUntilValue(updated.Metadata[key]); !ok || strings.TrimSpace(valueAsString(existing)) != startString {
					updated.Metadata[key] = startString
					changed = true
				}
			}
		}
	}
	return updated, changed
}

func codexSubscriptionBackfillShouldPersist(original, updated *coreauth.Auth) bool {
	if updated == nil || len(updated.Metadata) == 0 {
		return false
	}
	if value, ok := normalizeCodexSubscriptionUntilValue(updated.Metadata["subscription_expires_at"]); ok {
		updatedValue := strings.TrimSpace(valueAsString(value))
		if updatedValue != "" {
			if original == nil || len(original.Metadata) == 0 {
				return true
			}
			if existing, okExisting := normalizeCodexSubscriptionUntilValue(original.Metadata["subscription_expires_at"]); !okExisting || strings.TrimSpace(valueAsString(existing)) != updatedValue {
				return true
			}
		}
	}
	if value, ok := codexSubscriptionActiveStartValue(updated.Metadata); ok {
		updatedValue := strings.TrimSpace(valueAsString(value))
		if updatedValue != "" {
			if original == nil || len(original.Metadata) == 0 {
				return true
			}
			if existing, okExisting := codexSubscriptionActiveStartValue(original.Metadata); !okExisting || strings.TrimSpace(valueAsString(existing)) != updatedValue {
				return true
			}
		}
	}
	for _, key := range []string{"plan_type", "chatgpt_plan_type"} {
		updatedValue := strings.TrimSpace(valueAsString(updated.Metadata[key]))
		if updatedValue == "" {
			continue
		}
		if original == nil || !strings.EqualFold(strings.TrimSpace(valueAsString(original.Metadata[key])), updatedValue) {
			return true
		}
	}
	if updatedValue := strings.TrimSpace(valueAsString(updated.Metadata["email"])); updatedValue != "" {
		if original == nil || strings.TrimSpace(valueAsString(original.Metadata["email"])) == "" {
			return true
		}
	}
	return false
}

func (h *Handler) persistCodexSubscriptionBackfill(ctx context.Context, auth *coreauth.Auth) {
	if h == nil || auth == nil || !codexSubscriptionBackfillHasPersistableData(auth.Metadata) {
		return
	}
	persistCtx := context.Background()
	if ctx != nil {
		persistCtx = context.WithoutCancel(ctx)
	}
	if h.authManager != nil && strings.TrimSpace(auth.ID) != "" {
		if updated, err := h.authManager.Update(persistCtx, auth); err != nil {
			log.WithError(err).WithField("auth_id", auth.ID).Warn("failed to persist codex subscription info")
		} else if updated != nil {
			auth = updated
		}
	}
	path := resolveCodexSubscriptionBackfillPath(h, auth)
	if path == "" {
		return
	}
	authDir := ""
	if h.cfg != nil {
		authDir = strings.TrimSpace(h.cfg.AuthDir)
	}
	if err := persistCodexSubscriptionBackfillFile(path, authDir, auth.Metadata); err != nil {
		log.WithError(err).WithField("path", path).Warn("failed to persist codex subscription info")
	}
}

func codexSubscriptionBackfillHasPersistableData(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	if value, ok := normalizeCodexSubscriptionUntilValue(metadata["subscription_expires_at"]); ok && strings.TrimSpace(valueAsString(value)) != "" {
		return true
	}
	if value, ok := codexSubscriptionActiveStartValue(metadata); ok && strings.TrimSpace(valueAsString(value)) != "" {
		return true
	}
	for _, key := range []string{"plan_type", "chatgpt_plan_type", "email"} {
		if strings.TrimSpace(valueAsString(metadata[key])) != "" {
			return true
		}
	}
	return false
}

func resolveCodexSubscriptionBackfillPath(h *Handler, auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	authDir := ""
	if h != nil && h.cfg != nil {
		authDir = strings.TrimSpace(h.cfg.AuthDir)
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			if authDir == "" || authFilePathWithinDir(path, authDir) {
				return path
			}
		}
	}
	for _, candidate := range []string{auth.FileName, auth.ID} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if filepath.IsAbs(candidate) || authDir == "" {
			if authDir == "" || authFilePathWithinDir(candidate, authDir) {
				return candidate
			}
			continue
		}
		path := filepath.Join(authDir, candidate)
		if authFilePathWithinDir(path, authDir) {
			return path
		}
	}
	return ""
}

func persistCodexSubscriptionBackfillFile(path, authDir string, metadata map[string]any) error {
	path = strings.TrimSpace(path)
	if path == "" || len(metadata) == 0 {
		return nil
	}
	data, err := readManagedAuthPathFile(path, authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read auth file: %w", err)
	}
	doc := make(map[string]any)
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid auth file: %w", err)
	}
	if !applyCodexSubscriptionBackfillDocument(doc, metadata) {
		return nil
	}
	updated, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode auth file: %w", err)
	}
	updated = append(updated, '\n')
	if err := writeManagedAuthPathFile(path, authDir, updated, 0o600); err != nil {
		return fmt.Errorf("failed to write auth file: %w", err)
	}
	return nil
}

func applyCodexSubscriptionBackfillDocument(doc map[string]any, metadata map[string]any) bool {
	if doc == nil || len(metadata) == 0 {
		return false
	}
	changed := false
	if value, ok := normalizeCodexSubscriptionUntilValue(metadata["subscription_expires_at"]); ok {
		normalized := strings.TrimSpace(valueAsString(value))
		if normalized != "" {
			for _, key := range []string{"subscription_expires_at", "chatgpt_subscription_active_until"} {
				if existing, okExisting := normalizeCodexSubscriptionUntilValue(doc[key]); !okExisting || strings.TrimSpace(valueAsString(existing)) != normalized {
					doc[key] = normalized
					changed = true
				}
			}
		}
	}
	if value, ok := codexSubscriptionActiveStartValue(metadata); ok {
		normalized := strings.TrimSpace(valueAsString(value))
		if normalized != "" {
			for _, key := range []string{"chatgpt_subscription_active_start", "subscription_active_start"} {
				if existing, okExisting := normalizeCodexSubscriptionUntilValue(doc[key]); !okExisting || strings.TrimSpace(valueAsString(existing)) != normalized {
					doc[key] = normalized
					changed = true
				}
			}
		}
	}
	for _, key := range []string{"plan_type", "chatgpt_plan_type"} {
		value := strings.TrimSpace(valueAsString(metadata[key]))
		if value == "" || strings.EqualFold(strings.TrimSpace(valueAsString(doc[key])), value) {
			continue
		}
		doc[key] = value
		changed = true
	}
	value := strings.TrimSpace(valueAsString(metadata["email"]))
	if value != "" && strings.TrimSpace(valueAsString(doc["email"])) == "" {
		doc["email"] = value
		changed = true
	}
	return changed
}

func codexAuthMetadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if str := strings.TrimSpace(valueAsString(value)); str != "" {
				return str
			}
		}
	}
	for _, containerKey := range []string{"token", "tokens", "token_data", "tokenData"} {
		container, ok := metadata[containerKey]
		if !ok || container == nil {
			continue
		}
		if nested, ok := container.(map[string]any); ok {
			if str := codexAuthMetadataString(nested, keys...); str != "" {
				return str
			}
		}
	}
	return ""
}

func authFileMetadataProxyURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	return codexAuthMetadataString(auth.Metadata, "proxy_url", "proxy-url", "proxyUrl")
}

func authFileProxyPoolAssigned(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["proxy_pool_assigned"]), "true")
}

func codexSubscriptionCacheKey(accessToken, proxyURL string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(proxyURL)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.TrimSpace(accessToken)))
	return hex.EncodeToString(h.Sum(nil))
}

func (h *Handler) codexSubscriptionProxyURL(auth *coreauth.Auth) string {
	globalProxy := ""
	if h != nil && h.cfg != nil {
		globalProxy = strings.TrimSpace(h.cfg.ProxyURL)
	}
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			if codexProxySettingIsDirect(proxyURL) && globalProxy != "" {
				return globalProxy
			}
			return proxyURL
		}
		if proxyURL := codexAuthMetadataString(auth.Metadata, "proxy_url", "proxy-url", "proxyUrl"); proxyURL != "" {
			if codexProxySettingIsDirect(proxyURL) && globalProxy != "" {
				return globalProxy
			}
			return proxyURL
		}
	}
	return globalProxy
}

func codexProxySettingIsDirect(proxyURL string) bool {
	proxyURL = strings.TrimSpace(proxyURL)
	return strings.EqualFold(proxyURL, "none") || strings.EqualFold(proxyURL, "direct")
}

func (h *Handler) codexUsageTransport(auth *coreauth.Auth) http.RoundTripper {
	if h == nil {
		return nil
	}
	proxyURL := h.codexSubscriptionProxyURL(auth)
	if auth == nil || proxyURL == strings.TrimSpace(auth.ProxyURL) {
		return h.apiCallTransport(auth)
	}
	updated := auth.Clone()
	updated.ProxyURL = proxyURL
	return h.apiCallTransport(updated)
}

func applyCodexAccountSubscriptionInfo(auth *coreauth.Auth, info codexAccountSubscriptionInfo) *coreauth.Auth {
	if auth == nil || !info.hasData() {
		return auth
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	if info.SubscriptionExpiresAt != "" {
		updated.Metadata["subscription_expires_at"] = info.SubscriptionExpiresAt
		updated.Metadata["chatgpt_subscription_active_until"] = info.SubscriptionExpiresAt
	}
	if info.SubscriptionActiveStart != "" {
		updated.Metadata["chatgpt_subscription_active_start"] = info.SubscriptionActiveStart
		updated.Metadata["subscription_active_start"] = info.SubscriptionActiveStart
	}
	if info.PlanType != "" {
		updated.Metadata["plan_type"] = info.PlanType
		updated.Metadata["chatgpt_plan_type"] = info.PlanType
	}
	if info.Email != "" && strings.TrimSpace(valueAsString(updated.Metadata["email"])) == "" {
		updated.Metadata["email"] = info.Email
	}
	return updated
}

func (info codexAccountSubscriptionInfo) hasData() bool {
	return strings.TrimSpace(info.PlanType) != "" ||
		strings.TrimSpace(info.Email) != "" ||
		strings.TrimSpace(info.SubscriptionExpiresAt) != "" ||
		strings.TrimSpace(info.SubscriptionActiveStart) != ""
}

func resolveCodexAccountCheckOrgID(auth *coreauth.Auth, accessToken string) string {
	if auth != nil {
		for _, key := range []string{"organization_id", "organizationId", "org_id", "orgId", "poid"} {
			if value := codexAuthMetadataString(auth.Metadata, key); value != "" {
				return value
			}
		}
	}
	if claims := parseCodexJWTClaims(accessToken); claims != nil {
		if poid := strings.TrimSpace(claims.CodexAuthInfo.POID); poid != "" {
			return poid
		}
		if orgID := defaultCodexOrganizationID(claims); orgID != "" {
			return orgID
		}
		if accountID := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); accountID != "" {
			return accountID
		}
	}
	if auth != nil {
		if idToken := codexAuthMetadataString(auth.Metadata, "id_token", "idToken"); idToken != "" {
			if claims := parseCodexJWTClaims(idToken); claims != nil {
				if poid := strings.TrimSpace(claims.CodexAuthInfo.POID); poid != "" {
					return poid
				}
				if orgID := defaultCodexOrganizationID(claims); orgID != "" {
					return orgID
				}
				if accountID := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); accountID != "" {
					return accountID
				}
			}
		}
		if accountID := codexAuthMetadataString(auth.Metadata, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
			return accountID
		}
	}
	return ""
}

func parseCodexJWTClaims(token string) *codex.JWTClaims {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	claims, err := codex.ParseJWTToken(token)
	if err != nil {
		return nil
	}
	return claims
}

func defaultCodexOrganizationID(claims *codex.JWTClaims) string {
	if claims == nil {
		return ""
	}
	for _, org := range claims.CodexAuthInfo.Organizations {
		if org.IsDefault && strings.TrimSpace(org.ID) != "" {
			return strings.TrimSpace(org.ID)
		}
	}
	for _, org := range claims.CodexAuthInfo.Organizations {
		if strings.TrimSpace(org.ID) != "" {
			return strings.TrimSpace(org.ID)
		}
	}
	return ""
}

func (h *Handler) fetchCodexAccountSubscriptionInfo(ctx context.Context, accessToken, proxyURL, orgID string) (*codexAccountSubscriptionInfo, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, codexAccountsCheckURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codexAccountsCheckUserAgent)
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Dest", "empty")

	client := &http.Client{Timeout: 15 * time.Second}
	if h != nil && h.cfg != nil {
		sdkCfg := h.cfg.SDKConfig
		sdkCfg.ProxyURL = strings.TrimSpace(proxyURL)
		client = util.SetProxy(&sdkCfg, client)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex account check failed with status %d: %s", resp.StatusCode, truncateForLog(string(body), 200))
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return parseCodexAccountSubscriptionInfo(result, orgID), nil
}

func parseCodexAccountSubscriptionInfo(result map[string]any, orgID string) *codexAccountSubscriptionInfo {
	accounts, ok := result["accounts"].(map[string]any)
	if !ok || len(accounts) == 0 {
		return nil
	}
	orgID = strings.TrimSpace(orgID)
	if orgID != "" {
		if acct, ok := accountObject(accounts[orgID]); ok {
			info := codexAccountSubscriptionInfoFromAccount(acct)
			if info.hasData() {
				return &info
			}
		}
	}

	var defaultInfo, paidInfo, anyInfo codexAccountSubscriptionInfo
	for _, raw := range accounts {
		acct, ok := accountObject(raw)
		if !ok {
			continue
		}
		info := codexAccountSubscriptionInfoFromAccount(acct)
		if !info.hasData() {
			continue
		}
		if !anyInfo.hasData() {
			anyInfo = info
		}
		if isDefaultCodexAccount(acct) && !defaultInfo.hasData() {
			defaultInfo = info
		}
		if !strings.EqualFold(strings.TrimSpace(info.PlanType), "free") && !paidInfo.hasData() {
			paidInfo = info
		}
	}
	switch {
	case defaultInfo.hasData():
		return &defaultInfo
	case paidInfo.hasData():
		return &paidInfo
	case anyInfo.hasData():
		return &anyInfo
	default:
		return nil
	}
}

func accountObject(raw any) (map[string]any, bool) {
	acct, ok := raw.(map[string]any)
	return acct, ok
}

func codexAccountSubscriptionInfoFromAccount(acct map[string]any) codexAccountSubscriptionInfo {
	return codexAccountSubscriptionInfo{
		PlanType:                extractCodexAccountPlanType(acct),
		Email:                   extractCodexAccountEmail(acct),
		SubscriptionExpiresAt:   extractCodexEntitlementExpiresAt(acct),
		SubscriptionActiveStart: extractCodexEntitlementActiveStart(acct),
	}
}

func extractCodexAccountPlanType(acct map[string]any) string {
	if account, ok := acct["account"].(map[string]any); ok {
		if planType := strings.TrimSpace(valueAsString(account["plan_type"])); planType != "" {
			return planType
		}
		if planType := strings.TrimSpace(valueAsString(account["planType"])); planType != "" {
			return planType
		}
	}
	if entitlement, ok := acct["entitlement"].(map[string]any); ok {
		if planType := strings.TrimSpace(valueAsString(entitlement["subscription_plan"])); planType != "" {
			return planType
		}
		if planType := strings.TrimSpace(valueAsString(entitlement["subscriptionPlan"])); planType != "" {
			return planType
		}
	}
	return ""
}

func extractCodexAccountEmail(acct map[string]any) string {
	if account, ok := acct["account"].(map[string]any); ok {
		if email := strings.TrimSpace(valueAsString(account["email"])); email != "" {
			return email
		}
	}
	if user, ok := acct["user"].(map[string]any); ok {
		if email := strings.TrimSpace(valueAsString(user["email"])); email != "" {
			return email
		}
	}
	return ""
}

func extractCodexEntitlementExpiresAt(acct map[string]any) string {
	keys := []string{"expires_at", "expiresAt", "subscription_expires_at", "subscriptionExpiresAt", "current_period_end", "currentPeriodEnd", "period_end", "periodEnd"}
	for _, container := range []string{"entitlement", "subscription", "account"} {
		nested, ok := acct[container].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range keys {
			if value, ok := normalizeCodexSubscriptionUntilValue(nested[key]); ok {
				return strings.TrimSpace(valueAsString(value))
			}
		}
	}
	for _, key := range keys {
		if value, ok := normalizeCodexSubscriptionUntilValue(acct[key]); ok {
			return strings.TrimSpace(valueAsString(value))
		}
	}
	return ""
}

func extractCodexEntitlementActiveStart(acct map[string]any) string {
	for _, container := range []string{"entitlement", "subscription", "account"} {
		nested, ok := acct[container].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range codexSubscriptionActiveStartKeys() {
			if value, ok := normalizeCodexSubscriptionUntilValue(nested[key]); ok {
				return strings.TrimSpace(valueAsString(value))
			}
		}
	}
	for _, key := range codexSubscriptionActiveStartKeys() {
		if value, ok := normalizeCodexSubscriptionUntilValue(acct[key]); ok {
			return strings.TrimSpace(valueAsString(value))
		}
	}
	return ""
}

func isDefaultCodexAccount(acct map[string]any) bool {
	account, ok := acct["account"].(map[string]any)
	if !ok {
		return false
	}
	value, ok := account["is_default"].(bool)
	if !ok {
		if camelValue, okCamel := account["isDefault"].(bool); okCamel {
			return camelValue
		}
		return false
	}
	return value
}

func truncateForLog(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
