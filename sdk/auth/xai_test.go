package auth

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestXAIAuthenticatorProviderAndRefreshLead(t *testing.T) {
	authenticator := NewXAIAuthenticator()
	if authenticator.Provider() != "xai" {
		t.Fatalf("Provider() = %q, want xai", authenticator.Provider())
	}
	lead := authenticator.RefreshLead()
	if lead == nil || *lead <= 0 {
		t.Fatalf("RefreshLead() = %v, want positive duration", lead)
	}
}

func TestXAIRefreshLeadIsRegistered(t *testing.T) {
	lead := cliproxyauth.ProviderRefreshLead("xai", nil)
	if lead == nil || *lead <= 0 {
		t.Fatalf("ProviderRefreshLead(xai) = %v, want positive duration", lead)
	}
}

func TestParseXAIManualCallbackTokenAcceptsRawCode(t *testing.T) {
	result, ok, err := parseXAIManualCallbackToken(" V0auoESADonzF4bY_Ag2whBFnVeqzHJm6nW2uW012rqCCW5cstFV58qvDFBvnPBXXe0rZSKOcs3PwwfACKp1qg ", "state-1")
	if err != nil {
		t.Fatalf("parseXAIManualCallbackToken() error = %v", err)
	}
	if !ok {
		t.Fatal("parseXAIManualCallbackToken() ok = false, want true")
	}
	if result.Code != "V0auoESADonzF4bY_Ag2whBFnVeqzHJm6nW2uW012rqCCW5cstFV58qvDFBvnPBXXe0rZSKOcs3PwwfACKp1qg" {
		t.Fatalf("Code = %q", result.Code)
	}
	if result.State != "state-1" {
		t.Fatalf("State = %q, want state-1", result.State)
	}
}

func TestParseXAIManualCallbackTokenRejectsCallbackURL(t *testing.T) {
	_, _, err := parseXAIManualCallbackToken("http://127.0.0.1:56121/callback?state=state-1&code=token-1", "state-1")
	if err == nil {
		t.Fatal("parseXAIManualCallbackToken() error = nil, want error")
	}
}
