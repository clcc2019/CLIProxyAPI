package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestStartupAuthGroups(t *testing.T) {
	nonFree := &coreauth.Auth{ID: "plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}
	free := &coreauth.Auth{ID: "free", Provider: "codex", Attributes: map[string]string{"plan_type": "FREE"}}
	disabled := &coreauth.Auth{ID: "disabled", Provider: "codex", Disabled: true, Attributes: map[string]string{"plan_type": "plus"}}
	other := &coreauth.Auth{ID: "claude", Provider: "claude"}
	priority, deferred := startupAuthGroups([]*coreauth.Auth{nil, free, disabled, other, nonFree})

	if len(priority) != 2 || priority[0].ID != "claude" || priority[1].ID != "plus" {
		t.Fatalf("priority = %#v, want claude and plus", priority)
	}
	if len(deferred) != 1 || deferred[0].ID != "free" {
		t.Fatalf("deferred = %#v, want free", deferred)
	}
}
