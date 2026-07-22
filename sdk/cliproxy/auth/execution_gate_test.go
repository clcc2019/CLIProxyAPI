package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type quotaGateTestExecutor struct {
	provider string

	prepareStarted chan string
	releasePrepare chan struct{}

	mu           sync.Mutex
	executeAuths []string
	countAuths   []string
	streamAuths  []string
}

func newQuotaGateTestExecutor(provider string) *quotaGateTestExecutor {
	return &quotaGateTestExecutor{
		provider:       provider,
		prepareStarted: make(chan string, 1),
		releasePrepare: make(chan struct{}),
	}
}

func (e *quotaGateTestExecutor) Identifier() string { return e.provider }

func (e *quotaGateTestExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth != nil && auth.ID == "quota-gate-primary-"+e.provider
}

func (e *quotaGateTestExecutor) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	select {
	case e.prepareStarted <- auth.ID:
	default:
	}
	select {
	case <-e.releasePrepare:
		return auth.Clone(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *quotaGateTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeAuths = append(e.executeAuths, auth.ID)
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *quotaGateTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamAuths = append(e.streamAuths, auth.ID)
	e.mu.Unlock()
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *quotaGateTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *quotaGateTestExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.countAuths = append(e.countAuths, auth.ID)
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *quotaGateTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *quotaGateTestExecutor) calls(mode string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var source []string
	switch mode {
	case "execute":
		source = e.executeAuths
	case "count":
		source = e.countAuths
	case "stream":
		source = e.streamAuths
	}
	return append([]string(nil), source...)
}

func TestManagerExecutionAdmissionRejectsAuthCooldownWrittenAfterSelection(t *testing.T) {
	const model = "quota-gate-model"
	testCases := []struct {
		name   string
		invoke func(context.Context, *Manager) (string, error)
	}{
		{
			name: "execute",
			invoke: func(ctx context.Context, manager *Manager) (string, error) {
				response, err := manager.Execute(ctx, []string{"quota-gate-execute"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return string(response.Payload), err
			},
		},
		{
			name: "count",
			invoke: func(ctx context.Context, manager *Manager) (string, error) {
				response, err := manager.ExecuteCount(ctx, []string{"quota-gate-count"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return string(response.Payload), err
			},
		},
		{
			name: "stream",
			invoke: func(ctx context.Context, manager *Manager) (string, error) {
				result, err := manager.ExecuteStream(ctx, []string{"quota-gate-stream"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				if err != nil {
					return "", err
				}
				payload := make([]byte, 0, 64)
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						return "", chunk.Err
					}
					payload = append(payload, chunk.Payload...)
				}
				return string(payload), nil
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			provider := "quota-gate-" + testCase.name
			primaryID := "quota-gate-primary-" + provider
			backupID := "quota-gate-backup-" + provider
			executor := newQuotaGateTestExecutor(provider)
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			// A locally rejected credential must still fail over, even when the
			// normal credential retry limit would otherwise stop after one try.
			manager.SetRetryConfig(0, 0, 1)
			manager.RegisterExecutor(executor)

			for _, candidate := range []*Auth{
				{ID: primaryID, Provider: provider, Attributes: map[string]string{"priority": "100"}},
				{ID: backupID, Provider: provider, Attributes: map[string]string{"priority": "1"}},
			} {
				if _, err := manager.Register(WithSkipPersist(context.Background()), candidate); err != nil {
					t.Fatalf("register %s: %v", candidate.ID, err)
				}
			}
			registry.GetGlobalRegistry().RegisterClient(primaryID, provider, []*registry.ModelInfo{{ID: model}})
			registry.GetGlobalRegistry().RegisterClient(backupID, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() {
				registry.GetGlobalRegistry().UnregisterClient(primaryID)
				registry.GetGlobalRegistry().UnregisterClient(backupID)
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			released := false
			t.Cleanup(func() {
				if !released {
					close(executor.releasePrepare)
				}
			})

			type outcome struct {
				payload string
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				payload, err := testCase.invoke(ctx, manager)
				done <- outcome{payload: payload, err: err}
			}()

			select {
			case preparedID := <-executor.prepareStarted:
				if preparedID != primaryID {
					t.Fatalf("prepared auth = %q, want selected primary %q", preparedID, primaryID)
				}
			case <-ctx.Done():
				t.Fatalf("primary auth was not prepared: %v", ctx.Err())
			}

			manager.MarkAuthQuotaCooldown(context.Background(), primaryID, time.Now().Add(time.Hour))
			close(executor.releasePrepare)
			released = true

			select {
			case result := <-done:
				if result.err != nil {
					t.Fatalf("request error: %v", result.err)
				}
				if result.payload != backupID {
					t.Fatalf("response auth = %q, want fallback %q", result.payload, backupID)
				}
			case <-ctx.Done():
				t.Fatalf("request did not finish: %v", ctx.Err())
			}

			if calls := executor.calls(testCase.name); len(calls) != 1 || calls[0] != backupID {
				t.Fatalf("%s calls = %v, want only fallback %q", testCase.name, calls, backupID)
			}
		})
	}
}

func TestManagerQuotaCooldownSurvivesLateSuccessfulResult(t *testing.T) {
	const (
		provider = "quota-cooldown-late-success"
		authID   = "quota-cooldown-late-success-auth"
		model    = "quota-cooldown-late-success-model"
	)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       authID,
		Provider: provider,
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			model: {Status: StatusActive},
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	recoverAt := time.Now().Add(time.Hour)
	manager.MarkAuthQuotaCooldown(context.Background(), authID, recoverAt)
	// This represents a request admitted before the usage probe learned that
	// the account was exhausted. Its eventual success is not reset evidence.
	manager.MarkResult(context.Background(), Result{AuthID: authID, Provider: provider, Model: model, Success: true})

	stored, ok := manager.GetByID(authID)
	if !ok || stored == nil {
		t.Fatal("auth missing after quota cooldown")
	}
	if AuthAvailableForModel(stored, model, time.Now()) {
		t.Fatal("late successful result revived an auth-scoped quota cooldown")
	}
	if !stored.Unavailable || !stored.Quota.Exceeded || !stored.Quota.AuthScope {
		t.Fatalf("quota state was not preserved: unavailable=%v quota=%+v", stored.Unavailable, stored.Quota)
	}
	if !stored.NextRetryAfter.Equal(recoverAt) {
		t.Fatalf("next retry = %v, want reset time %v", stored.NextRetryAfter, recoverAt)
	}

	if !manager.ClearAuthQuotaCooldown(context.Background(), authID) {
		t.Fatal("ClearAuthQuotaCooldown = false, want true")
	}
	cleared, ok := manager.GetByID(authID)
	if !ok || cleared == nil || !AuthAvailableForModel(cleared, model, time.Now()) {
		t.Fatal("auth should become available after the explicit quota reset")
	}
}

func TestManagerRegisterDoesNotOverwriteActiveQuotaCooldown(t *testing.T) {
	const (
		provider = "quota-cooldown-register"
		authID   = "quota-cooldown-register-auth"
		model    = "quota-cooldown-register-model"
	)
	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: authID, Provider: provider, Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkAuthQuotaCooldown(context.Background(), authID, time.Now().Add(time.Hour))

	// Model the stale auth-file snapshot a reload can produce. It contains no
	// runtime quota state, so accepting it verbatim would reopen the auth.
	if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: authID, Provider: provider, Status: StatusActive}); err != nil {
		t.Fatalf("re-register auth: %v", err)
	}
	stored, ok := manager.GetByID(authID)
	if !ok || stored == nil {
		t.Fatal("auth missing after re-register")
	}
	if AuthAvailableForModel(stored, model, time.Now()) {
		t.Fatal("stale Register call revived an auth-scoped quota cooldown")
	}
}
