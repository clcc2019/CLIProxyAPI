package auth

import (
	"context"
	"testing"
	"time"
)

// authScopedTestErr is a minimal stand-in for the oauth executor's 429 wrapper
// used to exercise the conductor's MarkResult elevation logic without
// importing the executor package.
type authScopedTestErr struct{ code int }

func (e *authScopedTestErr) Error() string             { return "auth-scoped test error" }
func (e *authScopedTestErr) StatusCode() int           { return e.code }
func (e *authScopedTestErr) IsAuthScopedFailure() bool { return true }

// TestMarkResult_AuthScoped429_SuspendsEntireAuth verifies that when an
// executor returns an auth-scoped failure (i.e., OAuth's shared-bucket
// AGENTIC_REQUEST 429), the conductor suspends the whole auth so session
// affinity can slide to the next credential instead of only blocking the
// triggering model.
func TestMarkResult_AuthScoped429_SuspendsEntireAuth(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	t.Cleanup(mgr.stopPersistLoop)

	auth := &Auth{
		ID:       "oauth-shared-bucket",
		Provider: "oauth",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "oauth"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Simulate a 429 on the "claude-sonnet-4.5" model. Without AuthScoped,
	// the current code would only suspend that one model and leave
	// "claude-haiku" unblocked on this same auth.
	result := Result{
		AuthID:   auth.ID,
		Provider: "oauth",
		Model:    "claude-sonnet-4.5",
		Success:  false,
	}
	applyResultError(&result, &authScopedTestErr{code: 429})
	if !result.AuthScoped {
		t.Fatalf("applyResultError should have set AuthScoped=true, got false")
	}

	mgr.MarkResult(context.Background(), result)

	mgr.mu.RLock()
	stored := mgr.auths[auth.ID]
	mgr.mu.RUnlock()
	if stored == nil {
		t.Fatal("auth missing after MarkResult")
	}
	if !stored.Unavailable {
		t.Fatal("expected auth.Unavailable=true after auth-scoped 429")
	}
	if stored.Status != StatusError {
		t.Fatalf("expected auth.Status=StatusError, got %v", stored.Status)
	}
	// The triggering model should also be marked so selectors that check
	// per-model state route traffic off it.
	state, ok := stored.ModelStates["claude-sonnet-4.5"]
	if !ok || state == nil {
		t.Fatal("triggering model state not recorded")
	}
	if !state.Unavailable {
		t.Fatal("triggering model should also be unavailable")
	}
	if stored.NextRetryAfter.Before(time.Now().Add(500 * time.Millisecond)) {
		t.Fatalf("expected NextRetryAfter to be scheduled in the future, got %v", stored.NextRetryAfter)
	}
	if stored.Quota.NextRecoverAt.Before(time.Now().Add(quotaRefreshInterval - time.Minute)) {
		t.Fatalf("expected auth quota recover time near 5h refresh window, got %v", stored.Quota.NextRecoverAt)
	}
	// auth-scope quota marker must be set so the selector treats the whole
	// credential as exhausted and session affinity moves on.
	if !stored.Quota.Exceeded {
		t.Fatal("expected auth.Quota.Exceeded=true on auth-scoped 429")
	}
}

// TestMarkResult_ModelScoped429_OtherModelsStillRoutable guards the
// pre-existing per-model behavior: executors that do NOT mark AuthScoped
// suspend only the triggering model. Other models that already have a
// successful request recorded on this auth must remain selectable.
//
// Note: isAuthBlockedForModel falls through to auth-level state when a
// queried model has no per-model state and auth-level flags are set —
// that's intentional for the auth-scoped path but means this test must
// pre-register the unrelated model with a successful MarkResult so its
// ModelState takes priority over the aggregate unavailable flag.
func TestMarkResult_ModelScoped429_OtherModelsStillRoutable(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	t.Cleanup(mgr.stopPersistLoop)

	auth := &Auth{
		ID:       "per-model-quota",
		Provider: "kimi",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "kimi"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Register unrelated model as successful so it has clean per-model state.
	mgr.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: "kimi", Model: "kimi-k2.5", Success: true,
	})

	// Now 429 the other model.
	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "kimi",
		Model:    "gpt-5",
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
	})

	mgr.mu.RLock()
	stored := mgr.auths[auth.ID]
	mgr.mu.RUnlock()
	if stored == nil {
		t.Fatal("auth missing after MarkResult")
	}
	now := time.Now()
	if AuthAvailableForModel(stored, "gpt-5", now) {
		t.Fatal("triggering model should be blocked after 429")
	}
	state := stored.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("triggering model state missing")
	}
	if state.Quota.NextRecoverAt.Before(now.Add(quotaRefreshInterval - time.Minute)) {
		t.Fatalf("expected model quota recover time near 5h refresh window, got %v", state.Quota.NextRecoverAt)
	}
	if !state.NextRetryAfter.Equal(state.Quota.NextRecoverAt) {
		t.Fatalf("NextRetryAfter = %v, want quota recover time %v", state.NextRetryAfter, state.Quota.NextRecoverAt)
	}
	if !AuthAvailableForModel(stored, "kimi-k2.5", now) {
		t.Fatal("unrelated model with clean per-model state should remain routable")
	}
}

