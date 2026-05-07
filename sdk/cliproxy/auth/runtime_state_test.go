package auth

import (
	"context"
	"testing"
	"time"
)

type fakeRuntimeStateStore struct {
	states map[string]AuthRuntimeState
	saved  map[string]AuthRuntimeState
}

func (s *fakeRuntimeStateStore) Load(context.Context) (map[string]AuthRuntimeState, error) {
	out := make(map[string]AuthRuntimeState, len(s.states))
	for id, state := range s.states {
		out[id] = state
	}
	return out, nil
}

func (s *fakeRuntimeStateStore) Save(_ context.Context, authID string, state AuthRuntimeState) error {
	if s.saved == nil {
		s.saved = make(map[string]AuthRuntimeState)
	}
	s.saved[authID] = state
	return nil
}

func (s *fakeRuntimeStateStore) Delete(context.Context, string) error { return nil }

func TestAuthRuntimeStateRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 30, 0, 0, time.UTC)
	a := &Auth{
		ID:            "auth-1",
		Status:        StatusError,
		StatusMessage: "quota",
		Unavailable:   true,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: now.Add(time.Hour),
			BackoffLevel:  2,
		},
		LastError: &Error{Code: "rate_limit", Message: "too many requests", HTTPStatus: 429},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(30 * time.Minute),
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(30 * time.Minute), BackoffLevel: 1},
			},
		},
		Success: 3,
		Failed:  1,
	}
	a.recordRecentRequest(now, true)
	a.recordRecentRequest(now, false)

	state := a.RuntimeStateSnapshot()
	restored := &Auth{ID: "auth-1", Status: StatusActive}
	restored.ApplyRuntimeState(state)

	if restored.Success != 3 || restored.Failed != 1 {
		t.Fatalf("request counters = (%d,%d), want (3,1)", restored.Success, restored.Failed)
	}
	buckets := restored.RecentRequestsSnapshot(now)
	last := buckets[len(buckets)-1]
	if last.Success != 1 || last.Failed != 1 {
		t.Fatalf("recent bucket = (%d,%d), want (1,1)", last.Success, last.Failed)
	}
	if !restored.Quota.Exceeded || restored.Quota.BackoffLevel != 2 {
		t.Fatalf("quota state not restored: %#v", restored.Quota)
	}
	if restored.ModelStates["gpt-5"] == nil || !restored.ModelStates["gpt-5"].Quota.Exceeded {
		t.Fatalf("model quota state not restored: %#v", restored.ModelStates)
	}
}

func TestManagerAppliesLoadedRuntimeStateOnRegister(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 30, 0, 0, time.UTC)
	store := &fakeRuntimeStateStore{
		states: map[string]AuthRuntimeState{
			"auth-1": {
				Success: 5,
				Failed:  2,
				Status:  StatusError,
				Quota:   QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)},
			},
		},
	}
	mgr := NewManager(nil, nil, nil)
	mgr.SetRuntimeStateStore(store)
	if err := mgr.LoadRuntimeStates(context.Background()); err != nil {
		t.Fatalf("LoadRuntimeStates error: %v", err)
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "auth-1",
		Provider: "gemini",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	got, ok := mgr.GetByID("auth-1")
	if !ok {
		t.Fatal("auth not registered")
	}
	if got.Success != 5 || got.Failed != 2 {
		t.Fatalf("request counters = (%d,%d), want (5,2)", got.Success, got.Failed)
	}
	if got.Status != StatusError || !got.Quota.Exceeded {
		t.Fatalf("runtime quota status not applied: status=%s quota=%#v", got.Status, got.Quota)
	}
}

func TestManagerPersistsRuntimeStateFromMarkResult(t *testing.T) {
	store := &fakeRuntimeStateStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetRuntimeStateStore(store)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "auth-1",
		Provider: "gemini",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{AuthID: "auth-1", Success: true})
	mgr.flushPersistQueue()

	state, ok := store.saved["auth-1"]
	if !ok {
		t.Fatal("runtime state was not persisted")
	}
	if state.Success != 1 || state.Failed != 0 {
		t.Fatalf("persisted counters = (%d,%d), want (1,0)", state.Success, state.Failed)
	}
}
