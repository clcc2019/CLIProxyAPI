package auth

import (
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestKimiRefreshLeadIsRegistered(t *testing.T) {
	lead := cliproxyauth.ProviderRefreshLead("kimi", nil)
	if lead == nil {
		t.Fatal("ProviderRefreshLead(kimi) = nil, want 5 minutes")
	}
	if got, want := *lead, 5*time.Minute; got != want {
		t.Fatalf("ProviderRefreshLead(kimi) = %s, want %s", got, want)
	}
}