func TestQuotaRecoverAtUsesFiveHourRefreshWindow(t *testing.T) {
	now := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)

	if got, want := quotaRecoverAt(now, nil), now.Add(quotaRefreshInterval); !got.Equal(want) {
		t.Fatalf("quotaRecoverAt() = %v, want %v", got, want)
	}

	shortRetryAfter := 2 * time.Minute
	if got, want := quotaRecoverAt(now, &shortRetryAfter), now.Add(quotaRefreshInterval); !got.Equal(want) {
		t.Fatalf("quotaRecoverAt(short retry-after) = %v, want %v", got, want)
	}

	longRetryAfter := 6 * time.Hour
	if got, want := quotaRecoverAt(now, &longRetryAfter), now.Add(longRetryAfter); !got.Equal(want) {
		t.Fatalf("quotaRecoverAt(long retry-after) = %v, want %v", got, want)
	}
}

// TestMarkResult_AuthScoped429_AllModelsBlocked complements the model-scoped
// test above: when AuthScoped is true, even models that have no recorded
// per-model state must be treated as blocked on this auth so session
// affinity picks a different credential for every request.
func TestMarkResult_AuthScoped429_AllModelsBlocked(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	t.Cleanup(mgr.stopPersistLoop)

	auth := &Auth{
		ID:       "oauth-shared",
		Provider: "oauth",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "oauth"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}

	result := Result{
		AuthID:   auth.ID,
		Provider: "oauth",
		Model:    "claude-sonnet-4.5",
		Success:  false,
	}
	applyResultError(&result, &authScopedTestErr{code: 429})
	mgr.MarkResult(context.Background(), result)

	mgr.mu.RLock()
	stored := mgr.auths[auth.ID]
	mgr.mu.RUnlock()
	if stored == nil {
		t.Fatal("auth missing after MarkResult")
	}
	// With AuthScoped, the per-model check should treat every model as
	// blocked because auth-level NextRetryAfter applies globally. The
	// current isAuthBlockedForModel walks ModelStates first; for a model
	// with no entry, it falls through to auth-level check which we need
	// to verify here.
	now := time.Now()
	if AuthAvailableForModel(stored, "claude-sonnet-4.5", now) {
		t.Fatal("triggering model must be blocked after auth-scoped 429")
	}
	// unknown model should also be blocked via auth-level suspension
	if AuthAvailableForModel(stored, "claude-haiku-4.5", now) {
		t.Fatal("other models on auth-scoped-failed auth must also be blocked")
	}
}
