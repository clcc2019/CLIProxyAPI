package usage

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type fakeClientAPIKeyQuotaStore struct {
	usage  ClientAPIKeyQuotaUsage
	found  bool
	adds   []fakeClientAPIKeyQuotaAdd
	seeded ClientAPIKeyQuotaState
}

type fakeClientAPIKeyQuotaAdd struct {
	apiKey    string
	timestamp time.Time
	cost      float64
}

type fakeClientAPIKeyQuotaCounterStore struct {
	*fakeClientAPIKeyQuotaStore
	counterAdds []ClientAPIKeyQuotaUsage
}

func (s *fakeClientAPIKeyQuotaCounterStore) AddClientAPIKeyQuotaUsageCounters(_ context.Context, _ string, _ time.Time, usage ClientAPIKeyQuotaUsage) error {
	s.counterAdds = append(s.counterAdds, usage)
	return nil
}

func (s *fakeClientAPIKeyQuotaStore) LoadClientAPIKeyQuotaUsage(context.Context, string, time.Time) (ClientAPIKeyQuotaUsage, bool, error) {
	return s.usage, s.found, nil
}

func (s *fakeClientAPIKeyQuotaStore) AddClientAPIKeyQuotaUsage(_ context.Context, apiKey string, timestamp time.Time, cost float64) error {
	s.adds = append(s.adds, fakeClientAPIKeyQuotaAdd{apiKey: apiKey, timestamp: timestamp, cost: cost})
	return nil
}

func (s *fakeClientAPIKeyQuotaStore) SeedClientAPIKeyQuotaState(_ context.Context, state ClientAPIKeyQuotaState) error {
	s.seeded = state
	return nil
}

func resetClientAPIKeyQuotaGlobals(t *testing.T) {
	t.Helper()
	previousTracker := defaultClientAPIKeyQuotaTracker
	defaultClientAPIKeyQuotaTracker = newClientAPIKeyQuotaTracker()
	SetClientAPIKeyQuotaStore(nil)
	t.Cleanup(func() {
		SetClientAPIKeyQuotaStore(nil)
		defaultClientAPIKeyQuotaTracker = previousTracker
	})
}

func TestClientAPIKeyQuotaTrackerChecksCompletedUsage(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	tracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1, Completion: 2, Cache: 0.5},
	})
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now.Add(-time.Hour),
		Model:       "gpt-test",
		Detail: coreusage.Detail{
			InputTokens:  1_000_000,
			OutputTokens: 500_000,
		},
	})

	exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{DailyCost: 2}, now)
	if exceeded == nil {
		t.Fatal("expected daily cost quota to be exceeded")
	}
	if exceeded.Scope != "daily" || exceeded.Resource != "cost" || exceeded.Limit != 2 || exceeded.Used != 2 {
		t.Fatalf("unexpected exceeded quota: %#v", exceeded)
	}
	if want := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC); !exceeded.ResetAt.Equal(want) {
		t.Fatalf("reset_at = %s, want %s", exceeded.ResetAt, want)
	}
}

func TestClientAPIKeyQuotaTrackerUsesUTCWindows(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	tracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1},
	})
	now := time.Date(2026, 5, 7, 0, 30, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now.Add(-time.Hour),
		Model:       "gpt-test",
		Detail:      coreusage.Detail{InputTokens: 1_000_000},
	})

	if exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{DailyCost: 1}, now); exceeded != nil {
		t.Fatalf("previous UTC day should not count toward current daily quota: %#v", exceeded)
	}
	if exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{MonthlyCost: 1}, now); exceeded == nil {
		t.Fatal("same UTC month should count toward monthly quota")
	}
}

func TestClientAPIKeyQuotaTrackerChecksRequestAndTokenUsage(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "unknown-model",
		Detail:      coreusage.Detail{TotalTokens: 75},
	})

	if exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{DailyRequests: 1}, now); exceeded == nil || exceeded.Resource != "requests" || exceeded.Used != 1 {
		t.Fatalf("daily request quota = %#v, want one request exceeded", exceeded)
	}
	if exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{DailyTokens: 75}, now); exceeded == nil || exceeded.Resource != "tokens" || exceeded.Used != 75 {
		t.Fatalf("daily token quota = %#v, want 75 tokens exceeded", exceeded)
	}
}

func TestClientAPIKeyQuotaDoesNotDoubleCountAdditionalModelUsage(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Detail:      coreusage.Detail{TotalTokens: 75},
	})
	tracker.record(coreusage.Record{
		APIKey:               "client-key",
		RequestedAt:          now,
		AdditionalModelUsage: true,
		Detail:               coreusage.Detail{TotalTokens: 25},
	})

	usage := tracker.usage("client-key", now)
	if usage.DailyRequests != 1 || usage.TotalRequests != 1 {
		t.Fatalf("request usage = %#v, want one request", usage)
	}
	if usage.DailyTokens != 100 || usage.TotalTokens != 100 {
		t.Fatalf("token usage = %#v, want 100 tokens", usage)
	}
}

