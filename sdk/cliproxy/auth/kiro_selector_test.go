package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type kiroSelectorTestExecutor struct{}

func (kiroSelectorTestExecutor) Identifier() string { return "kiro" }

func (kiroSelectorTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (kiroSelectorTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (kiroSelectorTestExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }

func (kiroSelectorTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (kiroSelectorTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerPickNext_KiroUsesFillFirstUntilAuthScoped429(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	t.Cleanup(mgr.StopAutoRefresh)
	mgr.RegisterExecutor(kiroSelectorTestExecutor{})

	for _, auth := range []*Auth{
		{ID: "kiro-auth-b", Provider: "kiro", Metadata: map[string]any{"type": "kiro"}},
		{ID: "kiro-auth-a", Provider: "kiro", Metadata: map[string]any{"type": "kiro"}},
		{ID: "kiro-auth-c", Provider: "kiro", Metadata: map[string]any{"type": "kiro"}},
	} {
		if _, err := mgr.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}

	opts := cliproxyexecutor.Options{}
	for i := 0; i < 4; i++ {
		picked, _, err := mgr.pickNext(context.Background(), "kiro", "", opts, nil)
		if err != nil {
			t.Fatalf("pickNext #%d: %v", i, err)
		}
		if picked.ID != "kiro-auth-a" {
			t.Fatalf("pickNext #%d auth = %q, want %q", i, picked.ID, "kiro-auth-a")
		}
	}

	result := Result{
		AuthID:   "kiro-auth-a",
		Provider: "kiro",
		Model:    "claude-sonnet-4.5",
		Success:  false,
	}
	applyResultError(&result, &authScopedTestErr{code: 429})
	if !result.AuthScoped {
		t.Fatal("expected auth-scoped 429 to mark Result.AuthScoped")
	}
	mgr.MarkResult(context.Background(), result)

	picked, _, err := mgr.pickNext(context.Background(), "kiro", "", opts, nil)
	if err != nil {
		t.Fatalf("pickNext after 429: %v", err)
	}
	if picked.ID != "kiro-auth-b" {
		t.Fatalf("pickNext after 429 auth = %q, want %q", picked.ID, "kiro-auth-b")
	}
}
