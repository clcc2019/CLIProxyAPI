package executor

import (
	"context"
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestApplyCodexHeadersOmitsEmptyAuthorizationToken(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer stale")

	applyCodexHeaders(req, nil, "  ", true, nil)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
}

func TestApplyCodexHeadersAllowsCustomAuthorizationWithoutToken(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"header:Authorization": "Bearer custom",
	}}

	applyCodexHeaders(req, auth, "", true, nil)

	if got := req.Header.Get("Authorization"); got != "Bearer custom" {
		t.Fatalf("Authorization = %q, want custom bearer", got)
	}
}

func TestApplyCodexHeadersUsesAuthFileClientProfileAttributes(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := cliproxyauth.NewAuthFromAuthFileMetadata(map[string]any{
		"type":                   "codex",
		"access_token":           "oauth-token",
		"originator":             "codex_vscode",
		"beta_features":          "feature-a,feature-b",
		"installation_id":        "install-1",
		"include_timing_metrics": true,
	}, cliproxyauth.AuthFileProjectionOptions{ID: "codex.json"})

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	for header, want := range map[string]string{
		"Originator":                            "codex_vscode",
		"X-Codex-Beta-Features":                 "feature-a,feature-b",
		"X-Codex-Installation-Id":               "install-1",
		"x-responsesapi-include-timing-metrics": "true",
	} {
		if got := req.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q; headers=%#v", header, got, want, req.Header)
		}
	}
}

func TestApplyCodexHeadersSetsFedrampForOAuthAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "oauth-token",
			"account_id":   "account-1",
			"fedramp":      true,
		},
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get(codexHeaderOpenAIFedramp); got != "true" {
		t.Fatalf("%s = %q, want true", codexHeaderOpenAIFedramp, got)
	}
}

func TestApplyCodexHeadersDoesNotSetFedrampForAPIKeyAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"fedramp": true,
		},
		Attributes: map[string]string{
			"api_key":   "sk-test",
			"auth_kind": "api_key",
		},
	}

	applyCodexHeaders(req, auth, "sk-test", true, nil)

	if got := req.Header.Get(codexHeaderOpenAIFedramp); got != "" {
		t.Fatalf("%s = %q, want empty for API key auth", codexHeaderOpenAIFedramp, got)
	}
}

