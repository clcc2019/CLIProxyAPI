package usage

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

type clientAPIKeyQuotaPlugin struct{}

func init() {
	coreusage.RegisterPlugin(clientAPIKeyQuotaPlugin{})
}

func (clientAPIKeyQuotaPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	clientAPIKeyQuotaPlugin{}.HandleUsageBatch([]coreusage.Item{{
		Context: ctx,
		Record:  record,
	}})
}

func (clientAPIKeyQuotaPlugin) HandleUsageBatch(items []coreusage.Item) {
	if len(items) == 0 {
		return
	}
	store := clientAPIKeyQuotaStoreSnapshot()
	adds := defaultClientAPIKeyQuotaTracker.recordBatch(items, store != nil)
	var pending map[clientAPIKeyQuotaStoreAddKey]clientAPIKeyQuotaStoreAdd
	for _, addResult := range adds {
		key := clientAPIKeyQuotaStoreAddKey{
			apiKey: addResult.apiKey,
			day:    addResult.day,
			month:  addResult.month,
		}
		if pending == nil {
			pending = make(map[clientAPIKeyQuotaStoreAddKey]clientAPIKeyQuotaStoreAdd, 1)
		}
		add := pending[key]
		if add.timestamp.IsZero() || addResult.timestamp.Before(add.timestamp) {
			add.timestamp = addResult.timestamp
		}
		add.cost += addResult.cost
		pending[key] = add
	}
	if len(pending) == 0 || store == nil {
		return
	}
	ctx := clientAPIKeyQuotaBatchContext(items)
	storeCtx, cancel := clientAPIKeyQuotaStoreContext(ctx)
	defer cancel()
	for key, add := range pending {
		if err := store.AddClientAPIKeyQuotaUsage(storeCtx, key.apiKey, add.timestamp, add.cost); err != nil {
			log.WithError(err).Debug("client api key quota redis update failed")
		}
	}
}

type clientAPIKeyQuotaStoreAddKey struct {
	apiKey string
	day    string
	month  string
}

type clientAPIKeyQuotaStoreAdd struct {
	timestamp time.Time
	cost      float64
}

type clientAPIKeyQuotaRecordAdd struct {
	apiKey    string
	timestamp time.Time
	day       string
	month     string
	cost      float64
}

