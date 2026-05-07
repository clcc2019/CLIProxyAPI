package usage

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
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
	DailyRequests   int64
	MonthlyRequests int64
	TotalRequests   int64
	DailyTokens     int64
	MonthlyTokens   int64
	TotalTokens     int64
}

// ClientAPIKeyQuotaExceeded describes the first configured quota limit that has been reached.
type ClientAPIKeyQuotaExceeded struct {
	Scope    string
	Resource string
	Limit    int64
	Used     int64
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
	requests int64
	tokens   int64
}

type clientAPIKeyQuotaTracker struct {
	mu      sync.RWMutex
	total   map[string]clientAPIKeyQuotaCounters
	daily   map[string]map[string]clientAPIKeyQuotaCounters
	monthly map[string]map[string]clientAPIKeyQuotaCounters
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
	tokens := normaliseDetail(record.Detail).TotalTokens
	if tokens < 0 {
		tokens = 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.addCountersLocked(t.total, apiKey, "", 1, tokens)
	t.addCountersLocked(t.daily, apiKey, timestamp.Format("2006-01-02"), 1, tokens)
	t.addCountersLocked(t.monthly, apiKey, timestamp.Format("2006-01"), 1, tokens)
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
	if quota.TotalRequests > 0 && usage.TotalRequests >= quota.TotalRequests {
		return &ClientAPIKeyQuotaExceeded{Scope: "total", Resource: "requests", Limit: quota.TotalRequests, Used: usage.TotalRequests}
	}
	if quota.TotalTokens > 0 && usage.TotalTokens >= quota.TotalTokens {
		return &ClientAPIKeyQuotaExceeded{Scope: "total", Resource: "tokens", Limit: quota.TotalTokens, Used: usage.TotalTokens}
	}
	if quota.MonthlyRequests > 0 && usage.MonthlyRequests >= quota.MonthlyRequests {
		return &ClientAPIKeyQuotaExceeded{Scope: "monthly", Resource: "requests", Limit: quota.MonthlyRequests, Used: usage.MonthlyRequests, ResetAt: nextMonthlyResetUTC(now)}
	}
	if quota.MonthlyTokens > 0 && usage.MonthlyTokens >= quota.MonthlyTokens {
		return &ClientAPIKeyQuotaExceeded{Scope: "monthly", Resource: "tokens", Limit: quota.MonthlyTokens, Used: usage.MonthlyTokens, ResetAt: nextMonthlyResetUTC(now)}
	}
	if quota.DailyRequests > 0 && usage.DailyRequests >= quota.DailyRequests {
		return &ClientAPIKeyQuotaExceeded{Scope: "daily", Resource: "requests", Limit: quota.DailyRequests, Used: usage.DailyRequests, ResetAt: nextDailyResetUTC(now)}
	}
	if quota.DailyTokens > 0 && usage.DailyTokens >= quota.DailyTokens {
		return &ClientAPIKeyQuotaExceeded{Scope: "daily", Resource: "tokens", Limit: quota.DailyTokens, Used: usage.DailyTokens, ResetAt: nextDailyResetUTC(now)}
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
		DailyRequests:   daily.requests,
		MonthlyRequests: monthly.requests,
		TotalRequests:   total.requests,
		DailyTokens:     daily.tokens,
		MonthlyTokens:   monthly.tokens,
		TotalTokens:     total.tokens,
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

func (t *clientAPIKeyQuotaTracker) addCountersLocked(source any, apiKey, bucket string, requests, tokens int64) {
	switch typed := source.(type) {
	case map[string]clientAPIKeyQuotaCounters:
		current := typed[apiKey]
		current.requests += requests
		current.tokens += tokens
		typed[apiKey] = current
	case map[string]map[string]clientAPIKeyQuotaCounters:
		buckets := typed[apiKey]
		if buckets == nil {
			buckets = make(map[string]clientAPIKeyQuotaCounters)
			typed[apiKey] = buckets
		}
		current := buckets[bucket]
		current.requests += requests
		current.tokens += tokens
		buckets[bucket] = current
	}
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
