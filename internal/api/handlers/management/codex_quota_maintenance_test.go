package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRandomCodexQuotaRefreshIntervalStaysWithinTenToTwentyMinutes(t *testing.T) {
	for index := 0; index < 200; index++ {
		interval := randomCodexQuotaRefreshInterval()
		if interval < codexQuotaRefreshMinInterval || interval > codexQuotaRefreshMaxInterval {
			t.Fatalf("interval = %s, want [%s, %s]", interval, codexQuotaRefreshMinInterval, codexQuotaRefreshMaxInterval)
		}
	}
}

func TestCodexUsageNeedsFiveHourPrime(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	payload := func(used float64, windowSeconds int64, resetAfter time.Duration) gin.H {
		return gin.H{"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent":         used,
				"limit_window_seconds": windowSeconds,
				"reset_at":             now.Add(resetAfter).Unix(),
			},
		}}
	}

	tests := []struct {
		name    string
		payload gin.H
		want    bool
	}{
		{name: "pristine five hour window", payload: payload(0, 18_000, 5*time.Hour), want: true},
		{name: "ninety nine percent remaining", payload: payload(1, 18_000, 5*time.Hour), want: true},
		{name: "below ninety nine percent remaining", payload: payload(1.01, 18_000, 5*time.Hour)},
		{name: "different reset horizon", payload: payload(0, 18_000, 4*time.Hour)},
		{name: "weekly window", payload: payload(0, 7*24*60*60, 5*time.Hour)},
		{name: "missing rate limit", payload: gin.H{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := codexUsageNeedsFiveHourPrime(test.payload, now)
			if got != test.want {
				t.Fatalf("codexUsageNeedsFiveHourPrime() = %v, want %v", got, test.want)
			}
		})
	}
}

type quotaPrimeExecutor struct {
	calls atomic.Int32
}

func (e *quotaPrimeExecutor) Identifier() string { return "codex" }

func (e *quotaPrimeExecutor) Execute(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls.Add(1)
	return coreexecutor.Response{Payload: []byte(`{"id":"response-prime","status":"completed"}`)}, nil
}

func (e *quotaPrimeExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *quotaPrimeExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *quotaPrimeExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *quotaPrimeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestMaintainCodexQuotaPrimesOncePerFiveHourWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().Truncate(time.Second)
	var usageCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		usageCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":` +
			formatInt64(now.Add(5*time.Hour).Unix()) + `},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":` +
			formatInt64(now.Add(7*24*time.Hour).Unix()) + `}}}`))
	}))
	defer server.Close()
	originalUsageURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalUsageURL })

	const authID = "quota-prime-auth"
	const model = "gpt-5.4-mini"
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	executor := &quotaPrimeExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "account-1",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	t.Cleanup(handler.Close)
	handler.maintainCodexQuotaForAuth(context.Background(), mustAuthByID(t, manager, authID), now)
	handler.maintainCodexQuotaForAuth(context.Background(), mustAuthByID(t, manager, authID), now.Add(time.Minute))

	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("quota prime Execute calls = %d, want 1", got)
	}
	if got := usageCalls.Load(); got != 3 {
		t.Fatalf("usage calls = %d, want 3 (poll, post-prime refresh, next poll)", got)
	}
	updated := mustAuthByID(t, manager, authID)
	snapshot, ok := updated.RateLimits["codex"]
	if !ok || snapshot.Primary == nil {
		t.Fatalf("rate limit snapshot missing: %#v", updated.RateLimits)
	}
	if snapshot.Primary.UsedPercent != 1 || snapshot.Primary.WindowMinutes == nil || *snapshot.Primary.WindowMinutes != 300 {
		t.Fatalf("primary window = %#v, want 99%% remaining 300-minute window", snapshot.Primary)
	}
}

func mustAuthByID(t *testing.T, manager *coreauth.Manager, authID string) *coreauth.Auth {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %q not found", authID)
	}
	return auth
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
