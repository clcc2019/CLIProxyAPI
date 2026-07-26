package management

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestCodexUsageQuotaRecoverAtIgnoresAdditionalRateLimits(t *testing.T) {
	now := time.Unix(1780000000, 0)
	payload := gin.H{
		"plan_type": "plus",
		"rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": 40.0, "reset_at": float64(now.Add(2 * time.Hour).Unix())},
			"secondary_window": map[string]any{"used_percent": 61.0, "reset_at": float64(now.Add(72 * time.Hour).Unix())},
		},
		"additional_rate_limits": []any{
			map[string]any{
				"limit_name": "code-review",
				"rate_limit": map[string]any{
					"primary_window": map[string]any{"used_percent": 100.0, "reset_at": float64(now.Add(time.Hour).Unix())},
				},
			},
		},
	}

	if recoverAt, exhausted := codexUsageQuotaRecoverAt(payload, now); exhausted {
		t.Fatalf("exhausted = true (recoverAt %s), want false: a metered sub-feature must not block the credential", recoverAt)
	}
}

func TestCodexUsageQuotaRecoverAtSkipsPrimaryWindowOnFreePlan(t *testing.T) {
	now := time.Unix(1780000000, 0)
	payload := gin.H{
		"plan_type": "free",
		"rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": 100.0, "reset_at": float64(now.Add(time.Hour).Unix())},
			"secondary_window": map[string]any{"used_percent": 30.0, "reset_at": float64(now.Add(96 * time.Hour).Unix())},
		},
	}

	if recoverAt, exhausted := codexUsageQuotaRecoverAt(payload, now); exhausted {
		t.Fatalf("exhausted = true (recoverAt %s), want false: free plans only meter the weekly window", recoverAt)
	}
}

func TestCodexUsageQuotaRecoverAtUsesResetAfterSeconds(t *testing.T) {
	now := time.Unix(1780000000, 0)
	payload := gin.H{
		"plan_type": "plus",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{"used_percent": 100.0, "reset_after_seconds": 900.0},
		},
	}

	recoverAt, exhausted := codexUsageQuotaRecoverAt(payload, now)
	if !exhausted {
		t.Fatal("exhausted = false, want true")
	}
	if want := now.Add(15 * time.Minute); !recoverAt.Equal(want) {
		t.Fatalf("recoverAt = %s, want %s: reset_after_seconds must avoid the 5h fallback", recoverAt, want)
	}
}

func TestCodexUsageQuotaRecoverAtStillMarksExhaustedPrimaryWindow(t *testing.T) {
	now := time.Unix(1780000000, 0)
	want := now.Add(3 * time.Hour)
	payload := gin.H{
		"plan_type": "plus",
		"rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": 100.0, "reset_at": float64(want.Unix())},
			"secondary_window": map[string]any{"used_percent": 55.0, "reset_at": float64(now.Add(96 * time.Hour).Unix())},
		},
	}

	recoverAt, exhausted := codexUsageQuotaRecoverAt(payload, now)
	if !exhausted {
		t.Fatal("exhausted = false, want true")
	}
	if !recoverAt.Equal(want) {
		t.Fatalf("recoverAt = %s, want %s", recoverAt, want)
	}
}