func TestApplyCodexHeadersPinsFirstClientProfileToAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "oauth-token",
		},
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"path":      "/tmp/codex-auth.json",
		},
	}
	var published *cliproxyauth.Auth
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":              "first-codex/1.0",
		"Originator":              "codex_vscode",
		"X-Codex-Beta-Features":   "first-feature",
		"Version":                 "1.2.3",
		"X-Codex-Installation-Id": "first-install",
		"X-OpenAI-Subagent":       "first-subagent",
		codexHeaderOAIAttestation: "first-attestation",
		"Traceparent":             "00-first",
		"Tracestate":              "state-first",
	})
	ctx = cliproxyauth.WithAuthUpdateCallback(ctx, func(_ context.Context, updated *cliproxyauth.Auth) {
		published = updated.Clone()
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if published == nil {
		t.Fatal("expected first request client profile to be published")
	}
	if got := auth.Metadata["user_agent"]; got != "first-codex/1.0" {
		t.Fatalf("auth user_agent = %v, want first-codex/1.0", got)
	}
	if got := auth.Metadata["originator"]; got != "codex_vscode" {
		t.Fatalf("auth originator = %v, want codex_vscode", got)
	}
	if got := auth.Metadata[codexClientProfilePinnedMetadataKey]; got != true {
		t.Fatalf("auth profile pinned = %v, want true", got)
	}
	headers, ok := auth.Metadata["headers"].(map[string]any)
	if !ok {
		t.Fatalf("auth metadata headers = %T, want map[string]any", auth.Metadata["headers"])
	}
	for key, want := range map[string]string{
		"X-Codex-Beta-Features":   "first-feature",
		"Version":                 "1.2.3",
		"X-Codex-Installation-Id": "first-install",
		"X-OpenAI-Subagent":       "first-subagent",
		codexHeaderOAIAttestation: "first-attestation",
		"Traceparent":             "00-first",
		"Tracestate":              "state-first",
	} {
		if got := headers[key]; got != want {
			t.Fatalf("auth metadata header %s = %v, want %s", key, got, want)
		}
	}
	if got := published.Metadata["user_agent"]; got != "first-codex/1.0" {
		t.Fatalf("published user_agent = %v, want first-codex/1.0", got)
	}
	if got := req.Header.Get("User-Agent"); got != "first-codex/1.0" {
		t.Fatalf("request User-Agent = %q, want first-codex/1.0", got)
	}
	if got := req.Header.Get("Originator"); got != "codex_vscode" {
		t.Fatalf("request Originator = %q, want codex_vscode", got)
	}
	if got := req.Header.Get("X-Codex-Beta-Features"); got != "first-feature" {
		t.Fatalf("request X-Codex-Beta-Features = %q, want first-feature", got)
	}
	if got := req.Header.Get(codexHeaderInstallationID); got != "first-install" {
		t.Fatalf("request %s = %q, want first-install", codexHeaderInstallationID, got)
	}
	if got := req.Header.Get("X-OpenAI-Subagent"); got != "first-subagent" {
		t.Fatalf("request X-OpenAI-Subagent = %q, want first-subagent", got)
	}

	published = nil
	secondCtx := contextWithGinHeaders(map[string]string{
		"User-Agent":              "second-codex/2.0",
		"Originator":              "codex_desktop",
		"X-Codex-Beta-Features":   "second-feature",
		"Version":                 "9.9.9",
		"X-Codex-Installation-Id": "second-install",
		"X-OpenAI-Subagent":       "second-subagent",
		codexHeaderOAIAttestation: "second-attestation",
		"Traceparent":             "00-second",
		"Tracestate":              "state-second",
	})
	secondCtx = cliproxyauth.WithAuthUpdateCallback(secondCtx, func(_ context.Context, updated *cliproxyauth.Auth) {
		published = updated.Clone()
	})
	secondReq, err := http.NewRequestWithContext(secondCtx, http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("second NewRequestWithContext() error = %v", err)
	}

	applyCodexHeaders(secondReq, auth, "oauth-token", true, nil)

	if published != nil {
		t.Fatalf("second request should not publish another pin, got %#v", published.Metadata)
	}
	if got := secondReq.Header.Get("User-Agent"); got != "first-codex/1.0" {
		t.Fatalf("second request User-Agent = %q, want first-codex/1.0", got)
	}
	if got := secondReq.Header.Get("Originator"); got != "codex_vscode" {
		t.Fatalf("second request Originator = %q, want codex_vscode", got)
	}
	if got := secondReq.Header.Get("X-Codex-Beta-Features"); got != "first-feature" {
		t.Fatalf("second request X-Codex-Beta-Features = %q, want first-feature", got)
	}
	if got := secondReq.Header.Get("Version"); got != "1.2.3" {
		t.Fatalf("second request Version = %q, want 1.2.3", got)
	}
	if got := secondReq.Header.Get(codexHeaderInstallationID); got != "first-install" {
		t.Fatalf("second request %s = %q, want first-install", codexHeaderInstallationID, got)
	}
	if got := secondReq.Header.Get("X-OpenAI-Subagent"); got != "first-subagent" {
		t.Fatalf("second request X-OpenAI-Subagent = %q, want first-subagent", got)
	}
	if got := secondReq.Header.Get(codexHeaderOAIAttestation); got != "first-attestation" {
		t.Fatalf("second request %s = %q, want first-attestation", codexHeaderOAIAttestation, got)
	}
	if got := secondReq.Header.Get("Traceparent"); got != "00-first" {
		t.Fatalf("second request Traceparent = %q, want 00-first", got)
	}
	if got := secondReq.Header.Get("Tracestate"); got != "state-first" {
		t.Fatalf("second request Tracestate = %q, want state-first", got)
	}
}
