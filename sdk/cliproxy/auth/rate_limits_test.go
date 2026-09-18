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

func TestManagerUpdateRateLimitsPreservesPartialSnapshots(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	primaryReset := int64(1704070000)
	secondaryReset := int64(1704073600)
	manager.UpdateRateLimits(context.Background(), "codex-auth", []RateLimitSnapshot{{
		LimitID:   "codex",
		Primary:   &RateLimitWindow{UsedPercent: 25, WindowMinutes: int64Pointer(300), ResetsAt: &primaryReset},
		Secondary: &RateLimitWindow{UsedPercent: 40, WindowMinutes: int64Pointer(10080), ResetsAt: &secondaryReset},
		Credits:   &CreditsSnapshot{HasCredits: true, Balance: "12"},
	}})

	// A later usage response may only contain the primary window. Existing
	// secondary/credits data must remain available to management readers.
	manager.UpdateRateLimits(context.Background(), "codex-auth", []RateLimitSnapshot{{
		LimitID: "codex",
		Primary: &RateLimitWindow{UsedPercent: 50},
	}})

	auth, ok := manager.GetByID("codex-auth")
	if !ok || auth == nil {
		t.Fatal("updated auth missing")
	}
	snapshot := auth.RateLimits["codex"]
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 50 {
		t.Fatalf("primary snapshot = %#v, want updated usage", snapshot.Primary)
	}
	if snapshot.Primary.ResetsAt == nil || *snapshot.Primary.ResetsAt != primaryReset {
		t.Fatalf("primary reset = %#v, want %d", snapshot.Primary.ResetsAt, primaryReset)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.UsedPercent != 40 {
		t.Fatalf("secondary snapshot = %#v, want preserved usage", snapshot.Secondary)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance != "12" {
		t.Fatalf("credits snapshot = %#v, want preserved credits", snapshot.Credits)
	}
}

func TestManagerUpdatePreservesRateLimitsAcrossAuthReplacement(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &Auth{
		ID:         "codex-auth",
		Provider:   "codex",
		Status:     StatusActive,
		Metadata:   map[string]any{"type": "codex"},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	manager.UpdateRateLimits(context.Background(), "codex-auth", []RateLimitSnapshot{{
		LimitID: "codex",
		Primary: &RateLimitWindow{UsedPercent: 37},
		Credits: &CreditsSnapshot{HasCredits: true, Balance: "8"},
	}})

	// File watcher/OAuth refresh updates commonly contain only credential
	// metadata. They must not erase the in-memory quota snapshot.
	if _, err := manager.Update(context.Background(), &Auth{
		ID:         "codex-auth",
		Provider:   "codex",
		Status:     StatusActive,
		Metadata:   map[string]any{"type": "codex", "access_token": "new-token"},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	auth, ok := manager.GetByID("codex-auth")
	if !ok || auth == nil || auth.RateLimits["codex"].Primary == nil {
		t.Fatalf("rate limits lost after auth replacement: %#v", auth)
	}
	if got := auth.RateLimits["codex"].Primary.UsedPercent; got != 37 {
		t.Fatalf("primary usage after replacement = %v, want 37", got)
	}
}

func TestManagerUpdateRateLimitsPersistsAndReloadsRuntimeState(t *testing.T) {
	store := &fakeRuntimeStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetRuntimeStateStore(store)
	t.Cleanup(manager.stopPersistLoop)
	if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	manager.UpdateRateLimits(context.Background(), "codex-auth", []RateLimitSnapshot{{
		LimitID: "codex",
		Primary: &RateLimitWindow{UsedPercent: 61},
		Credits: &CreditsSnapshot{HasCredits: true, Balance: "3"},
	}})
	manager.flushPersistQueue()

	persisted, ok := store.saved["codex-auth"]
	if !ok || persisted.RateLimits["codex"].Primary == nil {
		t.Fatalf("runtime store missing rate limits: %#v", store.saved)
	}
	if got := persisted.RateLimits["codex"].Primary.UsedPercent; got != 61 {
		t.Fatalf("persisted primary usage = %v, want 61", got)
	}

	store.states = store.saved
	reloaded := NewManager(nil, nil, nil)
	reloaded.SetRuntimeStateStore(store)
	if err := reloaded.LoadRuntimeStates(context.Background()); err != nil {
		t.Fatalf("LoadRuntimeStates() error = %v", err)
	}
	t.Cleanup(reloaded.stopPersistLoop)
	if _, err := reloaded.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("reloaded Register() error = %v", err)
	}
	auth, ok := reloaded.GetByID("codex-auth")
	if !ok || auth == nil || auth.RateLimits["codex"].Primary == nil {
		t.Fatalf("reloaded auth missing rate limits: %#v", auth)
	}
	if got := auth.RateLimits["codex"].Credits.Balance; got != "3" {
		t.Fatalf("reloaded credits balance = %q, want 3", got)
	}
}

func int64Pointer(value int64) *int64 { return &value }

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
