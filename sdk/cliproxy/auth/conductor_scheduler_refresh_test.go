package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type schedulerProviderTestExecutor struct {
	provider string
}

func (e schedulerProviderTestExecutor) Identifier() string { return e.provider }

func (e schedulerProviderTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerProviderTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e schedulerProviderTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e schedulerProviderTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerProviderTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type unauthorizedRefreshTestExecutor struct {
	schedulerProviderTestExecutor
	err error
}

func (e unauthorizedRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if e.err != nil {
		return nil, e.err
	}
	return nil, errors.New("token refresh failed with status 401: invalid_grant")
}

type permanentRefreshStatusError struct {
	code int
	msg  string
}

func (e permanentRefreshStatusError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return "permanent refresh failure"
}

func (e permanentRefreshStatusError) StatusCode() int { return e.code }

func (e permanentRefreshStatusError) IsPermanentAuthError() bool { return true }

type requestTimeRefreshFailoverExecutor struct {
	provider string
	badID    string
}

func (e *requestTimeRefreshFailoverExecutor) Identifier() string { return e.provider }

func (e *requestTimeRefreshFailoverExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth != nil && auth.ID == e.badID {
		coord := RefreshCoordinatorFrom(ctx)
		if coord == nil {
			return cliproxyexecutor.Response{}, errors.New("missing refresh coordinator")
		}
		_, err := coord(ctx, auth)
		return cliproxyexecutor.Response{}, err
	}
	if auth == nil {
		return cliproxyexecutor.Response{}, errors.New("missing auth")
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *requestTimeRefreshFailoverExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *requestTimeRefreshFailoverExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth != nil && auth.ID == e.badID {
		return nil, permanentRefreshStatusError{
			code: http.StatusUnauthorized,
			msg:  `token refresh failed with status 401: {"error":{"message":"Your refresh token has already been used to generate a new access token. Please try signing in again.","type":"invalid_request_error","code":"refresh_token_reused"}}`,
		}
	}
	return auth, nil
}

func (e *requestTimeRefreshFailoverExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *requestTimeRefreshFailoverExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type proactiveCodexRefreshExecutor struct {
	provider      string
	refreshCalls  int
	executeTokens []string
}

func (e *proactiveCodexRefreshExecutor) Identifier() string { return e.provider }

func (e *proactiveCodexRefreshExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	token := authRefreshReloadString(auth, "access_token", "accessToken", "api_key")
	e.executeTokens = append(e.executeTokens, token)
	return cliproxyexecutor.Response{Payload: []byte(token)}, nil
}

func (e *proactiveCodexRefreshExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *proactiveCodexRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls++
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	oldToken := authRefreshReloadString(auth, "access_token", "accessToken", "api_key")
	updated.Metadata["access_token"] = "new-access-token"
	updated.Metadata["refresh_token"] = "new-refresh-token"
	updated.Metadata["expired"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	updated.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if updated.Attributes == nil {
		updated.Attributes = map[string]string{}
	}
	if strings.TrimSpace(updated.Attributes["api_key"]) == strings.TrimSpace(oldToken) {
		updated.Attributes["api_key"] = "new-access-token"
	}
	return updated, nil
}

func (e *proactiveCodexRefreshExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	token := authRefreshReloadString(auth, "access_token", "accessToken", "api_key")
	return cliproxyexecutor.Response{Payload: []byte(token)}, nil
}

func (e *proactiveCodexRefreshExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestManager_RefreshAuthUnauthorizedFailureStopsAutoRefreshRetry(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(unauthorizedRefreshTestExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
	})

	auth := &Auth{
		ID:       "unauthorized-refresh",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "x@example.com",
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.refreshAuth(ctx, auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after refresh", auth.ID)
	}
	if updated.LastError == nil {
		t.Fatal("expected unauthorized refresh failure to be recorded")
	}
	if got := updated.LastError.StatusCode(); got != http.StatusUnauthorized {
		t.Fatalf("LastError.StatusCode() = %d, want %d", got, http.StatusUnauthorized)
	}
	if updated.LastError.Code != "unauthorized" {
		t.Fatalf("LastError.Code = %q, want unauthorized", updated.LastError.Code)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %s, want zero for unauthorized refresh failure", updated.NextRefreshAfter)
	}
	if !updated.NextRetryAfter.After(time.Now()) {
		t.Fatalf("NextRetryAfter = %s, want future routing cooldown", updated.NextRetryAfter)
	}
	if AuthAvailableForModel(updated, "gpt-5-test", time.Now()) {
		t.Fatal("expected unauthorized refresh failure to make auth unavailable for routing")
	}
	now := time.Now()
	if manager.shouldRefresh(updated, now) {
		t.Fatal("expected unauthorized auth to stop refresh attempts")
	}
	if _, shouldSchedule := nextRefreshCheckAt(now, updated, time.Second); shouldSchedule {
		t.Fatal("expected unauthorized auth to be removed from the auto-refresh schedule")
	}
}

func TestManager_PermanentRefreshFailureIsAuthWideEvenWhenProviderReturns400(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(unauthorizedRefreshTestExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
		err: permanentRefreshStatusError{
			code: http.StatusBadRequest,
			msg:  `token refresh failed with status 400: {"error":"invalid_grant","error_description":"refresh token already used"}`,
		},
	})

	auth := &Auth{
		ID:       "permanent-refresh-400",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "x@example.com",
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.refreshAuth(ctx, auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after refresh", auth.ID)
	}
	if updated.LastError == nil {
		t.Fatal("expected permanent refresh failure to be recorded")
	}
	if got := updated.LastError.StatusCode(); got != http.StatusUnauthorized {
		t.Fatalf("LastError.StatusCode() = %d, want %d", got, http.StatusUnauthorized)
	}
	if !updated.Unavailable {
		t.Fatal("expected permanent refresh failure to mark auth unavailable")
	}
	if !updated.NextRetryAfter.After(time.Now()) {
		t.Fatalf("NextRetryAfter = %s, want future routing cooldown", updated.NextRetryAfter)
	}
	if AuthAvailableForModel(updated, "gpt-5-test", time.Now()) {
		t.Fatal("expected permanent refresh failure to block the auth for all models")
	}
}

func TestManager_RequestTimeRefreshTokenReusedFailsOverToNextAuth(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	badAuth := &Auth{ID: "aa-refresh-token-reused", Provider: "codex"}
	goodAuth := &Auth{ID: "bb-valid-refresh", Provider: "codex"}
	model := "gpt-5-refresh-token-reused-failover"
	exec := &requestTimeRefreshFailoverExecutor{provider: "codex", badID: badAuth.ID}
	manager.RegisterExecutor(exec)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(badAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := manager.Register(ctx, badAuth); errRegister != nil {
		t.Fatalf("register bad auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(ctx, goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	resp, errExecute := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v, want failover success", errExecute)
	}
	if string(resp.Payload) != goodAuth.ID {
		t.Fatalf("Execute() payload = %q, want %q", string(resp.Payload), goodAuth.ID)
	}

	updatedBad, ok := manager.GetByID(badAuth.ID)
	if !ok || updatedBad == nil {
		t.Fatalf("expected bad auth %q after request", badAuth.ID)
	}
	if !updatedBad.Unavailable {
		t.Fatal("expected refresh_token_reused to mark auth unavailable")
	}
	if updatedBad.LastError == nil || updatedBad.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("LastError = %+v, want unauthorized", updatedBad.LastError)
	}
	if AuthAvailableForModel(updatedBad, model, time.Now()) {
		t.Fatal("expected refresh_token_reused auth to be blocked for routing")
	}
}

func TestManager_RequestTimeProactiveCodexRefreshBeforeExpiry(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	exec := &proactiveCodexRefreshExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)

	now := time.Now().UTC()
	auth := &Auth{
		ID:       "codex-proactive-expiry",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "old-access-token",
		},
		Metadata: map[string]any{
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"email":         "codex@example.com",
			"expired":       now.Add(time.Minute).Format(time.RFC3339),
			"last_refresh":  now.Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	resp, errExecute := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(resp.Payload); got != "new-access-token" {
		t.Fatalf("Execute() token = %q, want refreshed token", got)
	}
	if exec.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", exec.refreshCalls)
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected updated auth %q", auth.ID)
	}
	if got := authRefreshReloadString(updated, "access_token", "accessToken", "api_key"); got != "new-access-token" {
		t.Fatalf("stored token = %q, want refreshed token", got)
	}
}

func TestManager_RequestTimeProactiveCodexRefreshUsesEightDayFallback(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name        string
		lastRefresh time.Time
		wantRefresh bool
	}{
		{
			name:        "fresh last refresh",
			lastRefresh: time.Now().UTC().Add(-7 * 24 * time.Hour),
			wantRefresh: false,
		},
		{
			name:        "stale last refresh",
			lastRefresh: time.Now().UTC().Add(-9 * 24 * time.Hour),
			wantRefresh: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			exec := &proactiveCodexRefreshExecutor{provider: "codex"}
			manager.RegisterExecutor(exec)
			auth := &Auth{
				ID:       "codex-proactive-" + strings.ReplaceAll(tc.name, " ", "-"),
				Provider: "codex",
				Attributes: map[string]string{
					"api_key": "old-access-token",
				},
				Metadata: map[string]any{
					"access_token":  "old-access-token",
					"refresh_token": "old-refresh-token",
					"email":         "codex@example.com",
					"last_refresh":  tc.lastRefresh.Format(time.RFC3339),
				},
			}
			if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			resp, errExecute := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
			if errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}
			wantToken := "old-access-token"
			if tc.wantRefresh {
				wantToken = "new-access-token"
			}
			if got := string(resp.Payload); got != wantToken {
				t.Fatalf("Execute() token = %q, want %q", got, wantToken)
			}
			wantRefreshCalls := 0
			if tc.wantRefresh {
				wantRefreshCalls = 1
			}
			if exec.refreshCalls != wantRefreshCalls {
				t.Fatalf("refresh calls = %d, want %d", exec.refreshCalls, wantRefreshCalls)
			}
		})
	}
}

func TestManager_RefreshSchedulerEntry_RebuildsSupportedModelSetAfterModelRegistration(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name  string
		prime func(*Manager, *Auth) error
	}{
		{
			name: "register",
			prime: func(manager *Manager, auth *Auth) error {
				_, errRegister := manager.Register(ctx, auth)
				return errRegister
			},
		},
		{
			name: "update",
			prime: func(manager *Manager, auth *Auth) error {
				_, errRegister := manager.Register(ctx, auth)
				if errRegister != nil {
					return errRegister
				}
				updated := auth.Clone()
				updated.Metadata = map[string]any{"updated": true}
				_, errUpdate := manager.Update(ctx, updated)
				return errUpdate
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			auth := &Auth{
				ID:       "refresh-entry-" + testCase.name,
				Provider: "gemini",
			}
			if errPrime := testCase.prime(manager, auth); errPrime != nil {
				t.Fatalf("prime auth %s: %v", testCase.name, errPrime)
			}

			registerSchedulerModels(t, "gemini", "scheduler-refresh-model", auth.ID)

			got, errPick := manager.scheduler.pickSingle(ctx, "gemini", "scheduler-refresh-model", cliproxyexecutor.Options{}, nil)
			var authErr *Error
			if !errors.As(errPick, &authErr) || authErr == nil {
				t.Fatalf("pickSingle() before refresh error = %v, want auth_not_found", errPick)
			}
			if authErr.Code != "auth_not_found" {
				t.Fatalf("pickSingle() before refresh code = %q, want %q", authErr.Code, "auth_not_found")
			}
			if got != nil {
				t.Fatalf("pickSingle() before refresh auth = %v, want nil", got)
			}

			manager.RefreshSchedulerEntry(auth.ID)

			got, errPick = manager.scheduler.pickSingle(ctx, "gemini", "scheduler-refresh-model", cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickSingle() after refresh error = %v", errPick)
			}
			if got == nil || got.ID != auth.ID {
				t.Fatalf("pickSingle() after refresh auth = %v, want %q", got, auth.ID)
			}
		})
	}
}