func clientAPIKeyQuotaBatchContext(items []coreusage.Item) context.Context {
	for _, item := range items {
		if item.Context != nil {
			return item.Context
		}
	}
	return context.Background()
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

// ClientAPIKeyQuotaState is the portable persisted state used to seed external
// quota backends after usage persistence has been restored.
type ClientAPIKeyQuotaState struct {
	Total   map[string]float64            `json:"total,omitempty"`
	Daily   map[string]map[string]float64 `json:"daily,omitempty"`
	Monthly map[string]map[string]float64 `json:"monthly,omitempty"`
}

func (state ClientAPIKeyQuotaState) isZero() bool {
	return len(state.Total) == 0 && len(state.Daily) == 0 && len(state.Monthly) == 0
}

// IsZero reports whether the state contains any quota counters.
func (state ClientAPIKeyQuotaState) IsZero() bool {
	return state.isZero()
}

// ClientAPIKeyQuotaStore provides optional shared quota counters. Redis uses
// this so clustered instances evaluate the same API key budget instead of only
// their local in-process counters.
type ClientAPIKeyQuotaStore interface {
	LoadClientAPIKeyQuotaUsage(ctx context.Context, apiKey string, now time.Time) (ClientAPIKeyQuotaUsage, bool, error)
	AddClientAPIKeyQuotaUsage(ctx context.Context, apiKey string, timestamp time.Time, cost float64) error
	SeedClientAPIKeyQuotaState(ctx context.Context, state ClientAPIKeyQuotaState) error
}

type clientAPIKeyQuotaTracker struct {
	mu          sync.RWMutex
	modelPrices config.ModelPrices
	priceCache  map[string]map[string]clientAPIKeyQuotaPriceCacheEntry
	total       map[string]clientAPIKeyQuotaCounters
	daily       map[string]map[string]clientAPIKeyQuotaCounters
	monthly     map[string]map[string]clientAPIKeyQuotaCounters
}

type clientAPIKeyQuotaPriceCacheEntry struct {
	price config.ModelPrice
	ok    bool
}

type persistedClientAPIKeyQuotaState = ClientAPIKeyQuotaState

var defaultClientAPIKeyQuotaTracker = newClientAPIKeyQuotaTracker()

var clientAPIKeyQuotaStore struct {
	mu    sync.RWMutex
	store ClientAPIKeyQuotaStore
}

const clientAPIKeyQuotaStoreTimeout = 2 * time.Second

func newClientAPIKeyQuotaTracker() *clientAPIKeyQuotaTracker {
	return &clientAPIKeyQuotaTracker{
		total:   make(map[string]clientAPIKeyQuotaCounters),
		daily:   make(map[string]map[string]clientAPIKeyQuotaCounters),
		monthly: make(map[string]map[string]clientAPIKeyQuotaCounters),
	}
}

// CheckClientAPIKeyQuota evaluates the configured quota for a client API key.
func CheckClientAPIKeyQuota(apiKey string, quota config.ClientAPIKeyQuota, now time.Time) *ClientAPIKeyQuotaExceeded {
	apiKey = strings.TrimSpace(apiKey)
	quota = config.NormalizeClientAPIKeyQuota(quota)
	if apiKey == "" || !normalizedClientAPIKeyQuotaHasLimits(quota) {
		return nil
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if store := clientAPIKeyQuotaStoreSnapshot(); store != nil {
		ctx, cancel := clientAPIKeyQuotaStoreContext(context.Background())
		usage, ok, err := store.LoadClientAPIKeyQuotaUsage(ctx, apiKey, now)
		cancel()
		if err == nil && ok {
			return evaluateNormalizedClientAPIKeyQuota(quota, usage, now)
		}
		if err != nil {
			log.WithError(err).Debug("client api key quota redis lookup failed")
		}
	}
	return defaultClientAPIKeyQuotaTracker.checkNormalized(apiKey, quota, now)
}

func normalizedClientAPIKeyQuotaHasLimits(quota config.ClientAPIKeyQuota) bool {
	return quota.DailyCost > 0 || quota.MonthlyCost > 0 || quota.TotalCost > 0
}

// SetClientAPIKeyQuotaStore swaps the optional shared quota counter backend.
func SetClientAPIKeyQuotaStore(store ClientAPIKeyQuotaStore) {
	clientAPIKeyQuotaStore.mu.Lock()
	clientAPIKeyQuotaStore.store = store
	clientAPIKeyQuotaStore.mu.Unlock()
	seedCurrentClientAPIKeyQuotaStore()
}

func clientAPIKeyQuotaStoreSnapshot() ClientAPIKeyQuotaStore {
	clientAPIKeyQuotaStore.mu.RLock()
	defer clientAPIKeyQuotaStore.mu.RUnlock()
	return clientAPIKeyQuotaStore.store
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
	t.priceCache = nil
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

func (t *clientAPIKeyQuotaTracker) record(record coreusage.Record) float64 {
	if t == nil {
		return 0
	}
	apiKey := strings.TrimSpace(record.APIKey)
	if apiKey == "" {
		return 0
	}

	timestamp := record.RequestedAt.UTC()
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	cost := t.costForRecordLocked(record)
	if cost <= 0 {
		return 0
	}

	t.addCountersLocked(t.total, apiKey, "", cost)
	t.addCountersLocked(t.daily, apiKey, timestamp.Format("2006-01-02"), cost)
	t.addCountersLocked(t.monthly, apiKey, timestamp.Format("2006-01"), cost)
	t.pruneLocked(timestamp)
	return cost
}

func (t *clientAPIKeyQuotaTracker) recordBatch(items []coreusage.Item, collectAdds bool) []clientAPIKeyQuotaRecordAdd {
	if t == nil || len(items) == 0 {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.modelPrices) == 0 {
		return nil
	}

	var adds []clientAPIKeyQuotaRecordAdd
	if collectAdds {
		adds = make([]clientAPIKeyQuotaRecordAdd, 0, len(items))
	}
	var pruneReference time.Time
	var lastDayYear int
	var lastDayYearDay int
	var lastDay string
	var lastMonthYear int
	var lastMonth time.Month
	var lastMonthValue string
	for _, item := range items {
		record := item.Record
		apiKey := strings.TrimSpace(record.APIKey)
		if apiKey == "" {
			continue
		}

		timestamp := record.RequestedAt.UTC()
		if timestamp.IsZero() {
			timestamp = time.Now().UTC()
		}

		cost := t.costForRecordLocked(record)
		if cost <= 0 {
			continue
		}

		year, monthValue, _ := timestamp.Date()
		yearDay := timestamp.YearDay()
		if lastDay == "" || year != lastDayYear || yearDay != lastDayYearDay {
			lastDayYear = year
			lastDayYearDay = yearDay
			lastDay = timestamp.Format("2006-01-02")
		}
		if lastMonthValue == "" || year != lastMonthYear || monthValue != lastMonth {
			lastMonthYear = year
			lastMonth = monthValue
			lastMonthValue = timestamp.Format("2006-01")
		}
		t.addCountersLocked(t.total, apiKey, "", cost)
		t.addCountersLocked(t.daily, apiKey, lastDay, cost)
		t.addCountersLocked(t.monthly, apiKey, lastMonthValue, cost)
		if pruneReference.IsZero() || timestamp.After(pruneReference) {
			pruneReference = timestamp
		}
		if collectAdds {
			adds = append(adds, clientAPIKeyQuotaRecordAdd{
				apiKey:    apiKey,
				timestamp: timestamp,
				day:       lastDay,
				month:     lastMonthValue,
				cost:      cost,
			})
		}
	}
	if !pruneReference.IsZero() {
		t.pruneLocked(pruneReference)
	}
	return adds
}

func (t *clientAPIKeyQuotaTracker) check(apiKey string, quota config.ClientAPIKeyQuota, now time.Time) *ClientAPIKeyQuotaExceeded {
	if t == nil {
		return nil
	}
	apiKey = strings.TrimSpace(apiKey)
	quota = config.NormalizeClientAPIKeyQuota(quota)
	if apiKey == "" || !normalizedClientAPIKeyQuotaHasLimits(quota) {
		return nil
	}
	return t.checkNormalized(apiKey, quota, now)
}

func (t *clientAPIKeyQuotaTracker) checkNormalized(apiKey string, quota config.ClientAPIKeyQuota, now time.Time) *ClientAPIKeyQuotaExceeded {
	if t == nil {
		return nil
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	usage := t.usage(apiKey, now)
	return evaluateNormalizedClientAPIKeyQuota(quota, usage, now)
}

func evaluateClientAPIKeyQuota(quota config.ClientAPIKeyQuota, usage ClientAPIKeyQuotaUsage, now time.Time) *ClientAPIKeyQuotaExceeded {
	quota = config.NormalizeClientAPIKeyQuota(quota)
	if !normalizedClientAPIKeyQuotaHasLimits(quota) {
		return nil
	}
	return evaluateNormalizedClientAPIKeyQuota(quota, usage, now)
}

func evaluateNormalizedClientAPIKeyQuota(quota config.ClientAPIKeyQuota, usage ClientAPIKeyQuotaUsage, now time.Time) *ClientAPIKeyQuotaExceeded {
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
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

func clientAPIKeyQuotaStoreContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(ctx, clientAPIKeyQuotaStoreTimeout)
}

func seedCurrentClientAPIKeyQuotaStore() {
	store := clientAPIKeyQuotaStoreSnapshot()
	if store == nil {
		return
	}
	state := defaultClientAPIKeyQuotaTracker.persistedState()
	if state.isZero() {
		return
	}
	ctx, cancel := clientAPIKeyQuotaStoreContext(context.Background())
	defer cancel()
	if err := store.SeedClientAPIKeyQuotaState(ctx, state); err != nil {
		log.WithError(err).Debug("client api key quota redis seed failed")
	}
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
	price, ok := t.lookupModelPriceLocked(record.Model, record.Alias)
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

func (t *clientAPIKeyQuotaTracker) lookupModelPriceLocked(model string, alias string) (config.ModelPrice, bool) {
	if t == nil {
		return config.ModelPrice{}, false
	}
	if t.priceCache != nil {
		if byAlias := t.priceCache[model]; byAlias != nil {
			if entry, ok := byAlias[alias]; ok {
				return entry.price, entry.ok
			}
		}
	}
	price, ok := config.LookupModelPrice(t.modelPrices, model, alias)
	if t.priceCache == nil {
		t.priceCache = make(map[string]map[string]clientAPIKeyQuotaPriceCacheEntry)
	}
	byAlias := t.priceCache[model]
	if byAlias == nil {
		byAlias = make(map[string]clientAPIKeyQuotaPriceCacheEntry)
		t.priceCache[model] = byAlias
	}
	byAlias[alias] = clientAPIKeyQuotaPriceCacheEntry{price: price, ok: ok}
	return price, ok
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
