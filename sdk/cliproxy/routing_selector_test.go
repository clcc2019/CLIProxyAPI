package cliproxy

import (
	"context"
	"fmt"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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

func TestConfiguredCredentialSelectorWeightedRoundRobin(t *testing.T) {
	t.Parallel()

	selector := configuredCredentialSelector("wrr", false, "")
	auths := []*coreauth.Auth{
		{ID: "auth-a", Attributes: map[string]string{coreauth.AttributeWeight: "5"}},
		{ID: "auth-b", Attributes: map[string]string{coreauth.AttributeWeight: "1"}},
	}
	counts := make(map[string]int)
	for i := 0; i < 6; i++ {
		got, err := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		counts[got.ID]++
	}
	if counts["auth-a"] != 5 || counts["auth-b"] != 1 {
		t.Fatalf("weighted picks = %#v, want auth-a:auth-b = 5:1", counts)
	}
}

func TestConfiguredCredentialSelectorSessionAffinityUsesWeightedFallback(t *testing.T) {
	t.Parallel()

	selector := configuredCredentialSelector("weighted-round-robin", true, "1h")
	if stoppable, ok := selector.(interface{ Stop() }); ok {
		defer stoppable.Stop()
	}
	auths := []*coreauth.Auth{
		{ID: "auth-a", Attributes: map[string]string{coreauth.AttributeWeight: "5"}},
		{ID: "auth-b", Attributes: map[string]string{coreauth.AttributeWeight: "1"}},
	}
	counts := make(map[string]int)
	for i := 0; i < 6; i++ {
		payload := []byte(fmt.Sprintf(`{"metadata":{"user_id":"user_xxx_account__session_%08d-0000-0000-0000-000000000000"}}`, i))
		got, err := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{OriginalRequest: payload}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		counts[got.ID]++
	}
	if counts["auth-a"] != 5 || counts["auth-b"] != 1 {
		t.Fatalf("weighted session picks = %#v, want auth-a:auth-b = 5:1", counts)
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
	if got := effectiveRoutingStrategy("weighted-round-robin", true); got != "weighted-round-robin" {
		t.Fatalf("effectiveRoutingStrategy(weighted-round-robin, true) = %q, want weighted-round-robin", got)
	}
}

func TestIsWeightedRoundRobinStrategyAliases(t *testing.T) {
	t.Parallel()

	for _, strategy := range []string{"weighted-round-robin", " WeightedRoundRobin ", "weighted", "WRR"} {
		if !isWeightedRoundRobinStrategy(strategy) {
			t.Fatalf("isWeightedRoundRobinStrategy(%q) = false, want true", strategy)
		}
	}
	if isWeightedRoundRobinStrategy("round-robin") {
		t.Fatal("round-robin must not match weighted-round-robin")
	}
}

func TestIsFillFirstStrategyAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		strategy string
		want     bool
	}{
		{name: "canonical", strategy: "fill-first", want: true},
		{name: "canonical mixed case", strategy: " Fill-First ", want: true},
		{name: "compact", strategy: "fillfirst", want: true},
		{name: "compact mixed case", strategy: "\tFillFirst\r\n", want: true},
		{name: "short", strategy: "FF", want: true},
		{name: "round robin", strategy: "round-robin", want: false},
		{name: "empty", strategy: " ", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFillFirstStrategy(tt.strategy); got != tt.want {
				t.Fatalf("isFillFirstStrategy(%q) = %t, want %t", tt.strategy, got, tt.want)
			}
		})
	}
}

func BenchmarkIsFillFirstStrategy(b *testing.B) {
	for b.Loop() {
		if !isFillFirstStrategy(" Fill-First ") {
			b.Fatal("expected fill-first strategy")
		}
	}
}
