package helps

import (
	"strings"
	"testing"
)

func resetSensitiveWordMatcherCacheForTest() {
	sensitiveWordMatcherCache.Lock()
	sensitiveWordMatcherCache.entries = make(map[string]*SensitiveWordMatcher)
	sensitiveWordMatcherCache.order = nil
	sensitiveWordMatcherCache.Unlock()
}

func TestBuildSensitiveWordMatcher_ReusesCachedMatcher(t *testing.T) {
	resetSensitiveWordMatcherCacheForTest()

	first := BuildSensitiveWordMatcher([]string{"proxy", "gateway", "x"})
	second := BuildSensitiveWordMatcher([]string{"proxy", "gateway", "x"})

	if first == nil {
		t.Fatal("expected matcher")
	}
	if first != second {
		t.Fatal("expected repeated sensitive word config to reuse cached matcher")
	}
	if got := first.obfuscateText("proxy gateway"); !strings.Contains(got, zeroWidthSpace) {
		t.Fatalf("expected obfuscated text, got %q", got)
	}
}

func TestBuildSensitiveWordMatcher_CacheEvictsOldest(t *testing.T) {
	resetSensitiveWordMatcherCacheForTest()

	first := BuildSensitiveWordMatcher([]string{"word-000"})
	for i := 1; i <= sensitiveWordMatcherCacheMaxEntries; i++ {
		BuildSensitiveWordMatcher([]string{"word-" + strings.Repeat("x", i)})
	}

	again := BuildSensitiveWordMatcher([]string{"word-000"})
	if again == nil {
		t.Fatal("expected matcher")
	}
	if again == first {
		t.Fatal("expected oldest matcher to be evicted after cache reaches capacity")
	}
}

func BenchmarkBuildSensitiveWordMatcherCached(b *testing.B) {
	resetSensitiveWordMatcherCacheForTest()
	words := []string{"proxy", "gateway", "claude", "router", "token"}
	BuildSensitiveWordMatcher(words)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if BuildSensitiveWordMatcher(words) == nil {
			b.Fatal("expected matcher")
		}
	}
}
