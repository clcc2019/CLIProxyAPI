package management

import (
	"context"
	"encoding/json"
	"time"

	log "github.com/sirupsen/logrus"
)

const managementCacheTimeout = 2 * time.Second

func (h *Handler) cacheStoreSnapshot() CacheStore {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	store := h.cacheStore
	h.mu.Unlock()
	return store
}

func (h *Handler) loadCacheJSON(ctx context.Context, namespace, key string, dst any) (bool, error) {
	store := h.cacheStoreSnapshot()
	if store == nil || dst == nil {
		return false, nil
	}
	cacheCtx, cancel := managementCacheContext(ctx)
	defer cancel()
	data, ok, err := store.LoadCache(cacheCtx, namespace, key)
	if err != nil || !ok {
		return ok, err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		deleteCtx, deleteCancel := managementCacheContext(context.Background())
		_ = store.DeleteCache(deleteCtx, namespace, key)
		deleteCancel()
		return false, err
	}
	return true, nil
}

func (h *Handler) saveCacheJSON(ctx context.Context, namespace, key string, value any, ttl time.Duration) error {
	store := h.cacheStoreSnapshot()
	if store == nil || value == nil || ttl <= 0 {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	cacheCtx, cancel := managementCacheContext(ctx)
	defer cancel()
	return store.SaveCache(cacheCtx, namespace, key, data, ttl)
}

func (h *Handler) deleteCache(ctx context.Context, namespace, key string) error {
	store := h.cacheStoreSnapshot()
	if store == nil {
		return nil
	}
	cacheCtx, cancel := managementCacheContext(ctx)
	defer cancel()
	return store.DeleteCache(cacheCtx, namespace, key)
}

func managementCacheContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, managementCacheTimeout)
}

func logManagementCacheDebug(err error, namespace string) {
	if err == nil {
		return
	}
	log.WithError(err).WithField("cache_namespace", namespace).Debug("management redis cache unavailable")
}
