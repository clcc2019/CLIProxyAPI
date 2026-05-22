package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const codexSubscriptionCacheNamespace = "codex_subscription"

type codexSubscriptionCacheEntryWire struct {
	Info      codexAccountSubscriptionInfo `json:"info,omitempty"`
	Found     bool                         `json:"found"`
	ExpiresAt time.Time                    `json:"expires_at"`
}

func (e codexSubscriptionCacheEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(codexSubscriptionCacheEntryWire{
		Info:      e.info,
		Found:     e.found,
		ExpiresAt: e.expiresAt,
	})
}

func (e *codexSubscriptionCacheEntry) UnmarshalJSON(data []byte) error {
	var wire codexSubscriptionCacheEntryWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	e.info = wire.Info
	e.found = wire.Found
	e.expiresAt = wire.ExpiresAt
	return nil
}

func (h *Handler) loadCodexSubscriptionCache(ctx context.Context, auth *coreauth.Auth, tokenCacheKey string, now time.Time) (codexSubscriptionCacheEntry, bool) {
	keys := codexSubscriptionCacheKeys(auth, tokenCacheKey)
	for _, key := range keys {
		if cached, ok := loadCodexSubscriptionMemoryCache(key, now); ok {
			return cached, true
		}
	}
	for _, key := range keys {
		var cached codexSubscriptionCacheEntry
		ok, err := h.loadCacheJSON(ctx, codexSubscriptionCacheNamespace, key, &cached)
		if err != nil {
			logManagementCacheDebug(err, codexSubscriptionCacheNamespace)
			continue
		}
		if !ok {
			continue
		}
		if cached.expiresAt.After(now) {
			codexSubscriptionCache.Store(key, cached)
			return cached, true
		}
		codexSubscriptionCache.Delete(key)
		if err := h.deleteCache(ctx, codexSubscriptionCacheNamespace, key); err != nil {
			logManagementCacheDebug(err, codexSubscriptionCacheNamespace)
		}
	}
	return codexSubscriptionCacheEntry{}, false
}

func loadCodexSubscriptionMemoryCache(key string, now time.Time) (codexSubscriptionCacheEntry, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return codexSubscriptionCacheEntry{}, false
	}
	cachedRaw, ok := codexSubscriptionCache.Load(key)
	if !ok {
		return codexSubscriptionCacheEntry{}, false
	}
	cached, _ := cachedRaw.(codexSubscriptionCacheEntry)
	if cached.expiresAt.After(now) {
		return cached, true
	}
	codexSubscriptionCache.Delete(key)
	return codexSubscriptionCacheEntry{}, false
}

func (h *Handler) storeCodexSubscriptionCache(ctx context.Context, auth *coreauth.Auth, tokenCacheKey string, entry codexSubscriptionCacheEntry) {
	ttl := time.Until(entry.expiresAt)
	if ttl <= 0 {
		return
	}
	for _, key := range codexSubscriptionCacheKeys(auth, tokenCacheKey) {
		codexSubscriptionCache.Store(key, entry)
		if err := h.saveCacheJSON(ctx, codexSubscriptionCacheNamespace, key, entry, ttl); err != nil {
			logManagementCacheDebug(err, codexSubscriptionCacheNamespace)
		}
	}
}

func codexSubscriptionCacheKeys(auth *coreauth.Auth, tokenCacheKey string) []string {
	keys := make([]string, 0, 2)
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		for _, existing := range keys {
			if existing == key {
				return
			}
		}
		keys = append(keys, key)
	}
	add(tokenCacheKey)
	add(codexSubscriptionAuthCacheKey(auth))
	return keys
}

func codexSubscriptionAuthCacheKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, strings.ToLower(value))
		}
	}
	add(auth.ID)
	add(auth.FileName)
	add(authEmail(auth))
	if len(parts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "auth:" + hex.EncodeToString(sum[:])
}
