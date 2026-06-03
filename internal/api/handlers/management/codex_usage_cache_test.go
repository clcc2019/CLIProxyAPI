package management

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

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