func TestClientAPIKeyQuotaCountersSaturateAtMaxInt64(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Detail:      coreusage.Detail{TotalTokens: math.MaxInt64},
	})
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Detail:      coreusage.Detail{TotalTokens: 1},
	})

	usage := tracker.usage("client-key", now)
	if usage.TotalTokens != math.MaxInt64 {
		t.Fatalf("total tokens = %d, want saturation at %d", usage.TotalTokens, math.MaxInt64)
	}
}

func TestClientAPIKeyQuotaPluginWritesRequestAndTokenCountersToExtendedStore(t *testing.T) {
	resetClientAPIKeyQuotaGlobals(t)
	store := &fakeClientAPIKeyQuotaCounterStore{fakeClientAPIKeyQuotaStore: &fakeClientAPIKeyQuotaStore{}}
	SetClientAPIKeyQuotaStore(store)

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clientAPIKeyQuotaPlugin{}.HandleUsage(context.Background(), coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "unknown-model",
		Detail:      coreusage.Detail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})

	if len(store.counterAdds) != 1 {
		t.Fatalf("counter store adds = %d, want 1", len(store.counterAdds))
	}
	got := store.counterAdds[0]
	if got.DailyRequests != 1 || got.MonthlyRequests != 1 || got.TotalRequests != 1 {
		t.Fatalf("request counters = %#v, want all 1", got)
	}
	if got.DailyTokens != 15 || got.MonthlyTokens != 15 || got.TotalTokens != 15 {
		t.Fatalf("token counters = %#v, want all 15", got)
	}
}

func TestClientAPIKeyQuotaTrackerUsesDefaultClaudePriceAliases(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	tracker.setModelPrices(nil)
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "claude-sonnet-4-6-agentic",
		Detail: coreusage.Detail{
			InputTokens:  1_000_000,
			OutputTokens: 1_000_000,
		},
	})

	exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{DailyCost: 18}, now)
	if exceeded == nil {
		t.Fatal("expected default Claude price to count toward quota")
	}
	if exceeded.Used != 18 {
		t.Fatalf("used cost = %v, want 18", exceeded.Used)
	}
}

func TestClientAPIKeyQuotaTrackerChargesCacheCreationAsPromptInput(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	tracker.setModelPrices(config.ModelPrices{
		"claude-test": {Prompt: 2, Cache: 0.5},
	})
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "claude-test",
		Detail: coreusage.Detail{
			InputTokens:         1_500_000,
			CachedTokens:        500_000,
			CacheCreationTokens: 250_000,
		},
	})

	usage := tracker.usage("client-key", now)
	if usage.DailyCost != 2.25 {
		t.Fatalf("daily cost = %v, want 2.25", usage.DailyCost)
	}
}

func TestClientAPIKeyQuotaTrackerClearsPriceCacheOnModelPriceUpdate(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	record := coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "gpt-test",
		Detail:      coreusage.Detail{InputTokens: 1_000_000},
	}

	tracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1},
	})
	tracker.record(record)

	tracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 2},
	})
	tracker.record(record)

	usage := tracker.usage("client-key", now)
	if usage.TotalCost != 3 || usage.DailyCost != 3 || usage.MonthlyCost != 3 {
		t.Fatalf("usage after price update = %#v, want all costs=3", usage)
	}
}

func TestClientAPIKeyQuotaCheckUsesSharedStoreWhenAvailable(t *testing.T) {
	resetClientAPIKeyQuotaGlobals(t)
	defaultClientAPIKeyQuotaTracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1},
	})
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	defaultClientAPIKeyQuotaTracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "gpt-test",
		Detail:      coreusage.Detail{InputTokens: 5_000_000},
	})

	store := &fakeClientAPIKeyQuotaStore{
		usage: ClientAPIKeyQuotaUsage{DailyCost: 0.5},
		found: true,
	}
	SetClientAPIKeyQuotaStore(store)

	if exceeded := CheckClientAPIKeyQuota("client-key", config.ClientAPIKeyQuota{DailyCost: 1}, now); exceeded != nil {
		t.Fatalf("quota check used local counters instead of shared store: %#v", exceeded)
	}

	store.usage.DailyCost = 2
	exceeded := CheckClientAPIKeyQuota("client-key", config.ClientAPIKeyQuota{DailyCost: 1}, now)
	if exceeded == nil || exceeded.Scope != "daily" || exceeded.Used != 2 {
		t.Fatalf("shared quota check = %#v, want daily cost exceeded from store", exceeded)
	}
}

func TestClientAPIKeyQuotaPluginWritesSharedStore(t *testing.T) {
	resetClientAPIKeyQuotaGlobals(t)
	defaultClientAPIKeyQuotaTracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1, Completion: 2},
	})
	store := &fakeClientAPIKeyQuotaStore{}
	SetClientAPIKeyQuotaStore(store)

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clientAPIKeyQuotaPlugin{}.HandleUsage(context.Background(), coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "gpt-test",
		Detail: coreusage.Detail{
			InputTokens:  1_000_000,
			OutputTokens: 500_000,
		},
	})

	if len(store.adds) != 1 {
		t.Fatalf("shared store adds = %d, want 1", len(store.adds))
	}
	add := store.adds[0]
	if add.apiKey != "client-key" || !add.timestamp.Equal(now) || add.cost != 2 {
		t.Fatalf("shared store add = %#v, want api key, timestamp and cost=2", add)
	}
}

