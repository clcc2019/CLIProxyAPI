package usage

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type clientAPIKeyQuotaPlugin struct{}

func init() {
	coreusage.RegisterPlugin(clientAPIKeyQuotaPlugin{})
}

func (clientAPIKeyQuotaPlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	defaultClientAPIKeyQuotaTracker.record(record)
}

// ClientAPIKeyQuotaUsage is the quota-relevant usage already recorded for one API key.
type ClientAPIKeyQuotaUsage struct {
	DailyCost   float64
	MonthlyCost float64
	TotalCost   float64
}

// ClientAPIKeyQuotaExceeded describes the first configured quota limit that has been reached.
type ClientAPIKeyQuotaExceeded struct {
	Scope    string
	Resource string
	Limit    float64
	Used     float64
	ResetAt  time.Time
}

// RetryAfter returns the duration until the exceeded window resets.
func (e *ClientAPIKeyQuotaExceeded) RetryAfter(now time.Time) time.Duration {
	if e == nil || e.ResetAt.IsZero() {
		return 0
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !e.ResetAt.After(now) {
		return 0
	}
	return e.ResetAt.Sub(now)
}

type clientAPIKeyQuotaCounters struct {
	cost float64
}

type clientAPIKeyQuotaTracker struct {
	mu          sync.RWMutex
	modelPrices config.ModelPrices
	total       map[string]clientAPIKeyQuotaCounters
	daily       map[string]map[string]clientAPIKeyQuotaCounters
	monthly     map[string]map[string]clientAPIKeyQuotaCounters
}

type persistedClientAPIKeyQuotaState struct {
	Total   map[string]float64            `json:"total,omitempty"`
	Daily   map[string]map[string]float64 `json:"daily,omitempty"`
	Monthly map[string]map[string]float64 `json:"monthly,omitempty"`
}

var defaultClientAPIKeyQuotaTracker = newClientAPIKeyQuotaTracker()

func newClientAPIKeyQuotaTracker() *clientAPIKeyQuotaTracker {
	return &clientAPIKeyQuotaTracker{
		total:   make(map[string]clientAPIKeyQuotaCounters),
		daily:   make(map[string]map[string]clientAPIKeyQuotaCounters),
		monthly: make(map[string]map[string]clientAPIKeyQuotaCounters),
	}
}

// CheckClientAPIKeyQuota evaluates the configured quota for a client API key.
func CheckClientAPIKeyQuota(apiKey string, quota config.ClientAPIKeyQuota, now time.Time) *ClientAPIKeyQuotaExceeded {
	return defaultClientAPIKeyQuotaTracker.check(apiKey, quota, now)
}

// SetClientAPIKeyQuotaModelPrices updates server-side model prices used for spend quotas.
func SetClientAPIKeyQuotaModelPrices(prices config.ModelPrices) {
	defaultClientAPIKeyQuotaTracker.setModelPrices(prices)
}

func (t *clientAPIKeyQuotaTracker) setModelPrices(prices config.ModelPrices) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.modelPrices = config.EffectiveModelPrices(prices)
}

