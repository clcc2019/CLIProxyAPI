package auth

import (
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexAutoRefreshIsRegistered(t *testing.T) {
	if !cliproxyauth.ProviderDefaultAutoRefresh("codex") {
		t.Fatal("ProviderDefaultAutoRefresh(codex) = false, want true")
	}
	expiryLead, fallbackInterval, ok := cliproxyauth.ProviderDefaultRefreshTiming("codex")
	if !ok || expiryLead != 5*time.Minute || fallbackInterval != 8*24*time.Hour {
		t.Fatalf("ProviderDefaultRefreshTiming(codex) = (%s, %s, %t)", expiryLead, fallbackInterval, ok)
	}
}

func TestKimiRefreshLeadIsRegistered(t *testing.T) {
	lead := cliproxyauth.ProviderRefreshLead("kimi", nil)
	if lead == nil {
		t.Fatal("ProviderRefreshLead(kimi) = nil, want 5 minutes")
	}
	if got, want := *lead, 5*time.Minute; got != want {
		t.Fatalf("ProviderRefreshLead(kimi) = %s, want %s", got, want)
	}
}
