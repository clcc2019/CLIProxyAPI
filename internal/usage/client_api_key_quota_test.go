package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

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

func TestClientAPIKeyQuotaTrackerUsesDefaultKiroClaudePriceAliases(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	tracker.setModelPrices(nil)
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now,
		Model:       "kiro-claude-sonnet-4-6-agentic",
		Detail: coreusage.Detail{
			InputTokens:  1_000_000,
			OutputTokens: 1_000_000,
		},
	})

	exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{DailyCost: 18}, now)
	if exceeded == nil {
		t.Fatal("expected default Kiro Claude price to count toward quota")
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
