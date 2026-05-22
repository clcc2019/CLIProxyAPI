package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
)

const kiroUsageCacheNamespace = "kiro_usage"

type kiroUsageCacheWire struct {
	Usage     *kiroauth.KiroUsageInfo `json:"usage,omitempty"`
	ExpiresAt time.Time               `json:"expires_at"`
}

func (h *Handler) loadKiroUsageCache(ctx context.Context, cache *kiroUsageCache, authID string, now time.Time) (*kiroauth.KiroUsageInfo, bool) {
	if cache != nil {
		if usage, ok := cache.load(authID, now); ok {
			return usage, true
		}
	}
	key := kiroUsageCacheKey(authID)
	if key == "" {
		return nil, false
	}
	var wire kiroUsageCacheWire
	ok, err := h.loadCacheJSON(ctx, kiroUsageCacheNamespace, key, &wire)
	if err != nil {
		logManagementCacheDebug(err, kiroUsageCacheNamespace)
		return nil, false
	}
	if !ok || wire.Usage == nil {
		return nil, false
	}
	if !now.Before(wire.ExpiresAt) {
		if err := h.deleteCache(ctx, kiroUsageCacheNamespace, key); err != nil {
			logManagementCacheDebug(err, kiroUsageCacheNamespace)
		}
		return nil, false
	}
	if cache != nil {
		cache.entries.Store(authID, &kiroUsageCacheEntry{usage: wire.Usage, expiresAt: wire.ExpiresAt})
	}
	return wire.Usage, true
}

func (h *Handler) storeKiroUsageCache(ctx context.Context, cache *kiroUsageCache, authID string, usage *kiroauth.KiroUsageInfo, ttl time.Duration) {
	if authID == "" || usage == nil || ttl <= 0 {
		return
	}
	expiresAt := time.Now().Add(ttl)
	if cache != nil {
		cache.entries.Store(authID, &kiroUsageCacheEntry{usage: usage, expiresAt: expiresAt})
	}
	key := kiroUsageCacheKey(authID)
	if key == "" {
		return
	}
	if err := h.saveCacheJSON(ctx, kiroUsageCacheNamespace, key, kiroUsageCacheWire{Usage: usage, ExpiresAt: expiresAt}, ttl); err != nil {
		logManagementCacheDebug(err, kiroUsageCacheNamespace)
	}
}

func (h *Handler) invalidateKiroUsageCache(ctx context.Context, cache *kiroUsageCache, authID string) {
	if cache != nil {
		cache.invalidate(authID)
	}
	key := kiroUsageCacheKey(authID)
	if key == "" {
		return
	}
	if err := h.deleteCache(ctx, kiroUsageCacheNamespace, key); err != nil {
		logManagementCacheDebug(err, kiroUsageCacheNamespace)
	}
}

func kiroUsageCacheKey(authID string) string {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(authID)))
	return hex.EncodeToString(sum[:])
}
