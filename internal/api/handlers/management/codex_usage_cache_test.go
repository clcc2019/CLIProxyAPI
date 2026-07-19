package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexUsageRequestRetryLimitUsesConfigAndAuthOverride(t *testing.T) {
	h := &Handler{cfg: &config.Config{RequestRetry: 3}}
	if got := h.codexUsageRequestRetryLimit(nil); got != 3 {
		t.Fatalf("configured retry limit = %d, want 3", got)
	}
	if got := h.codexUsageRequestRetryLimit(&coreauth.Auth{Metadata: map[string]any{"request_retry": 1}}); got != 1 {
		t.Fatalf("auth retry override = %d, want 1", got)
	}
	if got := h.codexUsageRequestRetryLimit(&coreauth.Auth{Metadata: map[string]any{"request-retry": 0}}); got != 0 {
		t.Fatalf("zero auth retry override = %d, want 0", got)
	}
	if got := (&Handler{cfg: &config.Config{}}).codexUsageRequestRetryLimit(nil); got != 0 {
		t.Fatalf("explicit zero retry limit = %d, want 0", got)
	}
	if got := (*Handler)(nil).codexUsageRequestRetryLimit(nil); got != codexUsageMaxRequestRetries {
		t.Fatalf("nil handler retry limit = %d, want fallback %d", got, codexUsageMaxRequestRetries)
	}
}

func codexUsageOptionsContext(target string) *gin.Context {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	return ctx
}

func TestParseCodexUsageRequestOptions(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		wantForce bool
		wantTTL   time.Duration
	}{
		{name: "default", target: "/v0/management/auth-files/codex-usage", wantTTL: codexUsageCacheDefaultTTL},
		{name: "force mixed case", target: "/v0/management/auth-files/codex-usage?force=YES", wantForce: true, wantTTL: codexUsageCacheDefaultTTL},
		{name: "codex usage refresh mixed case", target: "/v0/management/auth-files/codex-usage?codexUsage=Fetch", wantForce: true, wantTTL: codexUsageCacheDefaultTTL},
		{name: "zero ttl forces refresh", target: "/v0/management/auth-files/codex-usage?ttl=0", wantForce: true, wantTTL: codexUsageCacheDefaultTTL},
		{name: "ttl capped", target: "/v0/management/auth-files/codex-usage?ttl=999", wantTTL: codexUsageCacheMaxTTL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCodexUsageRequestOptions(codexUsageOptionsContext(tt.target))
			if got.force != tt.wantForce {
				t.Fatalf("force = %t, want %t", got.force, tt.wantForce)
			}
			if got.ttl != tt.wantTTL {
				t.Fatalf("ttl = %s, want %s", got.ttl, tt.wantTTL)
			}
		})
	}
}

func TestCodexUsageQueryValueMatchers(t *testing.T) {
	if !isTruthyQueryValue(" On ") {
		t.Fatalf("isTruthyQueryValue(On) = false, want true")
	}
	if isTruthyQueryValue("off") {
		t.Fatalf("isTruthyQueryValue(off) = true, want false")
	}
	if !isRefreshQueryValue("\tRefresh\r\n") {
		t.Fatalf("isRefreshQueryValue(Refresh) = false, want true")
	}
	if isRefreshQueryValue("skip") {
		t.Fatalf("isRefreshQueryValue(skip) = true, want false")
	}
	if !isSkipQueryValue(" NO ") {
		t.Fatalf("isSkipQueryValue(NO) = false, want true")
	}
}

func TestCodexSubscriptionListModeFromRequest(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   codexSubscriptionListMode
	}{
		{name: "default", target: "/v0/management/auth-files", want: codexSubscriptionListCache},
		{name: "refresh mixed case", target: "/v0/management/auth-files?codex_subscription=Fetch", want: codexSubscriptionListRefresh},
		{name: "skip mixed case", target: "/v0/management/auth-files?codexSubscription=OFF", want: codexSubscriptionListSkip},
		{name: "unknown", target: "/v0/management/auth-files?codex_subscription=maybe", want: codexSubscriptionListCache},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexSubscriptionListModeFromRequest(codexUsageOptionsContext(tt.target))
			if got != tt.want {
				t.Fatalf("codexSubscriptionListModeFromRequest() = %d, want %d", got, tt.want)
			}
		})
	}
}

