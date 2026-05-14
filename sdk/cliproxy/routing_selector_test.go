package cliproxy

import (
	"context"
	"fmt"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestConfiguredCredentialSelectorSessionAffinityUsesRoundRobinFallback(t *testing.T) {
	t.Parallel()

	selector := configuredCredentialSelector("fill-first", true, "1h")
	if stoppable, ok := selector.(interface{ Stop() }); ok {
		defer stoppable.Stop()
	}

	auths := []*coreauth.Auth{
		{ID: "auth-a", Attributes: map[string]string{"priority": "10"}},
		{ID: "auth-b", Attributes: map[string]string{"priority": "10"}},
		{ID: "auth-c", Attributes: map[string]string{"priority": "0"}},
	}

	for i, want := range []string{"auth-a", "auth-b", "auth-a", "auth-b"} {
		payload := []byte(fmt.Sprintf(`{"metadata":{"user_id":"user_xxx_account__session_%08d-0000-0000-0000-000000000000"}}`, i))
		got, err := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{OriginalRequest: payload}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil || got.ID != want {
			t.Fatalf("Pick() #%d auth = %v, want %s", i, got, want)
		}
	}
}

func TestConfiguredCredentialSelectorFillFirstWithoutSessionAffinity(t *testing.T) {
	t.Parallel()

	selector := configuredCredentialSelector("fill-first", false, "")
	auths := []*coreauth.Auth{{ID: "auth-b"}, {ID: "auth-a"}}

	for i := 0; i < 3; i++ {
		got, err := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil || got.ID != "auth-a" {
			t.Fatalf("Pick() #%d auth = %v, want auth-a", i, got)
		}
	}
}

func TestEffectiveRoutingStrategySessionAffinityNormalizesToRoundRobin(t *testing.T) {
	t.Parallel()

	if got := effectiveRoutingStrategy("fill-first", true); got != "round-robin" {
		t.Fatalf("effectiveRoutingStrategy(fill-first, true) = %q, want round-robin", got)
	}
	if got := effectiveRoutingStrategy("fill-first", false); got != "fill-first" {
		t.Fatalf("effectiveRoutingStrategy(fill-first, false) = %q, want fill-first", got)
	}
}
