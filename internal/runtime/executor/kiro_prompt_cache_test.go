package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func mustKiroPromptCacheClaudeRequest(t *testing.T, cacheText string) []byte {
	t.Helper()
	payload := map[string]any{
		"model": "claude-sonnet-4.5",
		"system": []any{
			map[string]any{
				"type": "text",
				"text": cacheText,
				"cache_control": map[string]any{
					"type": "ephemeral",
				},
			},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "Use the cached project context."},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return raw
}

func TestKiroPromptCacheTrackerCreationThenRead(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := newKiroPromptCacheTracker(func() time.Time { return now })
	body := mustKiroPromptCacheClaudeRequest(t, strings.Repeat("cacheable project context ", 900))

	first := tracker.plan("auth-a", sdktranslator.FormatClaude, body, 0, "claude-sonnet-4.5")
	if first == nil {
		t.Fatalf("expected cache plan")
	}
	if first.usage.cacheCreationInputTokens <= 0 {
		t.Fatalf("first request should create cache tokens: %+v", first.usage)
	}
	if first.usage.cacheReadInputTokens != 0 {
		t.Fatalf("first request should not hit cache: %+v", first.usage)
	}
	markKiroPromptCachePlanSuccess(first)

	second := tracker.plan("auth-a", sdktranslator.FormatClaude, body, 0, "claude-sonnet-4.5")
	if second == nil {
		t.Fatalf("expected second cache plan")
	}
	if second.usage.cacheReadInputTokens <= 0 {
		t.Fatalf("second request should read cache: %+v", second.usage)
	}
	if second.usage.cacheCreationInputTokens != 0 {
		t.Fatalf("full cache hit should not create more tokens: %+v", second.usage)
	}
}

func TestKiroPromptCacheTrackerIsScopedByAuth(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := newKiroPromptCacheTracker(func() time.Time { return now })
	body := mustKiroPromptCacheClaudeRequest(t, strings.Repeat("cacheable project context ", 900))

	first := tracker.plan("auth-a", sdktranslator.FormatClaude, body, 0, "claude-sonnet-4.5")
	if first == nil || first.usage.cacheCreationInputTokens <= 0 {
		t.Fatalf("expected initial cache creation, got %+v", first)
	}
	markKiroPromptCachePlanSuccess(first)

	otherAuth := tracker.plan("auth-b", sdktranslator.FormatClaude, body, 0, "claude-sonnet-4.5")
	if otherAuth == nil {
		t.Fatalf("expected cache plan for other auth")
	}
	if otherAuth.usage.cacheReadInputTokens != 0 {
		t.Fatalf("different auth must not hit cache: %+v", otherAuth.usage)
	}
	if otherAuth.usage.cacheCreationInputTokens <= 0 {
		t.Fatalf("different auth should create its own cache: %+v", otherAuth.usage)
	}
}

func TestKiroPromptCacheTrackerOpusMinimum(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := newKiroPromptCacheTracker(func() time.Time { return now })
	body := mustKiroPromptCacheClaudeRequest(t, strings.Repeat("small cacheable context ", 300))

	first := tracker.plan("auth-a", sdktranslator.FormatClaude, body, 0, "claude-opus-4.1")
	if first == nil {
		t.Fatalf("expected cache plan")
	}
	if first.profile.breakpoints[len(first.profile.breakpoints)-1].cumulativeTokens >= kiroPromptCacheOpusMinTokens {
		t.Fatalf("test fixture unexpectedly exceeds opus threshold: %+v", first.profile.breakpoints[len(first.profile.breakpoints)-1])
	}
	if first.usage.cacheCreationInputTokens != 0 {
		t.Fatalf("opus requests below 4096 tokens should not create cache: %+v", first.usage)
	}
	markKiroPromptCachePlanSuccess(first)

	second := tracker.plan("auth-a", sdktranslator.FormatClaude, body, 0, "claude-opus-4.1")
	if second == nil {
		t.Fatalf("expected second cache plan")
	}
	if second.usage.cacheReadInputTokens != 0 {
		t.Fatalf("under-threshold opus cache should not be stored/read: %+v", second.usage)
	}
}

func TestApplyKiroPromptCachePlanPreservesUpstreamCacheFields(t *testing.T) {
	plan := &kiroPromptCachePlan{usage: kiroPromptCacheUsage{cacheReadInputTokens: 100, cacheCreationInputTokens: 50}}
	detail := usage.Detail{InputTokens: 200, OutputTokens: 25, CachedTokens: 20, TotalTokens: 225}

	if !applyKiroPromptCachePlan(&detail, plan) {
		t.Fatalf("expected missing cache creation to be applied")
	}
	if detail.CachedTokens != 20 {
		t.Fatalf("upstream cached tokens should be preserved, got %d", detail.CachedTokens)
	}
	if detail.CacheCreationTokens != 50 {
		t.Fatalf("cache creation should be filled, got %d", detail.CacheCreationTokens)
	}
	if detail.TotalTokens != 225 {
		t.Fatalf("total tokens changed unexpectedly: %+v", detail)
	}
}

func TestBuildKiroPromptCacheProfileRequiresBreakpoint(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hello"}]}`)
	if profile := buildKiroPromptCacheProfile(sdktranslator.FormatClaude, body, 0, "claude-sonnet-4.5"); profile != nil {
		t.Fatalf("request without cache_control should not build profile: %+v", profile)
	}
}
