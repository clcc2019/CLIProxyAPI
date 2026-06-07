package configaccess

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
