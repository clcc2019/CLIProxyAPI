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

func TestManagerClearAuthQuotaCooldownClearsAuthAndModelQuota(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &Auth{
		ID:             "codex-auth",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(time.Hour),
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: time.Now().Add(time.Hour),
			AuthScope:     true,
		},
		LastError: &Error{Code: "rate_limited"},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(time.Hour),
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: time.Now().Add(time.Hour)},
				LastError:      &Error{Code: "rate_limited"},
			},
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if !manager.ClearAuthQuotaCooldown(context.Background(), "codex-auth") {
		t.Fatal("ClearAuthQuotaCooldown() = false, want true")
	}
	updated, ok := manager.GetByID("codex-auth")
	if !ok || updated == nil {
		t.Fatal("auth not found after clear")
	}
	if updated.Unavailable || updated.Status != StatusActive || updated.Quota.Exceeded || !updated.NextRetryAfter.IsZero() || updated.LastError != nil {
		t.Fatalf("auth quota cooldown not cleared: %#v", updated)
	}
	state := updated.ModelStates["gpt-5-codex"]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Unavailable || state.Status != StatusActive || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		t.Fatalf("model quota cooldown not cleared: %#v", state)
	}
}

func TestManagerClearAuthQuotaCooldownKeepsDisabledAuthDisabled(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &Auth{
		ID:            "codex-disabled",
		Provider:      "codex",
		Status:        StatusDisabled,
		StatusMessage: "manually disabled",
		Unavailable:   true,
		Quota:         QuotaState{Exceeded: true, Reason: "quota", AuthScope: true},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if manager.ClearAuthQuotaCooldown(context.Background(), "codex-disabled") {
		t.Fatal("ClearAuthQuotaCooldown() = true, want false for disabled auth")
	}
	updated, ok := manager.GetByID("codex-disabled")
	if !ok || updated == nil {
		t.Fatal("auth not found after clear")
	}
	if updated.Status != StatusDisabled || !updated.Quota.Exceeded || !updated.Unavailable {
		t.Fatalf("disabled auth should remain unchanged: %#v", updated)
	}
}
