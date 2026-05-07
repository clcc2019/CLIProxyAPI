package configaccess

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestAuthenticateIncludesClientAPIKeyQuotaMetadata(t *testing.T) {
	quota := internalconfig.ClientAPIKeyQuota{
		DailyTokens:     100,
		MonthlyTokens:   200,
		TotalTokens:     300,
		DailyRequests:   10,
		MonthlyRequests: 20,
		TotalRequests:   30,
	}
	provider := newProvider("test", internalconfig.ClientAPIKeys{{
		APIKey: "quota-key",
		Quota:  quota,
	}})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer quota-key")

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
