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

type aggregatedUsageCredentialsResponse struct {
	Usage struct {
		GeneratedAt time.Time                                   `json:"generated_at"`
		Windows     map[string]aggregatedUsageCredentialsWindow `json:"windows"`
	} `json:"usage"`
}

type aggregatedUsageCredentialsWindow struct {
	Credentials []usage.AggregatedUsageCredentialStats `json:"credentials"`
}

type aggregatedUsageResponseOptions struct {
	Window string
	Fields string
}

type aggregatedUsageCacheEntry struct {
	stats     *usage.RequestStatistics
	revision  uint64
	expiresAt time.Time
	snapshot  usage.AggregatedUsageSnapshot
	payloads  map[string][]byte
}

// aggregatedUsageCache stores one immutable snapshot and its encoded window
// variants because a Handler serves one usage statistics instance.
type aggregatedUsageCache struct {
	mu      sync.RWMutex
	entry   aggregatedUsageCacheEntry
	flights singleflight.Group
}

func (c *aggregatedUsageCache) load(stats *usage.RequestStatistics, revision uint64, now time.Time, variant string) ([]byte, bool) {
	if c == nil || stats == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry := c.entry
	payload := entry.payloads[variant]
	if entry.stats != stats || entry.revision != revision || !now.Before(entry.expiresAt) || len(payload) == 0 {
		return nil, false
	}
	return payload, true
}

func (c *aggregatedUsageCache) loadSnapshot(stats *usage.RequestStatistics, revision uint64, now time.Time) (usage.AggregatedUsageSnapshot, bool) {
	if c == nil || stats == nil {
		return usage.AggregatedUsageSnapshot{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry := c.entry
	if entry.stats != stats || entry.revision != revision || !now.Before(entry.expiresAt) || entry.snapshot.Windows == nil {
		return usage.AggregatedUsageSnapshot{}, false
	}
	return entry.snapshot, true
}

func (c *aggregatedUsageCache) store(stats *usage.RequestStatistics, revision uint64, expiresAt time.Time, snapshot usage.AggregatedUsageSnapshot, variant string, payload []byte) {
	if c == nil || stats == nil || len(payload) == 0 {
		return
	}
	c.mu.Lock()
	c.entry = aggregatedUsageCacheEntry{
		stats:     stats,
		revision:  revision,
		expiresAt: expiresAt,
		snapshot:  snapshot,
		payloads:  map[string][]byte{variant: payload},
	}
	c.mu.Unlock()
}

func (c *aggregatedUsageCache) storePayload(stats *usage.RequestStatistics, revision uint64, now time.Time, variant string, payload []byte) {
	if c == nil || stats == nil || len(payload) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entry.stats != stats || c.entry.revision != revision || !now.Before(c.entry.expiresAt) {
		return
	}
	c.entry.payloads[variant] = payload
}

func (c *aggregatedUsageCache) storeVariant(stats *usage.RequestStatistics, revision uint64, now, expiresAt time.Time, variant string, payload []byte) {
	if c == nil || stats == nil || len(payload) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entry.stats == stats && c.entry.revision == revision && now.Before(c.entry.expiresAt) {
		c.entry.payloads[variant] = payload
		return
	}
	c.entry = aggregatedUsageCacheEntry{
		stats:     stats,
		revision:  revision,
		expiresAt: expiresAt,
		payloads:  map[string][]byte{variant: payload},
	}
}

func (h *Handler) aggregatedUsageHandlerCache() *aggregatedUsageCache {
	if h == nil {
		return nil
	}
	return &h.aggregatedUsageCache
}

func (h *Handler) aggregatedUsageResponse(now time.Time, requestedOptions ...aggregatedUsageResponseOptions) ([]byte, error) {
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	options := aggregatedUsageResponseOptions{}
	if len(requestedOptions) > 0 {
		options = requestedOptions[0]
	}
	variant := options.Window + "\x00" + options.Fields

	stats := h.usageStatisticsSnapshot()
	if stats == nil {
		return marshalAggregatedUsageResponse(usage.AggregatedUsageSnapshot{
			GeneratedAt: now,
			Windows:     map[string]usage.AggregatedUsageWindow{},
		}, options)
	}

	cache := h.aggregatedUsageHandlerCache()
	revision := stats.AggregatedUsageRevision()
	if payload, ok := cache.load(stats, revision, now, variant); ok {
		return payload, nil
	}

	// A dashboard can have several polling clients. Collapse cache misses for
	// the same response variant.
	flightKey := fmt.Sprintf("%p:%d:%s", stats, revision, variant)
	value, err, _ := cache.flights.Do(flightKey, func() (any, error) {
		if payload, ok := cache.load(stats, revision, now, variant); ok {
			return payload, nil
		}
		if options.Window == "all" && options.Fields == "credentials" {
			snapshot, snapshotRevision := stats.AggregatedUsageCredentialsSnapshotWithRevision(now)
			payload, err := marshalAggregatedUsageCredentialsResponse(snapshot)
			if err != nil {
				return nil, err
			}
			cache.storeVariant(stats, snapshotRevision, now, now.Add(aggregatedUsageCacheTTL), variant, payload)
			return payload, nil
		}

		snapshot, ok := cache.loadSnapshot(stats, revision, now)
		snapshotRevision := revision
		if !ok {
			snapshot, snapshotRevision = stats.AggregatedUsageSnapshotWithRevision(now)
		}
		payload, err := marshalAggregatedUsageResponse(snapshot, options)
		if err != nil {
			return nil, err
		}
		if ok {
			cache.storePayload(stats, snapshotRevision, now, variant, payload)
		} else {
			cache.store(stats, snapshotRevision, now.Add(aggregatedUsageCacheTTL), snapshot, variant, payload)
		}
		return payload, nil
	})
	if err != nil {
		return nil, err
	}
	payload, _ := value.([]byte)
	return payload, nil
}

func marshalAggregatedUsageCredentialsResponse(snapshot usage.AggregatedUsageCredentialsSnapshot) ([]byte, error) {
	response := aggregatedUsageCredentialsResponse{}
	response.Usage.GeneratedAt = snapshot.GeneratedAt
	response.Usage.Windows = map[string]aggregatedUsageCredentialsWindow{
		"all": {Credentials: snapshot.Credentials},
	}
	return json.Marshal(response)
}

func marshalAggregatedUsageResponse(snapshot usage.AggregatedUsageSnapshot, options aggregatedUsageResponseOptions) ([]byte, error) {
	if options.Fields == "credentials" {
		response := aggregatedUsageCredentialsResponse{}
		response.Usage.GeneratedAt = snapshot.GeneratedAt
		response.Usage.Windows = make(map[string]aggregatedUsageCredentialsWindow)
		for key, selected := range snapshot.Windows {
			if options.Window == "" || key == options.Window {
				response.Usage.Windows[key] = aggregatedUsageCredentialsWindow{Credentials: selected.Credentials}
			}
		}
		return json.Marshal(response)
	}
	failedRequests := int64(0)
	if allWindow, ok := snapshot.Windows["all"]; ok {
		failedRequests = allWindow.FailureCount
	}
	if options.Window != "" {
		selected, ok := snapshot.Windows[options.Window]
		snapshot.Windows = map[string]usage.AggregatedUsageWindow{}
		snapshot.ModelNames = nil
		if ok {
			snapshot.Windows[options.Window] = selected
			snapshot.ModelNames = append([]string(nil), selected.ModelNames...)
		}
	}
	return json.Marshal(aggregatedUsageResponse{
		Usage:          snapshot,
		FailedRequests: failedRequests,
	})
}
