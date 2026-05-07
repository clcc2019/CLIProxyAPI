package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestClientAPIKeyQuotaTrackerChecksCompletedUsage(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now.Add(-time.Hour),
		Detail: coreusage.Detail{
			InputTokens:  40,
			OutputTokens: 60,
		},
	})

	exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{DailyTokens: 100}, now)
	if exceeded == nil {
		t.Fatal("expected daily token quota to be exceeded")
	}
	if exceeded.Scope != "daily" || exceeded.Resource != "tokens" || exceeded.Limit != 100 || exceeded.Used != 100 {
		t.Fatalf("unexpected exceeded quota: %#v", exceeded)
	}
	if want := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC); !exceeded.ResetAt.Equal(want) {
		t.Fatalf("reset_at = %s, want %s", exceeded.ResetAt, want)
	}
}

func TestClientAPIKeyQuotaTrackerUsesUTCWindows(t *testing.T) {
	tracker := newClientAPIKeyQuotaTracker()
	now := time.Date(2026, 5, 7, 0, 30, 0, 0, time.UTC)
	tracker.record(coreusage.Record{
		APIKey:      "client-key",
		RequestedAt: now.Add(-time.Hour),
		Detail:      coreusage.Detail{TotalTokens: 1000},
	})

	if exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{DailyRequests: 1}, now); exceeded != nil {
		t.Fatalf("previous UTC day should not count toward current daily quota: %#v", exceeded)
	}
	if exceeded := tracker.check("client-key", config.ClientAPIKeyQuota{MonthlyRequests: 1}, now); exceeded == nil {
		t.Fatal("same UTC month should count toward monthly quota")
	}
}