func BenchmarkRefreshQueryValue(b *testing.B) {
	for b.Loop() {
		if !isRefreshQueryValue(" Fetch ") {
			b.Fatal("expected refresh query value")
		}
	}
}

func TestCodexUsageCacheLoadDoesNotMutateEntry(t *testing.T) {
	now := time.Now()
	entry := &codexUsageCacheEntry{
		Payload:   gin.H{"credits": gin.H{"balance": 10}},
		ExpiresAt: now.Add(time.Minute),
	}
	cache := &codexUsageCache{}
	cache.store("usage-key", entry)

	if _, _, ok := cache.load("usage-key", now, false); !ok {
		t.Fatal("expected cache hit")
	}
	if !entry.StaleUntil.IsZero() {
		t.Fatalf("StaleUntil was mutated to %s", entry.StaleUntil)
	}
}

func TestCodexUsageCacheLoadDeepClonesPayload(t *testing.T) {
	now := time.Now()
	entry := &codexUsageCacheEntry{
		Payload: gin.H{
			"credits": gin.H{"balance": 10},
			"history": []any{gin.H{"used": 1}},
			"labels":  []string{"cached"},
		},
		ExpiresAt:  now.Add(time.Minute),
		StaleUntil: now.Add(2 * time.Minute),
	}
	cache := &codexUsageCache{}
	cache.store("usage-key", entry)

	payload, _, ok := cache.load("usage-key", now, false)
	if !ok {
		t.Fatal("expected cache hit")
	}
	payload["credits"].(gin.H)["balance"] = 0
	payload["history"].([]any)[0].(gin.H)["used"] = 99
	payload["labels"].([]string)[0] = "changed"

	if got := entry.Payload["credits"].(gin.H)["balance"]; got != 10 {
		t.Fatalf("cached credits.balance = %#v, want 10", got)
	}
	if got := entry.Payload["history"].([]any)[0].(gin.H)["used"]; got != 1 {
		t.Fatalf("cached history[0].used = %#v, want 1", got)
	}
	if got := entry.Payload["labels"].([]string)[0]; got != "cached" {
		t.Fatalf("cached labels[0] = %#v, want cached", got)
	}
}

func TestFetchCodexUsageWithCacheIsolatesSingleflightPayloads(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var upstreamCalls atomic.Int32
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRequest) }) }
	t.Cleanup(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		startedOnce.Do(func() { close(requestStarted) })
		<-releaseRequest
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit_reset_credits":{"available_count":3},"rate_limit":null}`))
	}))
	t.Cleanup(server.Close)
	originalURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalURL })

	h := &Handler{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "usage-access-token",
			"account_id":   "acct_123",
		},
	}

	const callers = 16
	payloads := make([]gin.H, callers)
	statuses := make([]int, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	for i := range callers {
		go func(index int) {
			defer done.Done()
			ready.Done()
			<-start
			payloads[index], statuses[index], errs[index] = h.fetchCodexUsageWithCache(
				context.Background(),
				auth,
				codexUsageRequestOptions{force: true, ttl: codexUsageCacheDefaultTTL},
			)
		}(i)
	}
	ready.Wait()
	close(start)
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the singleflight leader request")
	}
	// Keep the leader in flight long enough for all released goroutines to join
	// the same request. The call-count assertion below verifies coalescing.
	time.Sleep(100 * time.Millisecond)
	release()
	done.Wait()

	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 singleflight request", got)
	}
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d error = %v", i, errs[i])
		}
		if statuses[i] != http.StatusOK {
			t.Fatalf("caller %d status = %d, want %d", i, statuses[i], http.StatusOK)
		}
	}

	payloads[0]["caller_marker"] = "first"
	firstCredits, ok := codexUsageWindowMap(payloads[0]["rate_limit_reset_credits"])
	if !ok {
		t.Fatalf("first caller credits missing: %#v", payloads[0])
	}
	firstCredits["available_count"] = float64(0)
	for i := 1; i < callers; i++ {
		if marker, exists := payloads[i]["caller_marker"]; exists {
			t.Fatalf("caller %d observed another caller's top-level mutation: %#v", i, marker)
		}
		credits, ok := codexUsageWindowMap(payloads[i]["rate_limit_reset_credits"])
		if !ok {
			t.Fatalf("caller %d credits missing: %#v", i, payloads[i])
		}
		if got := credits["available_count"]; got != float64(3) {
			t.Fatalf("caller %d observed another caller's nested mutation: %#v", i, got)
		}
	}
}

