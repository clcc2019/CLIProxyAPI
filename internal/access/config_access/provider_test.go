package configaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAuthenticateIncludesClientAPIKeyQuotaMetadata(t *testing.T) {
	quota := internalconfig.ClientAPIKeyQuota{
		DailyCost:   1.5,
		MonthlyCost: 30,
		TotalCost:   100,
	}
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey: "quota-key",
		Quota:  quota,
	}})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Authorization", "bEaReR quota-key")

	result, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("authenticate failed: %v", authErr)
	}
	if result == nil {
		t.Fatal("expected auth result")
	}
	got := internalconfig.ClientAPIKeyQuotaFromMetadata(result.Metadata)
	if !reflect.DeepEqual(got, quota) {
		t.Fatalf("quota metadata = %#v, want %#v", got, quota)
	}
}

func TestAuthenticateIncludesBoundAuthFiles(t *testing.T) {
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey:    "bound-key",
		AuthFiles: []string{"codex-a.json", "codex-b.json"},
	}})
	req := httptest.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	req.Header.Set("Authorization", "Bearer bound-key")
	result, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil || result == nil {
		t.Fatalf("authenticate failed: result=%#v err=%v", result, authErr)
	}
	var files []string
	if err := json.Unmarshal([]byte(result.Metadata[coreexecutor.ClientAuthFilesMetadataKey]), &files); err != nil {
		t.Fatalf("decode auth files metadata: %v", err)
	}
	if !reflect.DeepEqual(files, []string{"codex-a.json", "codex-b.json"}) {
		t.Fatalf("auth files metadata = %#v", files)
	}
}

func TestAuthenticateIncludesDisableModelAliasMetadata(t *testing.T) {
	provider := newProvider("test", internalconfig.ClientAPIKeys{{APIKey: "raw-key", DisableModelAlias: true}})
	req := httptest.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	req.Header.Set("Authorization", "Bearer raw-key")
	result, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil || result == nil {
		t.Fatalf("authenticate failed: result=%#v err=%v", result, authErr)
	}
	if result.Metadata[coreexecutor.DisableModelAliasMetadataKey] != "true" {
		t.Fatalf("disable model alias metadata = %#v", result.Metadata)
	}
}

func BenchmarkExtractBearerTokenMixedCase(b *testing.B) {
	for b.Loop() {
		if got := extractBearerToken("bEaReR quota-key"); got != "quota-key" {
			b.Fatalf("extractBearerToken() = %q", got)
		}
	}
}

func TestAuthenticateRejectsDisabledClientAPIKey(t *testing.T) {
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey:   "disabled-key",
		Disabled: true,
	}})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer disabled-key")

	result, authErr := provider.Authenticate(context.Background(), req)
	if result != nil {
		t.Fatalf("expected no auth result, got %#v", result)
	}
	if authErr == nil {
		t.Fatal("expected auth error")
	}
	if authErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", authErr.StatusCode, http.StatusTooManyRequests)
	}
	if authErr.Message != "API key disabled" {
		t.Fatalf("message = %q, want %q", authErr.Message, "API key disabled")
	}
}

func TestAuthenticateFallsBackFromInvalidHeaderToQueryKey(t *testing.T) {
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey: "query-key",
	}})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/v1/models?key=query-key", nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer wrong-key")

	result, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("authenticate failed: %v", authErr)
	}
	if result == nil || result.Principal != "query-key" || result.Metadata["source"] != "query-key" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestAuthenticateUnrelatedQueryIsNoCredentials(t *testing.T) {
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey: "valid-key",
	}})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/v1/models?foo=bar", nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}

	result, authErr := provider.Authenticate(context.Background(), req)
	if result != nil {
		t.Fatalf("expected no result, got %#v", result)
	}
	if authErr == nil {
		t.Fatal("expected auth error")
	}
	if authErr.StatusCode != http.StatusUnauthorized || authErr.Message != "Missing API key" {
		t.Fatalf("unexpected auth error: %#v", authErr)
	}
}

func BenchmarkAuthenticateAuthorizationHeader(b *testing.B) {
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey: "valid-key",
	}})
	req, err := http.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	if err != nil {
		b.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer valid-key")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, authErr := provider.Authenticate(context.Background(), req)
		if authErr != nil || result == nil {
			b.Fatalf("authenticate failed: result=%#v err=%v", result, authErr)
		}
	}
}

func BenchmarkAuthenticateConfiguredKey(b *testing.B) {
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey:         "configured-key",
		AllowedModels:  []string{"gpt-5", "gpt-5-mini"},
		ExcludedModels: []string{"gpt-4", "gpt-4-mini"},
		AuthFiles:      []string{"codex-a.json", "codex-b.json"},
		Quota: internalconfig.ClientAPIKeyQuota{
			DailyCost: 1.5, MonthlyCost: 30, TotalCost: 100,
			DailyTokens: 10000, DailyRequests: 100,
		},
	}})
	req := httptest.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	req.Header.Set("Authorization", "Bearer configured-key")
	b.ReportAllocs()
	for b.Loop() {
		result, authErr := provider.Authenticate(context.Background(), req)
		if authErr != nil || result == nil {
			b.Fatalf("authenticate failed: result=%#v err=%v", result, authErr)
		}
	}
}

func TestAuthenticateMetadataIsIsolated(t *testing.T) {
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey:         "valid-key",
		AllowedModels:  []string{"gpt-5", "gpt-5-mini"},
		ExcludedModels: []string{"gpt-4"},
		AuthFiles:      []string{"codex-a.json"},
		Quota:          internalconfig.ClientAPIKeyQuota{DailyRequests: 100},
	}})
	req := httptest.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	req.Header.Set("Authorization", "Bearer valid-key")
	first, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil || first == nil {
		t.Fatalf("authenticate failed: result=%#v err=%v", first, authErr)
	}
	for key := range first.Metadata {
		first.Metadata[key] = "changed"
	}
	first.Metadata["extra"] = "request-local"
	req.Header.Del("Authorization")
	req.Header.Set("X-Api-Key", "valid-key")
	second, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil || second == nil {
		t.Fatalf("authenticate failed: result=%#v err=%v", second, authErr)
	}
	want := map[string]string{
		"source": "x-api-key", "allowed_models": "gpt-5,gpt-5-mini",
		"excluded_models": "gpt-4", "quota_daily_requests": "100",
		coreexecutor.ClientAuthFilesMetadataKey: `["codex-a.json"]`,
	}
	if !reflect.DeepEqual(second.Metadata, want) {
		t.Fatalf("metadata = %#v, want %#v", second.Metadata, want)
	}
}
