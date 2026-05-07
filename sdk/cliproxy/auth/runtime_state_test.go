package auth

import (
	"context"
	"encoding/json"
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

type captureAuthStore struct {
	items []*Auth
	saved map[string]*Auth
}

func (s *captureAuthStore) List(context.Context) ([]*Auth, error) {
	out := make([]*Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item.Clone())
	}
	return out, nil
}

func (s *captureAuthStore) Save(_ context.Context, auth *Auth) (string, error) {
	if s.saved == nil {
		s.saved = make(map[string]*Auth)
	}
	s.saved[auth.ID] = auth.Clone()
	return "", nil
}

func (s *captureAuthStore) Delete(context.Context, string) error { return nil }

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

func TestAuthRuntimeStateMetadataRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 30, 0, 0, time.UTC)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusError,
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Hour),
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour), BackoffLevel: 1},
				LastError:      &Error{HTTPStatus: 429, Message: "usage_limit_reached"},
				UpdatedAt:      now,
			},
		},
	}

	auth.SetRuntimeStateMetadata()
	if _, ok := auth.Metadata[runtimeStateMetadataKey]; !ok {
		t.Fatalf("runtime state metadata key %q missing", runtimeStateMetadataKey)
	}

	rawMetadata, errMarshal := json.Marshal(auth.Metadata)
	if errMarshal != nil {
		t.Fatalf("marshal metadata: %v", errMarshal)
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(rawMetadata, &metadata); errUnmarshal != nil {
		t.Fatalf("unmarshal metadata: %v", errUnmarshal)
	}

	restored := &Auth{ID: "auth-1", Provider: "codex", Status: StatusActive, Metadata: metadata}
	if !restored.ApplyRuntimeStateFromMetadata() {
		t.Fatal("runtime state metadata was not applied")
	}
	state := restored.ModelStates["gpt-5-codex"]
	if state == nil || !state.Quota.Exceeded || !state.NextRetryAfter.After(now) {
		t.Fatalf("metadata quota state not restored: %#v", restored.ModelStates)
	}
}

func TestManagerLoadAppliesRuntimeStateFromAuthMetadata(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 30, 0, 0, time.UTC)
	state := AuthRuntimeState{
		Version:        1,
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Hour),
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Hour),
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)},
			},
		},
	}
	store := &captureAuthStore{
		items: []*Auth{{
			ID:       "auth-1",
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{
				"type":                  "codex",
				runtimeStateMetadataKey: state,
			},
		}},
	}
	mgr := NewManager(store, nil, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load error: %v", err)
	}

	got, ok := mgr.GetByID("auth-1")
	if !ok {
		t.Fatal("auth not loaded")
	}
	if got.Status != StatusError || !got.Quota.Exceeded {
		t.Fatalf("runtime state metadata not applied: status=%s quota=%#v", got.Status, got.Quota)
	}
	if got.ModelStates["gpt-5-codex"] == nil || !got.ModelStates["gpt-5-codex"].Quota.Exceeded {
		t.Fatalf("model quota state not applied: %#v", got.ModelStates)
	}
}

func TestManagerPersistEmbedsRuntimeStateInAuthMetadata(t *testing.T) {
	store := &captureAuthStore{}
	mgr := NewManager(store, nil, nil)
	if _, err := mgr.Register(context.Background(), &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex"},
	}); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	store.saved = nil

	retryAfter := time.Hour
	mgr.MarkResult(context.Background(), Result{
		AuthID:     "auth-1",
		Provider:   "codex",
		Model:      "gpt-5-codex",
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &Error{HTTPStatus: 429, Message: "usage_limit_reached"},
	})
	mgr.flushPersistQueue()

	saved := store.saved["auth-1"]
	if saved == nil {
		t.Fatal("auth was not persisted")
	}
	raw := saved.Metadata[runtimeStateMetadataKey]
	if raw == nil {
		t.Fatalf("runtime state metadata key %q missing: %#v", runtimeStateMetadataKey, saved.Metadata)
	}
	data, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		t.Fatalf("marshal runtime metadata: %v", errMarshal)
	}
	var state AuthRuntimeState
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		t.Fatalf("unmarshal runtime metadata: %v", errUnmarshal)
	}
	if state.ModelStates["gpt-5-codex"] == nil || !state.ModelStates["gpt-5-codex"].Quota.Exceeded {
		t.Fatalf("persisted runtime metadata missing quota state: %#v", state.ModelStates)
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