func (t *clientAPIKeyQuotaTracker) persistedState() persistedClientAPIKeyQuotaState {
	if t == nil {
		return persistedClientAPIKeyQuotaState{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	return persistedClientAPIKeyQuotaState{
		Total:   persistedClientAPIKeyQuotaCounters(t.total),
		Daily:   persistedClientAPIKeyQuotaBuckets(t.daily),
		Monthly: persistedClientAPIKeyQuotaBuckets(t.monthly),
	}
}

func (state persistedClientAPIKeyQuotaState) isZero() bool {
	return len(state.Total) == 0 && len(state.Daily) == 0 && len(state.Monthly) == 0
}

func (t *clientAPIKeyQuotaTracker) restorePersistedState(state persistedClientAPIKeyQuotaState, now time.Time) {
	if t == nil {
		return
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.total = restoredClientAPIKeyQuotaCounters(state.Total)
	t.daily = restoredClientAPIKeyQuotaBuckets(state.Daily)
	t.monthly = restoredClientAPIKeyQuotaBuckets(state.Monthly)
	t.pruneLocked(now)
}

func (t *clientAPIKeyQuotaTracker) record(record coreusage.Record) {
	if t == nil {
		return
	}
	apiKey := strings.TrimSpace(record.APIKey)
	if apiKey == "" {
		return
	}

	timestamp := record.RequestedAt.UTC()
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	cost := t.costForRecordLocked(record)
	if cost <= 0 {
		return
	}

	t.addCountersLocked(t.total, apiKey, "", cost)
	t.addCountersLocked(t.daily, apiKey, timestamp.Format("2006-01-02"), cost)
	t.addCountersLocked(t.monthly, apiKey, timestamp.Format("2006-01"), cost)
	t.pruneLocked(timestamp)
}

func (t *clientAPIKeyQuotaTracker) check(apiKey string, quota config.ClientAPIKeyQuota, now time.Time) *ClientAPIKeyQuotaExceeded {
	if t == nil {
		return nil
	}
	apiKey = strings.TrimSpace(apiKey)
	quota = config.NormalizeClientAPIKeyQuota(quota)
	if apiKey == "" || !quota.HasLimits() {
		return nil
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	usage := t.usage(apiKey, now)
	if quota.TotalCost > 0 && usage.TotalCost >= quota.TotalCost {
		return &ClientAPIKeyQuotaExceeded{Scope: "total", Resource: "cost", Limit: quota.TotalCost, Used: usage.TotalCost}
	}
	if quota.MonthlyCost > 0 && usage.MonthlyCost >= quota.MonthlyCost {
		return &ClientAPIKeyQuotaExceeded{Scope: "monthly", Resource: "cost", Limit: quota.MonthlyCost, Used: usage.MonthlyCost, ResetAt: nextMonthlyResetUTC(now)}
	}
	if quota.DailyCost > 0 && usage.DailyCost >= quota.DailyCost {
		return &ClientAPIKeyQuotaExceeded{Scope: "daily", Resource: "cost", Limit: quota.DailyCost, Used: usage.DailyCost, ResetAt: nextDailyResetUTC(now)}
	}
	return nil
}

func (t *clientAPIKeyQuotaTracker) usage(apiKey string, now time.Time) ClientAPIKeyQuotaUsage {
	if t == nil {
		return ClientAPIKeyQuotaUsage{}
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	total := t.total[apiKey]
	daily := lookupClientAPIKeyQuotaCounters(t.daily, apiKey, now.Format("2006-01-02"))
	monthly := lookupClientAPIKeyQuotaCounters(t.monthly, apiKey, now.Format("2006-01"))
	return ClientAPIKeyQuotaUsage{
		DailyCost:   daily.cost,
		MonthlyCost: monthly.cost,
		TotalCost:   total.cost,
	}
}

func lookupClientAPIKeyQuotaCounters(source map[string]map[string]clientAPIKeyQuotaCounters, apiKey, bucket string) clientAPIKeyQuotaCounters {
	if len(source) == 0 {
		return clientAPIKeyQuotaCounters{}
	}
	buckets := source[apiKey]
	if len(buckets) == 0 {
		return clientAPIKeyQuotaCounters{}
	}
	return buckets[bucket]
}

func (t *clientAPIKeyQuotaTracker) addCountersLocked(source any, apiKey, bucket string, cost float64) {
	switch typed := source.(type) {
	case map[string]clientAPIKeyQuotaCounters:
		current := typed[apiKey]
		current.cost += cost
		typed[apiKey] = current
	case map[string]map[string]clientAPIKeyQuotaCounters:
		buckets := typed[apiKey]
		if buckets == nil {
			buckets = make(map[string]clientAPIKeyQuotaCounters)
			typed[apiKey] = buckets
		}
		current := buckets[bucket]
		current.cost += cost
		buckets[bucket] = current
	}
}

func (t *clientAPIKeyQuotaTracker) costForRecordLocked(record coreusage.Record) float64 {
	if t == nil || len(t.modelPrices) == 0 {
		return 0
	}
	price, ok := config.LookupModelPrice(t.modelPrices, record.Model, record.Alias)
	if !ok {
		return 0
	}
	detail := record.Detail
	cachedTokens := maxInt64(detail.CachedTokens, 0)
	cacheCreationTokens := maxInt64(detail.CacheCreationTokens, 0)
	inputTokens := maxInt64(detail.InputTokens, 0)
	if minimumInputTokens := cachedTokens + cacheCreationTokens; inputTokens < minimumInputTokens {
		inputTokens = minimumInputTokens
	}
	outputTokens := maxInt64(detail.OutputTokens, 0)
	promptTokens := inputTokens - cachedTokens
	if promptTokens < 0 {
		promptTokens = 0
	}
	const tokensPerPriceUnit = 1_000_000
	cost := (float64(promptTokens)/tokensPerPriceUnit)*price.Prompt +
		(float64(cachedTokens)/tokensPerPriceUnit)*price.Cache +
		(float64(outputTokens)/tokensPerPriceUnit)*price.Completion
	if cost <= 0 {
		return 0
	}
	return cost
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}

func (t *clientAPIKeyQuotaTracker) pruneLocked(reference time.Time) {
	if t == nil {
		return
	}
	reference = reference.UTC()
	if reference.IsZero() {
		reference = time.Now().UTC()
	}
	dailyCutoff := reference.AddDate(0, 0, -2).Format("2006-01-02")
	monthlyCutoff := time.Date(reference.Year(), reference.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -2, 0).Format("2006-01")
	pruneClientAPIKeyQuotaBuckets(t.daily, dailyCutoff)
	pruneClientAPIKeyQuotaBuckets(t.monthly, monthlyCutoff)
}

func pruneClientAPIKeyQuotaBuckets(source map[string]map[string]clientAPIKeyQuotaCounters, cutoff string) {
	for apiKey, buckets := range source {
		for bucket := range buckets {
			if bucket < cutoff {
				delete(buckets, bucket)
			}
		}
		if len(buckets) == 0 {
			delete(source, apiKey)
		}
	}
}

func persistedClientAPIKeyQuotaCounters(source map[string]clientAPIKeyQuotaCounters) map[string]float64 {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]float64, len(source))
	for apiKey, counters := range source {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" || counters.cost <= 0 {
			continue
		}
		out[apiKey] = counters.cost
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func persistedClientAPIKeyQuotaBuckets(source map[string]map[string]clientAPIKeyQuotaCounters) map[string]map[string]float64 {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]map[string]float64, len(source))
	for apiKey, buckets := range source {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" || len(buckets) == 0 {
			continue
		}
		persistedBuckets := make(map[string]float64, len(buckets))
		for bucket, counters := range buckets {
			bucket = strings.TrimSpace(bucket)
			if bucket == "" || counters.cost <= 0 {
				continue
			}
			persistedBuckets[bucket] = counters.cost
		}
		if len(persistedBuckets) > 0 {
			out[apiKey] = persistedBuckets
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func restoredClientAPIKeyQuotaCounters(source map[string]float64) map[string]clientAPIKeyQuotaCounters {
	out := make(map[string]clientAPIKeyQuotaCounters, len(source))
	for apiKey, cost := range source {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" || cost <= 0 {
			continue
		}
		out[apiKey] = clientAPIKeyQuotaCounters{cost: cost}
	}
	return out
}

func restoredClientAPIKeyQuotaBuckets(source map[string]map[string]float64) map[string]map[string]clientAPIKeyQuotaCounters {
	out := make(map[string]map[string]clientAPIKeyQuotaCounters, len(source))
	for apiKey, buckets := range source {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" || len(buckets) == 0 {
			continue
		}
		restoredBuckets := make(map[string]clientAPIKeyQuotaCounters, len(buckets))
		for bucket, cost := range buckets {
			bucket = strings.TrimSpace(bucket)
			if bucket == "" || cost <= 0 {
				continue
			}
			restoredBuckets[bucket] = clientAPIKeyQuotaCounters{cost: cost}
		}
		if len(restoredBuckets) > 0 {
			out[apiKey] = restoredBuckets
		}
	}
	return out
}

func nextDailyResetUTC(now time.Time) time.Time {
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

func nextMonthlyResetUTC(now time.Time) time.Time {
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
}
