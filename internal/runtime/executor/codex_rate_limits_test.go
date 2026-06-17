package executor

import (
	"net/http"
	"testing"
)

func TestCodexRateLimitSnapshotsFromHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "12.5")
	headers.Set("X-Codex-Primary-Window-Minutes", "60")
	headers.Set("X-Codex-Primary-Reset-At", "1704069000")
	headers.Set("X-Codex-Credits-Has-Credits", "true")
	headers.Set("X-Codex-Credits-Unlimited", "false")
	headers.Set("X-Codex-Credits-Balance", "3")
	headers.Set("X-Codex-Other-Primary-Used-Percent", "80")
	headers.Set("X-Codex-Other-Limit-Name", "Other bucket")

	snapshots := codexRateLimitSnapshotsFromHeaders(headers)
	if len(snapshots) != 2 {
		t.Fatalf("snapshots len = %d, want 2: %#v", len(snapshots), snapshots)
	}
	if got := snapshots[0].LimitID; got != "codex" {
		t.Fatalf("default limit id = %q, want codex", got)
	}
	if snapshots[0].Primary == nil || snapshots[0].Primary.UsedPercent != 12.5 {
		t.Fatalf("default primary = %#v, want used percent 12.5", snapshots[0].Primary)
	}
	if snapshots[0].Primary.WindowMinutes == nil || *snapshots[0].Primary.WindowMinutes != 60 {
		t.Fatalf("default window minutes = %#v, want 60", snapshots[0].Primary.WindowMinutes)
	}
	if snapshots[0].Credits == nil || !snapshots[0].Credits.HasCredits || snapshots[0].Credits.Unlimited {
		t.Fatalf("credits = %#v, want finite credits", snapshots[0].Credits)
	}
	if got := snapshots[1].LimitID; got != "codex_other" {
		t.Fatalf("additional limit id = %q, want codex_other", got)
	}
	if got := snapshots[1].LimitName; got != "Other bucket" {
		t.Fatalf("additional limit name = %q, want Other bucket", got)
	}
}

func TestCodexRateLimitSnapshotFromEvent(t *testing.T) {
	payload := []byte(`{
		"type":"codex.rate_limits",
		"plan_type":"plus",
		"metered_limit_name":"codex-other",
		"rate_limits":{
			"primary":{"used_percent":90.5,"window_minutes":300,"reset_at":1704070000},
			"secondary":{"used_percent":30}
		},
		"credits":{"has_credits":true,"unlimited":false,"balance":"5"}
	}`)

	snapshot, ok := codexRateLimitSnapshotFromEvent(payload)
	if !ok {
		t.Fatal("codexRateLimitSnapshotFromEvent() ok = false")
	}
	if got := snapshot.LimitID; got != "codex_other" {
		t.Fatalf("LimitID = %q, want codex_other", got)
	}
	if got := snapshot.PlanType; got != "plus" {
		t.Fatalf("PlanType = %q, want plus", got)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 90.5 {
		t.Fatalf("Primary = %#v, want used percent 90.5", snapshot.Primary)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.UsedPercent != 30 {
		t.Fatalf("Secondary = %#v, want used percent 30", snapshot.Secondary)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance != "5" {
		t.Fatalf("Credits = %#v, want balance 5", snapshot.Credits)
	}
}

func TestCodexRateLimitSnapshotsFromErrorBody(t *testing.T) {
	body := []byte(`{"error":{"message":"usage limit","rate_limits":{"limitId":"codex","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1704070000},"rateLimitReachedType":"rate_limit_reached"}}}`)

	snapshots := codexRateLimitSnapshotsFromErrorBody(body)
	if len(snapshots) != 1 {
		t.Fatalf("snapshots len = %d, want 1: %#v", len(snapshots), snapshots)
	}
	snapshot := snapshots[0]
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 100 {
		t.Fatalf("Primary = %#v, want used percent 100", snapshot.Primary)
	}
	if got := snapshot.RateLimitReachedType; got != "rate_limit_reached" {
		t.Fatalf("RateLimitReachedType = %q, want rate_limit_reached", got)
	}
}
