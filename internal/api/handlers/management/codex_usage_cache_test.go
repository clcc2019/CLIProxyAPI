package management

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

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
