package management

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	"golang.org/x/sync/singleflight"
)

// aggregatedUsageCacheTTL bounds the age of rate-based windows. Normal usage
// writes invalidate the cache sooner through RequestStatistics' revision.
const aggregatedUsageCacheTTL = time.Second

type aggregatedUsageResponse struct {
	Usage          usage.AggregatedUsageSnapshot `json:"usage"`
	FailedRequests int64                         `json:"failed_requests"`
}

type aggregatedUsageCacheEntry struct {
	stats     *usage.RequestStatistics
	revision  uint64
	expiresAt time.Time
	payload   []byte
}

// aggregatedUsageCache stores a single response because a Handler serves one
// usage statistics instance. Keeping the JSON bytes private to the handler
// avoids exposing a cache of mutable map/slice-backed snapshots.
type aggregatedUsageCache struct {
	mu      sync.RWMutex
	entry   aggregatedUsageCacheEntry
	flights singleflight.Group
}

func (c *aggregatedUsageCache) load(stats *usage.RequestStatistics, revision uint64, now time.Time) ([]byte, bool) {
	if c == nil || stats == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry := c.entry
	if entry.stats != stats || entry.revision != revision || !now.Before(entry.expiresAt) || len(entry.payload) == 0 {
		return nil, false
	}
	return entry.payload, true
}

func (c *aggregatedUsageCache) store(stats *usage.RequestStatistics, revision uint64, expiresAt time.Time, payload []byte) {
	if c == nil || stats == nil || len(payload) == 0 {
		return
	}
	c.mu.Lock()
	c.entry = aggregatedUsageCacheEntry{
		stats:     stats,
		revision:  revision,
		expiresAt: expiresAt,
		payload:   payload,
	}
	c.mu.Unlock()
}

func (h *Handler) aggregatedUsageHandlerCache() *aggregatedUsageCache {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	cache := h.aggregatedUsageCache
	h.mu.RUnlock()
	if cache != nil {
		return cache
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.aggregatedUsageCache == nil {
		h.aggregatedUsageCache = &aggregatedUsageCache{}
	}
	return h.aggregatedUsageCache
}

func (h *Handler) aggregatedUsageResponse(now time.Time) ([]byte, error) {
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	stats := h.usageStatisticsSnapshot()
	if stats == nil {
		return marshalAggregatedUsageResponse(usage.AggregatedUsageSnapshot{
			GeneratedAt: now,
			Windows:     map[string]usage.AggregatedUsageWindow{},
		})
	}

	cache := h.aggregatedUsageHandlerCache()
	revision := stats.AggregatedUsageRevision()
	if payload, ok := cache.load(stats, revision, now); ok {
		return payload, nil
	}

	// A dashboard can have several polling clients. Collapse cache misses so
	// only one request builds the allocation-heavy aggregate snapshot.
	flightKey := fmt.Sprintf("%p:%d", stats, revision)
	value, err, _ := cache.flights.Do(flightKey, func() (any, error) {
		if payload, ok := cache.load(stats, revision, now); ok {
			return payload, nil
		}

		snapshot, snapshotRevision := stats.AggregatedUsageSnapshotWithRevision(now)
		payload, err := marshalAggregatedUsageResponse(snapshot)
		if err != nil {
			return nil, err
		}
		cache.store(stats, snapshotRevision, now.Add(aggregatedUsageCacheTTL), payload)
		return payload, nil
	})
	if err != nil {
		return nil, err
	}
	payload, _ := value.([]byte)
	return payload, nil
}

func marshalAggregatedUsageResponse(snapshot usage.AggregatedUsageSnapshot) ([]byte, error) {
	failedRequests := int64(0)
	if allWindow, ok := snapshot.Windows["all"]; ok {
		failedRequests = allWindow.FailureCount
	}
	return json.Marshal(aggregatedUsageResponse{
		Usage:          snapshot,
		FailedRequests: failedRequests,
	})
}
