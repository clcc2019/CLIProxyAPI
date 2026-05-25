package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

const (
	codexUsageCacheNamespace  = "codex_usage"
	codexUsageCacheDefaultTTL = 30 * time.Second
	codexUsageCacheMaxTTL     = 5 * time.Minute
	codexUsageCacheStaleTTL   = 5 * time.Minute
)

type codexUsageCacheEntry struct {
	Payload    gin.H     `json:"payload,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	StaleUntil time.Time `json:"stale_until"`
}

type codexUsageCache struct {
	entries sync.Map // cache key -> *codexUsageCacheEntry
	flights singleflight.Group
}

type codexUsageRequestOptions struct {
	force bool
	ttl   time.Duration
}

func (c *codexUsageCache) load(key string, now time.Time, allowStale bool) (gin.H, bool, bool) {
	if c == nil || key == "" {
		return nil, false, false
	}
	value, ok := c.entries.Load(key)
	if !ok {
		return nil, false, false
	}
	entry, ok := value.(*codexUsageCacheEntry)
	if !ok || entry == nil || len(entry.Payload) == 0 {
		return nil, false, false
	}
	staleUntil := entry.StaleUntil
	if staleUntil.IsZero() {
		staleUntil = entry.ExpiresAt
	}
	if now.Before(entry.ExpiresAt) {
		return cloneGinH(entry.Payload), false, true
	}
	if allowStale && now.Before(staleUntil) {
		return cloneGinH(entry.Payload), true, true
	}
	if !now.Before(staleUntil) {
		c.entries.Delete(key)
	}
	return nil, false, false
}

func (c *codexUsageCache) store(key string, entry *codexUsageCacheEntry) {
	if c == nil || key == "" || entry == nil || len(entry.Payload) == 0 {
		return
	}
	c.entries.Store(key, entry)
}

func (h *Handler) codexUsageHandlerCache() *codexUsageCache {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.codexUsageCache == nil {
		h.codexUsageCache = &codexUsageCache{}
	}
	return h.codexUsageCache
}

func parseCodexUsageRequestOptions(c *gin.Context) codexUsageRequestOptions {
	opts := codexUsageRequestOptions{ttl: codexUsageCacheDefaultTTL}
	if c == nil {
		return opts
	}
	switch strings.ToLower(strings.TrimSpace(c.Query("force"))) {
	case "1", "true", "yes", "on":
		opts.force = true
	}
	switch strings.ToLower(strings.TrimSpace(firstNonEmptyQueryValue(c, "codex_usage", "codexUsage"))) {
	case "refresh", "force", "fetch", "1", "true", "yes", "on":
		opts.force = true
	}
	if raw := strings.TrimSpace(c.Query("ttl")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil {
			if seconds <= 0 {
				opts.force = true
			} else {
				ttl := time.Duration(seconds) * time.Second
				if ttl > codexUsageCacheMaxTTL {
					ttl = codexUsageCacheMaxTTL
				}
				opts.ttl = ttl
			}
		}
	}
	return opts
}

func (h *Handler) fetchCodexUsageWithCache(ctx context.Context, auth *coreauth.Auth, opts codexUsageRequestOptions) (gin.H, int, error) {
	if h == nil {
		return nil, 0, fmt.Errorf("handler not initialized")
	}
	cacheKey := h.codexUsageCacheKey(auth)
	cache := h.codexUsageHandlerCache()
	now := time.Now()
	if cacheKey != "" && !opts.force {
		if payload, _, ok := h.loadCodexUsageCache(ctx, cache, cacheKey, now, false); ok {
			return payload, http.StatusOK, nil
		}
	}
	if cacheKey == "" || cache == nil {
		return h.fetchCodexUsage(ctx, auth)
	}

	flightKey := cacheKey
	if opts.force {
		flightKey += "|force"
	}
	type result struct {
		payload gin.H
		status  int
		err     error
	}
	value, err, _ := cache.flights.Do(flightKey, func() (any, error) {
		if !opts.force {
			if payload, _, ok := h.loadCodexUsageCache(ctx, cache, cacheKey, time.Now(), false); ok {
				return result{payload: payload, status: http.StatusOK}, nil
			}
		}
		payload, status, fetchErr := h.fetchCodexUsage(ctx, auth)
		if fetchErr == nil {
			h.storeCodexUsageCache(ctx, cache, cacheKey, payload, opts.ttl)
			return result{payload: payload, status: status}, nil
		}
		if codexUsageTransientFailure(status, fetchErr) {
			if stale, _, ok := h.loadCodexUsageCache(ctx, cache, cacheKey, time.Now(), true); ok {
				return result{payload: markCodexUsageStale(stale, fetchErr, status), status: http.StatusOK}, nil
			}
		}
		return result{status: status, err: fetchErr}, nil
	})
	if err != nil {
		return nil, 0, err
	}
	res, ok := value.(result)
	if !ok {
		return nil, 0, fmt.Errorf("codex usage: invalid singleflight result")
	}
	return res.payload, res.status, res.err
}

func (h *Handler) loadCodexUsageCache(ctx context.Context, cache *codexUsageCache, key string, now time.Time, allowStale bool) (gin.H, bool, bool) {
	if cache != nil {
		if payload, stale, ok := cache.load(key, now, allowStale); ok {
			return payload, stale, true
		}
	}
	if key == "" {
		return nil, false, false
	}
	var entry codexUsageCacheEntry
	ok, err := h.loadCacheJSON(ctx, codexUsageCacheNamespace, key, &entry)
	if err != nil {
		logManagementCacheDebug(err, codexUsageCacheNamespace)
		return nil, false, false
	}
	if !ok || len(entry.Payload) == 0 {
		return nil, false, false
	}
	if entry.StaleUntil.IsZero() {
		entry.StaleUntil = entry.ExpiresAt
	}
	if now.Before(entry.ExpiresAt) {
		if cache != nil {
			cache.store(key, &entry)
		}
		return cloneGinH(entry.Payload), false, true
	}
	if allowStale && now.Before(entry.StaleUntil) {
		if cache != nil {
			cache.store(key, &entry)
		}
		return cloneGinH(entry.Payload), true, true
	}
	if !now.Before(entry.StaleUntil) {
		if err := h.deleteCache(ctx, codexUsageCacheNamespace, key); err != nil {
			logManagementCacheDebug(err, codexUsageCacheNamespace)
		}
	}
	return nil, false, false
}

func (h *Handler) storeCodexUsageCache(ctx context.Context, cache *codexUsageCache, key string, payload gin.H, ttl time.Duration) {
	if key == "" || len(payload) == 0 || ttl <= 0 {
		return
	}
	if ttl > codexUsageCacheMaxTTL {
		ttl = codexUsageCacheMaxTTL
	}
	now := time.Now()
	entry := &codexUsageCacheEntry{
		Payload:    cloneGinH(payload),
		ExpiresAt:  now.Add(ttl),
		StaleUntil: now.Add(codexUsageCacheStaleTTL),
	}
	if entry.StaleUntil.Before(entry.ExpiresAt) {
		entry.StaleUntil = entry.ExpiresAt
	}
	if cache != nil {
		cache.store(key, entry)
	}
	if err := h.saveCacheJSON(ctx, codexUsageCacheNamespace, key, entry, time.Until(entry.StaleUntil)); err != nil {
		logManagementCacheDebug(err, codexUsageCacheNamespace)
	}
}

func (h *Handler) codexUsageCacheKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	accessToken := codexUsageAccessToken(auth)
	accountID := resolveCodexUsageAccountID(auth, accessToken)
	parts := make([]string, 0, 6)
	add := func(label string, value string) {
		label = strings.TrimSpace(label)
		value = strings.TrimSpace(value)
		if label == "" {
			return
		}
		if value != "" {
			parts = append(parts, label+"="+value)
		}
	}
	add("account", accountID)
	if accountID == "" && strings.TrimSpace(accessToken) != "" {
		add("token", codexUsageTokenFingerprint(accessToken))
	}
	add("auth", auth.ID)
	add("file", auth.FileName)
	add("fedramp", strconv.FormatBool(codexUsageFedramp(auth)))
	if h != nil {
		add("proxy", h.codexSubscriptionProxyURL(auth))
	}
	if len(parts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func codexUsageTokenFingerprint(accessToken string) string {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(accessToken))
	return hex.EncodeToString(sum[:])
}

func markCodexUsageStale(payload gin.H, err error, upstreamStatus int) gin.H {
	if payload == nil {
		payload = gin.H{}
	}
	payload = cloneGinH(payload)
	payload["codex_usage_stale"] = true
	payload["codex_usage_cache"] = "stale"
	if upstreamStatus > 0 {
		payload["codex_usage_upstream_status"] = upstreamStatus
	}
	if err != nil {
		payload["codex_usage_error"] = err.Error()
	}
	return payload
}

func codexUsageUnavailablePayload(err error, upstreamStatus int) gin.H {
	payload := gin.H{
		"codex_usage_unavailable": true,
		"rate_limit":              gin.H{},
		"credits":                 gin.H{"unavailable": true},
	}
	if upstreamStatus > 0 {
		payload["codex_usage_upstream_status"] = upstreamStatus
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	return payload
}
