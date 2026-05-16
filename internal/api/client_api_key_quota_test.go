package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type staticAccessProvider struct {
	result  *sdkaccess.Result
	authErr *sdkaccess.AuthError
}

func (p staticAccessProvider) Identifier() string { return "static" }

func (p staticAccessProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p.authErr != nil {
		return nil, p.authErr
	}
	return p.result, nil
}

func TestAuthMiddlewareRejectsDisabledClientAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{staticAccessProvider{authErr: sdkaccess.NewDisabledCredentialError()}})

	router := gin.New()
	router.Use(AuthMiddleware(manager))
	router.GET("/v1/models", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusTooManyRequests, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"error":"API key disabled"`) {
		t.Fatalf("body = %q, want disabled error", resp.Body.String())
	}
}

func TestAuthMiddlewareRejectsClientAPIKeyQuotaExceeded(t *testing.T) {
	gin.SetMode(gin.TestMode)

	apiKey := "quota-middleware-key"
	now := time.Now().UTC()
	usage.SetClientAPIKeyQuotaModelPrices(config.ModelPrices{
		"gpt-test": {Prompt: 1},
	})
	t.Cleanup(func() {
		usage.SetClientAPIKeyQuotaModelPrices(nil)
	})
	coreusage.PublishRecord(context.Background(), coreusage.Record{
		APIKey:      apiKey,
		RequestedAt: now,
		Model:       "gpt-test",
		Detail:      coreusage.Detail{InputTokens: 1_000_000},
	})
	flushCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := coreusage.FlushDefault(flushCtx); err != nil {
		t.Fatalf("flush usage failed: %v", err)
	}

	metadata := map[string]string{}
	config.AddClientAPIKeyQuotaMetadata(metadata, config.ClientAPIKeyQuota{DailyCost: 1})
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{staticAccessProvider{result: &sdkaccess.Result{
		Provider:  "static",
		Principal: apiKey,
		Metadata:  metadata,
	}}})

	router := gin.New()
	router.Use(AuthMiddleware(manager))
	router.GET("/v1/models", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusTooManyRequests, resp.Body.String())
	}
	if retryAfter := resp.Header().Get("Retry-After"); retryAfter == "" {
		t.Fatal("expected Retry-After header")
	}
	body := resp.Body.String()
	for _, expected := range []string{`"error":"api key quota exceeded"`, `"scope":"daily"`, `"resource":"cost"`, `"currency":"USD"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body %q missing %s", body, expected)
		}
	}
}