func TestClientAPIKeyQuotaPluginBatchesSharedStoreAdds(t *testing.T) {
	resetClientAPIKeyQuotaGlobals(t)
	defaultClientAPIKeyQuotaTracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1, Completion: 2},
	})
	store := &fakeClientAPIKeyQuotaStore{}
	SetClientAPIKeyQuotaStore(store)

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clientAPIKeyQuotaPlugin{}.HandleUsageBatch([]coreusage.Item{
		{
			Context: context.Background(),
			Record: coreusage.Record{
				APIKey:      "client-key",
				RequestedAt: now,
				Model:       "gpt-test",
				Detail:      coreusage.Detail{InputTokens: 1_000_000},
			},
		},
		{
			Context: context.Background(),
			Record: coreusage.Record{
				APIKey:      "client-key",
				RequestedAt: now.Add(time.Minute),
				Model:       "gpt-test",
				Detail:      coreusage.Detail{OutputTokens: 500_000},
			},
		},
	})

	if len(store.adds) != 1 {
		t.Fatalf("shared store adds = %d, want 1", len(store.adds))
	}
	add := store.adds[0]
	if add.apiKey != "client-key" || !add.timestamp.Equal(now) || add.cost != 2 {
		t.Fatalf("shared store add = %#v, want earliest timestamp and cost=2", add)
	}
	usage := defaultClientAPIKeyQuotaTracker.usage("client-key", now)
	if usage.DailyCost != 2 || usage.MonthlyCost != 2 || usage.TotalCost != 2 {
		t.Fatalf("local usage = %#v, want all costs=2", usage)
	}
}

func BenchmarkClientAPIKeyQuotaRecordBatch(b *testing.B) {
	tracker := newClientAPIKeyQuotaTracker()
	tracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1, Completion: 2, Cache: 0.5},
	})
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	items := make([]coreusage.Item, 64)
	for i := range items {
		items[i] = coreusage.Item{
			Context: context.Background(),
			Record: coreusage.Record{
				APIKey:      "client-key",
				RequestedAt: now.Add(time.Duration(i) * time.Second),
				Model:       "gpt-test",
				Detail: coreusage.Detail{
					InputTokens:  1_000,
					CachedTokens: 250,
					OutputTokens: 500,
				},
			},
		}
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tracker.recordBatch(items, true)
	}
}

func BenchmarkClientAPIKeyQuotaRecordBatchLocalOnly(b *testing.B) {
	tracker := newClientAPIKeyQuotaTracker()
	tracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1, Completion: 2, Cache: 0.5},
	})
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	items := make([]coreusage.Item, 64)
	for i := range items {
		items[i] = coreusage.Item{
			Context: context.Background(),
			Record: coreusage.Record{
				APIKey:      "client-key",
				RequestedAt: now.Add(time.Duration(i) * time.Second),
				Model:       "gpt-test",
				Detail: coreusage.Detail{
					InputTokens:  1_000,
					CachedTokens: 250,
					OutputTokens: 500,
				},
			},
		}
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tracker.recordBatch(items, false)
	}
}

func BenchmarkClientAPIKeyQuotaRecordIndividually(b *testing.B) {
	tracker := newClientAPIKeyQuotaTracker()
	tracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1, Completion: 2, Cache: 0.5},
	})
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	records := make([]coreusage.Record, 64)
	for i := range records {
		records[i] = coreusage.Record{
			APIKey:      "client-key",
			RequestedAt: now.Add(time.Duration(i) * time.Second),
			Model:       "gpt-test",
			Detail: coreusage.Detail{
				InputTokens:  1_000,
				CachedTokens: 250,
				OutputTokens: 500,
			},
		}
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for j := range records {
			tracker.record(records[j])
		}
	}
}

func TestSetClientAPIKeyQuotaStoreSeedsCurrentState(t *testing.T) {
	resetClientAPIKeyQuotaGlobals(t)
	defaultClientAPIKeyQuotaTracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1},
	})
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	defaultClientAPIKeyQuotaTracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "gpt-test",
		Detail:      coreusage.Detail{InputTokens: 1_000_000},
	})

	store := &fakeClientAPIKeyQuotaStore{}
	SetClientAPIKeyQuotaStore(store)

	if store.seeded.Total["client-key"] != 1 {
		t.Fatalf("seeded total = %#v, want client-key cost 1", store.seeded.Total)
	}
	if store.seeded.Daily["client-key"]["2026-05-07"] != 1 {
		t.Fatalf("seeded daily = %#v, want 2026-05-07 cost 1", store.seeded.Daily)
	}
	if store.seeded.Monthly["client-key"]["2026-05"] != 1 {
		t.Fatalf("seeded monthly = %#v, want 2026-05 cost 1", store.seeded.Monthly)
	}
}
