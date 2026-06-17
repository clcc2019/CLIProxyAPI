package auth

import (
	"context"
	"testing"
	"time"
)

func TestManagerUpdateRateLimitsMergesSnapshots(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	resetAt := int64(1704070000)

	manager.UpdateRateLimits(context.Background(), "codex-auth", []RateLimitSnapshot{{
		LimitID: "codex-other",
		Primary: &RateLimitWindow{
			UsedPercent: 75,
			ResetsAt:    &resetAt,
		},
		PlanType: "plus",
	}})

	auths := manager.List()
	if len(auths) != 1 {
		t.Fatalf("List() len = %d, want 1", len(auths))
	}
	snapshot, ok := auths[0].RateLimits["codex_other"]
	if !ok {
		t.Fatalf("rate limit snapshot missing: %#v", auths[0].RateLimits)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 75 {
		t.Fatalf("Primary = %#v, want used percent 75", snapshot.Primary)
	}
	if snapshot.Primary.ResetsAt == nil || *snapshot.Primary.ResetsAt != resetAt {
		t.Fatalf("ResetsAt = %#v, want %d", snapshot.Primary.ResetsAt, resetAt)
	}
	if got := snapshot.PlanType; got != "plus" {
		t.Fatalf("PlanType = %q, want plus", got)
	}
	if snapshot.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt was not set")
	}
}

func TestMergeRateLimitSnapshotsIgnoresTimestampOnlyChanges(t *testing.T) {
	auth := &Auth{ID: "codex-auth"}
	first := RateLimitSnapshot{
		LimitID: "codex",
		Primary: &RateLimitWindow{
			UsedPercent: 20,
		},
		UpdatedAt: time.Unix(100, 0),
	}
	if !mergeRateLimitSnapshots(auth, []RateLimitSnapshot{first}, time.Unix(100, 0)) {
		t.Fatal("first merge changed = false, want true")
	}
	second := first
	second.UpdatedAt = time.Unix(200, 0)
	if mergeRateLimitSnapshots(auth, []RateLimitSnapshot{second}, time.Unix(200, 0)) {
		t.Fatal("timestamp-only merge changed = true, want false")
	}
}
