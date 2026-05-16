package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestPersistedStateRestoresSummaryDetailsAndRecentAggregates(t *testing.T) {
	prevEnabled := StatisticsEnabled()
	SetStatisticsEnabled(true)
	t.Cleanup(func() {
		SetStatisticsEnabled(prevEnabled)
	})

	prevLimit := DetailRetentionLimit()
	SetDetailRetentionLimit(1)
	t.Cleanup(func() {
		SetDetailRetentionLimit(prevLimit)
	})

	// Anchor the test on the wall clock so retention pruning inside
	// LoadPersistedState (which uses time.Now()) doesn't drop records
	// once the hard-coded date drifts past the 7-day retention window.
	// Truncate to the second so SSE timestamp formatting in the snapshot
	// stays deterministic across save/restore.
	now := time.Now().UTC().Truncate(time.Second)
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "api-1",
		Model:       "gpt-5",
		Source:      "user-1@example.com",
		RequestedAt: now.Add(-30 * time.Minute),
		Latency:     250 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  2,
			OutputTokens: 3,
			TotalTokens:  5,
		},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "api-1",
		Model:       "gpt-5",
		Source:      "user-2@example.com",
		RequestedAt: now.Add(-10 * time.Minute),
		Latency:     400 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  3,
			OutputTokens: 4,
			TotalTokens:  7,
		},
	})

	path := filepath.Join(t.TempDir(), "usage.snapshot")
	if err := SavePersistedState(path, stats); err != nil {
		t.Fatalf("SavePersistedState error: %v", err)
	}

	restored := NewRequestStatistics()
	loaded, err := LoadPersistedState(path, restored)
	if err != nil {
		t.Fatalf("LoadPersistedState error: %v", err)
	}
	if !loaded {
		t.Fatalf("LoadPersistedState loaded = false, want true")
	}

	snapshot := restored.Snapshot()
	if snapshot.TotalRequests != 2 {
		t.Fatalf("total requests = %d, want 2", snapshot.TotalRequests)
	}
	model := snapshot.APIs["api-1"].Models["gpt-5"]
	if model.TotalTokens != 12 {
		t.Fatalf("total tokens = %d, want 12", model.TotalTokens)
	}
	if len(model.Details) != 1 {
		t.Fatalf("details len = %d, want 1", len(model.Details))
	}
	if got := model.Details[0].Source; got != "user-2@example.com" {
		t.Fatalf("retained detail source = %q, want %q", got, "user-2@example.com")
	}

	aggregated := restored.AggregatedUsageSnapshot(now)
	if got := aggregated.Windows["1h"].TotalRequests; got != 2 {
		t.Fatalf("1h total requests = %d, want 2", got)
	}
	if got := aggregated.Windows["all"].TotalRequests; got != 2 {
		t.Fatalf("all-window total requests = %d, want 2", got)
	}
}

func TestPersistedStateRestoresRolledUpAggregateHistory(t *testing.T) {
	prevEnabled := StatisticsEnabled()
	SetStatisticsEnabled(true)
	t.Cleanup(func() {
		SetStatisticsEnabled(prevEnabled)
	})

	prevLimit := DetailRetentionLimit()
	SetDetailRetentionLimit(0)
	t.Cleanup(func() {
		SetDetailRetentionLimit(prevLimit)
	})

	// Anchor on the wall clock so retention pruning inside LoadPersistedState
	// (which uses time.Now()) doesn't drop records as the hard-coded date
	// drifts past the 7-day retention window.
	now := time.Now().UTC().Truncate(time.Second)
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "api-2",
		Model:       "gpt-5",
		Source:      "older@example.com",
		RequestedAt: now.Add(-8 * 24 * time.Hour),
		Detail: coreusage.Detail{
			InputTokens:  4,
			OutputTokens: 1,
			TotalTokens:  5,
		},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "api-2",
		Model:       "gpt-5",
		Source:      "recent@example.com",
		RequestedAt: now.Add(-2 * time.Hour),
		Detail: coreusage.Detail{
			InputTokens:  5,
			OutputTokens: 2,
			TotalTokens:  7,
		},
	})

	path := filepath.Join(t.TempDir(), "usage.snapshot")
	if err := SavePersistedState(path, stats); err != nil {
		t.Fatalf("SavePersistedState error: %v", err)
	}

	restored := NewRequestStatistics()
	if _, err := LoadPersistedState(path, restored); err != nil {
		t.Fatalf("LoadPersistedState error: %v", err)
	}

	aggregated := restored.AggregatedUsageSnapshot(now)
	if got := aggregated.Windows["7d"].TotalRequests; got != 1 {
		t.Fatalf("7d total requests = %d, want 1", got)
	}
	if got := aggregated.Windows["all"].TotalRequests; got != 2 {
		t.Fatalf("all-window total requests = %d, want 2", got)
	}
	if got := aggregated.Windows["all"].TotalTokens; got != 12 {
		t.Fatalf("all-window total tokens = %d, want 12", got)
	}
}

func TestPersistedStateRestoresClientAPIKeyQuotaCounters(t *testing.T) {
	previousTracker := defaultClientAPIKeyQuotaTracker
	defaultClientAPIKeyQuotaTracker = newClientAPIKeyQuotaTracker()
	t.Cleanup(func() {
		defaultClientAPIKeyQuotaTracker = previousTracker
	})

	defaultClientAPIKeyQuotaTracker.setModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1},
	})
	now := time.Now().UTC()
	defaultClientAPIKeyQuotaTracker.record(coreusage.Record{
		APIKey:      "persisted-quota-key",
		RequestedAt: now,
		Model:       "gpt-test",
		Detail:      coreusage.Detail{InputTokens: 1_000_000},
	})

	data, err := MarshalPersistedState(NewRequestStatistics())
	if err != nil {
		t.Fatalf("MarshalPersistedState error: %v", err)
	}

	defaultClientAPIKeyQuotaTracker = newClientAPIKeyQuotaTracker()
	loaded, err := LoadPersistedStateBytes(data, NewRequestStatistics())
	if err != nil {
		t.Fatalf("LoadPersistedStateBytes error: %v", err)
	}
	if !loaded {
		t.Fatal("LoadPersistedStateBytes loaded = false, want true")
	}

	usage := defaultClientAPIKeyQuotaTracker.usage("persisted-quota-key", now)
	if usage.DailyCost != 1 || usage.MonthlyCost != 1 || usage.TotalCost != 1 {
		t.Fatalf("restored quota usage = %+v, want all costs 1", usage)
	}
}