func TestManager_ReconcileRegistryModelStates_PreservesActiveQuotaCooldown(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	model := "gpt-5.4-codex"
	authID := "codex-quota-preserve"
	registerSchedulerModels(t, "codex", model, authID)

	if _, errRegister := manager.Register(ctx, &Auth{
		ID:       authID,
		Provider: "codex",
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	retryAfter := 2 * time.Hour
	manager.MarkResult(ctx, Result{
		AuthID:     authID,
		Provider:   "codex",
		Model:      model,
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"},
	})

	manager.ReconcileRegistryModelStates(ctx, authID)

	got, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth not found")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatalf("model state missing after reconcile: %#v", got.ModelStates)
	}
	if !state.Quota.Exceeded || !state.Unavailable || !state.NextRetryAfter.After(time.Now()) {
		t.Fatalf("quota cooldown was not preserved: %#v", state)
	}

	picked, errPick := manager.scheduler.pickSingle(ctx, "codex", model, cliproxyexecutor.Options{}, nil)
	var cooldownErr *modelCooldownError
	if !errors.As(errPick, &cooldownErr) {
		t.Fatalf("pickSingle() error = %v, want modelCooldownError", errPick)
	}
	if picked != nil {
		t.Fatalf("pickSingle() auth = %v, want nil while quota cooldown is active", picked)
	}
}

func TestManager_ReconcileRegistryModelStates_ClearsExpiredQuotaCooldown(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	model := "gpt-5.4-codex"
	authID := "codex-quota-expired"
	registerSchedulerModels(t, "codex", model, authID)

	past := time.Now().Add(-1 * time.Minute)
	if _, errRegister := manager.Register(ctx, &Auth{
		ID:       authID,
		Provider: "codex",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: past,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: past},
				LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"},
				UpdatedAt:      past,
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.ReconcileRegistryModelStates(ctx, authID)

	got, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth not found")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if !modelStateIsClean(state) {
		t.Fatalf("expired quota cooldown was not cleared: %#v", state)
	}

	picked, errPick := manager.scheduler.pickSingle(ctx, "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() after expired cooldown error = %v", errPick)
	}
	if picked == nil || picked.ID != authID {
		t.Fatalf("pickSingle() auth = %v, want %q", picked, authID)
	}
}

