package usage

import (
	"context"
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
