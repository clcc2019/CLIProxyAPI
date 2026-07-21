package main

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestFindCodexAuthIncludesAgentIdentityWithoutTokens(t *testing.T) {
	agent := &coreauth.Auth{
		ID:       "agent.json",
		Provider: "codex",
		Metadata: map[string]any{
			"auth_kind":         "agent_identity",
			"agent_runtime_id":  "runtime-test",
			"agent_private_key": "private-key-test",
		},
	}

	if got := findCodexAuth([]*coreauth.Auth{agent}); got != agent {
		t.Fatalf("findCodexAuth() = %#v, want Agent Identity auth", got)
	}
}

func TestEnsureAccessTokenDoesNotRequireTokenForAgentIdentity(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind": "agent_identity",
		},
		Metadata: map[string]any{
			"agent_runtime_id":  "runtime-test",
			"agent_private_key": "private-key-test",
			// Agent Identity must win even if an imported record retained stale
			// OAuth material; the runtime executor will generate the assertion.
			"access_token":  "stale-access-token",
			"refresh_token": "stale-refresh-token",
		},
	}

	token, refreshed, err := ensureAccessToken(context.Background(), nil, auth)
	if err != nil {
		t.Fatalf("ensureAccessToken: %v", err)
	}
	if token != "" || refreshed {
		t.Fatalf("token/refreshed = %q/%v, want empty/false", token, refreshed)
	}
}