func TestManager_PickNext_RebuildsSchedulerAfterModelCooldownError(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "gemini"})

	registerSchedulerModels(t, "gemini", "scheduler-cooldown-rebuild-model", "cooldown-stale-old")

	oldAuth := &Auth{
		ID:       "cooldown-stale-old",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, oldAuth); errRegister != nil {
		t.Fatalf("register old auth: %v", errRegister)
	}

	manager.MarkResult(ctx, Result{
		AuthID:   oldAuth.ID,
		Provider: "gemini",
		Model:    "scheduler-cooldown-rebuild-model",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})

	newAuth := &Auth{
		ID:       "cooldown-stale-new",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, newAuth); errRegister != nil {
		t.Fatalf("register new auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(newAuth.ID, "gemini", []*registry.ModelInfo{{ID: "scheduler-cooldown-rebuild-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(newAuth.ID)
	})

	got, errPick := manager.scheduler.pickSingle(ctx, "gemini", "scheduler-cooldown-rebuild-model", cliproxyexecutor.Options{}, nil)
	var cooldownErr *modelCooldownError
	if !errors.As(errPick, &cooldownErr) {
		t.Fatalf("pickSingle() before sync error = %v, want modelCooldownError", errPick)
	}
	if got != nil {
		t.Fatalf("pickSingle() before sync auth = %v, want nil", got)
	}

	got, executor, errPick := manager.pickNext(ctx, "gemini", "scheduler-cooldown-rebuild-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if executor == nil {
		t.Fatal("pickNext() executor = nil")
	}
	if got == nil || got.ID != newAuth.ID {
		t.Fatalf("pickNext() auth = %v, want %q", got, newAuth.ID)
	}
}
