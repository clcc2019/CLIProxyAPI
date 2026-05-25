package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexAuthServiceReusesProxyBucket(t *testing.T) {
	t.Parallel()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "http://proxy.example.com:8080"}

	first := executor.codexAuthService(auth)
	second := executor.codexAuthService(auth)

	if first != second {
		t.Fatal("expected codex auth service to be reused for the same proxy URL")
	}
}

func TestCodexAuthServiceSeparatesProxyBuckets(t *testing.T) {
	t.Parallel()

	executor := NewCodexExecutor(&config.Config{})

	first := executor.codexAuthService(&cliproxyauth.Auth{ProxyURL: "http://proxy-a.example.com:8080"})
	second := executor.codexAuthService(&cliproxyauth.Auth{ProxyURL: "http://proxy-b.example.com:8080"})

	if first == second {
		t.Fatal("expected distinct codex auth services for different proxy URLs")
	}
}

func TestCodexAuthServiceFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	executor := NewCodexExecutor(&config.Config{
		SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
	})

	first := executor.codexAuthService(&cliproxyauth.Auth{})
	second := executor.codexAuthService(nil)

	if first != second {
		t.Fatal("expected empty auth proxy URLs to share the config proxy bucket")
	}
}

func TestApplyCodexTokenDataToAuthPreservesMissingRefreshFields(t *testing.T) {
	t.Parallel()

	refreshedAt := time.Date(2026, time.May, 22, 10, 30, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":   "old-access-token",
			"plan_type": "plus",
		},
		Metadata: map[string]any{
			"type":                                 "codex",
			"id_token":                             "old-id-token",
			"access_token":                         "old-access-token",
			"refresh_token":                        "old-refresh-token",
			"account_id":                           "old-account",
			"email":                                "old@example.com",
			"plan_type":                            "plus",
			"chatgpt_plan_type":                    "plus",
			"expired":                              "2026-05-23T00:00:00Z",
			"subscription_expires_at":              "2026-06-19T11:44:26Z",
			"chatgpt_subscription_active_until":    "2026-06-19T11:44:26Z",
			"chatgpt_subscription_active_start_at": "2026-05-19T11:44:26Z",
		},
	}

	applyCodexTokenDataToAuth(auth, &codexauth.CodexTokenData{
		AccessToken: "new-access-token",
		AccountID:   "new-account",
		PlanType:    "pro",
		Expire:      "2026-05-24T00:00:00Z",
	}, refreshedAt)

	assertMetadataValue(t, auth.Metadata, "id_token", "old-id-token")
	assertMetadataValue(t, auth.Metadata, "access_token", "new-access-token")
	assertMetadataValue(t, auth.Metadata, "refresh_token", "old-refresh-token")
	assertMetadataValue(t, auth.Metadata, "account_id", "new-account")
	assertMetadataValue(t, auth.Metadata, "email", "old@example.com")
	assertMetadataValue(t, auth.Metadata, "plan_type", "pro")
	assertMetadataValue(t, auth.Metadata, "chatgpt_plan_type", "pro")
	assertMetadataValue(t, auth.Metadata, "expired", "2026-05-24T00:00:00Z")
	assertMetadataValue(t, auth.Metadata, "subscription_expires_at", "2026-06-19T11:44:26Z")
	assertMetadataValue(t, auth.Metadata, "chatgpt_subscription_active_until", "2026-06-19T11:44:26Z")
	assertMetadataValue(t, auth.Metadata, "chatgpt_subscription_active_start_at", "2026-05-19T11:44:26Z")
	assertMetadataValue(t, auth.Metadata, "last_refresh", "2026-05-22T10:30:00Z")
	if got := auth.Attributes["plan_type"]; got != "pro" {
		t.Fatalf("attribute plan_type = %q, want %q", got, "pro")
	}
	if got := auth.Attributes["api_key"]; got != "new-access-token" {
		t.Fatalf("attribute api_key = %q, want mirrored access token to update", got)
	}

	customKeyAuth := &cliproxyauth.Auth{
		Attributes: map[string]string{"api_key": "custom-api-key"},
		Metadata:   map[string]any{"access_token": "old-access-token"},
	}
	applyCodexTokenDataToAuth(customKeyAuth, &codexauth.CodexTokenData{AccessToken: "new-access-token"}, refreshedAt)
	if got := customKeyAuth.Attributes["api_key"]; got != "custom-api-key" {
		t.Fatalf("custom attribute api_key = %q, want custom-api-key", got)
	}
}

func TestCodexExecutorExecuteDoesNotRefreshAfterUnauthorized(t *testing.T) {
	t.Parallel()

	var attempts int
	var authorizationHeaders []string
	rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		authorizationHeaders = append(authorizationHeaders, req.Header.Get("Authorization"))
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"expired token"}}`)),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(codexCompletedAfterOutputItemDoneSSE)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "codex-1",
		Attributes: map[string]string{
			"base_url": "https://codex.test/backend-api/codex",
		},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "refresh-token",
		},
	}

	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"Say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want unauthorized error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if len(authorizationHeaders) != 1 || authorizationHeaders[0] != "Bearer old-access-token" {
		t.Fatalf("authorization headers = %#v, want old bearer token only", authorizationHeaders)
	}
}

func TestCodexExecutorExecuteRefreshesAfterUnauthorizedWithCoordinator(t *testing.T) {
	t.Parallel()

	var attempts int
	var authorizationHeaders []string
	rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		authorizationHeaders = append(authorizationHeaders, req.Header.Get("Authorization"))
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"expired token"}}`)),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(codexCompletedAfterOutputItemDoneSSE)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))

	var refreshCalls int
	ctx = cliproxyauth.WithRefreshCoordinator(ctx, func(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
		refreshCalls++
		refreshed := auth.Clone()
		if refreshed.Metadata == nil {
			refreshed.Metadata = map[string]any{}
		}
		refreshed.Metadata["access_token"] = "new-access-token"
		refreshed.Metadata["refresh_token"] = "new-refresh-token"
		return refreshed, nil
	})

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://codex.test/backend-api/codex",
		},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "refresh-token",
		},
	}

	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"Say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls)
	}
	wantHeaders := []string{"Bearer old-access-token", "Bearer new-access-token"}
	if len(authorizationHeaders) != len(wantHeaders) {
		t.Fatalf("authorization headers = %#v, want %#v", authorizationHeaders, wantHeaders)
	}
	for i := range wantHeaders {
		if authorizationHeaders[i] != wantHeaders[i] {
			t.Fatalf("authorization headers = %#v, want %#v", authorizationHeaders, wantHeaders)
		}
	}
}

func assertMetadataValue(t *testing.T, metadata map[string]any, key string, want any) {
	t.Helper()
	if got := metadata[key]; got != want {
		t.Fatalf("metadata[%q] = %#v, want %#v", key, got, want)
	}
}