func TestCodexUsageCacheKeyPreservesCaseSensitiveAuthIdentity(t *testing.T) {
	h := &Handler{}
	upper := h.codexUsageCacheKey(&coreauth.Auth{ID: "Auth-A", FileName: "Codex.json"})
	lower := h.codexUsageCacheKey(&coreauth.Auth{ID: "auth-a", FileName: "codex.json"})
	if upper == "" || lower == "" {
		t.Fatalf("cache keys must be non-empty: upper=%q lower=%q", upper, lower)
	}
	if upper == lower {
		t.Fatalf("case-distinct auth identities produced the same cache key: %s", upper)
	}
}

func TestCodexUsageCacheKeyUsesOpaqueTokenFingerprintWhenAccountMissing(t *testing.T) {
	h := &Handler{}
	first := h.codexUsageCacheKey(&coreauth.Auth{
		ID:       "same-auth",
		FileName: "codex.json",
		Metadata: map[string]any{"access_token": "opaque-token-1"},
	})
	second := h.codexUsageCacheKey(&coreauth.Auth{
		ID:       "same-auth",
		FileName: "codex.json",
		Metadata: map[string]any{"access_token": "opaque-token-2"},
	})
	if first == "" || second == "" {
		t.Fatalf("cache keys must be non-empty: first=%q second=%q", first, second)
	}
	if first == second {
		t.Fatalf("opaque tokens without account ids produced the same cache key: %s", first)
	}
}

func TestCodexUsageCacheKeyIncludesUsageUserAgentOnly(t *testing.T) {
	h := &Handler{}
	base := &coreauth.Auth{
		ID:       "same-auth",
		FileName: "codex.json",
		Metadata: map[string]any{
			"access_token":            "opaque-token",
			"account_id":              "acct_123",
			"user_agent":              "codex-profile/1.0",
			"originator":              "codex_cli",
			"beta_features":           "feature-a",
			"installation_id":         "install-1",
			"include_timing_metrics":  true,
			"header:Originator":       "codex_vscode",
			"X-Codex-Installation-Id": "install-2",
			"X-Codex-Beta-Features":   "feature-b",
			"include-timing-metrics":  "false",
		},
	}
	changedProfile := base.Clone()
	changedProfile.Metadata["originator"] = "codex_vscode"
	changedProfile.Metadata["beta_features"] = "feature-b"
	changedProfile.Metadata["installation_id"] = "install-2"
	changedProfile.Metadata["include_timing_metrics"] = false

	first := h.codexUsageCacheKey(base)
	second := h.codexUsageCacheKey(changedProfile)
	if first == "" || second == "" {
		t.Fatalf("cache keys must be non-empty: first=%q second=%q", first, second)
	}
	if first != second {
		t.Fatalf("non-official usage profile fields changed cache key: first=%s second=%s", first, second)
	}

	changedUserAgent := base.Clone()
	changedUserAgent.Metadata["user_agent"] = "codex-profile/2.0"
	third := h.codexUsageCacheKey(changedUserAgent)
	if third == "" {
		t.Fatal("cache key must be non-empty")
	}
	if third == first {
		t.Fatalf("usage User-Agent did not change cache key: %s", first)
	}
}
