package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestMergeKiroUsageMaxSemantics locks in the fix for the usage-overwrite
// bug: when Kiro emits multiple messageMetadataEvent frames, older builds
// would overwrite `dst` with the newest block even if that block reported
// smaller counts (e.g. a transient zeroed usage mid-stream). The merge must
// use max() on every field so the accumulator only grows.
func TestMergeKiroUsageMaxSemantics(t *testing.T) {
	dst := usage.Detail{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	// A later event reports lower output (e.g. partial vs final bill). The
	// merge must keep the larger value, not clobber with the smaller one.
	mergeKiroUsage(&dst, usage.Detail{InputTokens: 100, OutputTokens: 10, TotalTokens: 110})
	if dst.OutputTokens != 50 {
		t.Fatalf("OutputTokens regressed: want 50, got %d", dst.OutputTokens)
	}
	if dst.InputTokens != 100 {
		t.Fatalf("InputTokens changed: want 100, got %d", dst.InputTokens)
	}
	if dst.TotalTokens != 150 {
		t.Fatalf("TotalTokens regressed: want 150, got %d", dst.TotalTokens)
	}
}

// TestMergeKiroUsageGrowsMonotonically verifies the happy path: a stream of
// three usage blocks with monotonically increasing counts produces the
// final (largest) value.
func TestMergeKiroUsageGrowsMonotonically(t *testing.T) {
	var dst usage.Detail
	mergeKiroUsage(&dst, usage.Detail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15})
	mergeKiroUsage(&dst, usage.Detail{InputTokens: 10, OutputTokens: 25, TotalTokens: 35})
	mergeKiroUsage(&dst, usage.Detail{InputTokens: 10, OutputTokens: 50, TotalTokens: 60})
	if dst.OutputTokens != 50 {
		t.Fatalf("want OutputTokens=50 (final), got %d", dst.OutputTokens)
	}
	if dst.TotalTokens != 60 {
		t.Fatalf("want TotalTokens=60 (final), got %d", dst.TotalTokens)
	}
}

// TestMergeKiroUsageCacheFields asserts the cache buckets follow the same
// max() semantics. These drive Claude's cache_read_input_tokens /
// cache_creation_input_tokens reporting downstream, so correctness matters
// for the "is prompt caching working?" user question.
func TestMergeKiroUsageCacheFields(t *testing.T) {
	dst := usage.Detail{CachedTokens: 200, CacheCreationTokens: 50}
	mergeKiroUsage(&dst, usage.Detail{CachedTokens: 150, CacheCreationTokens: 75, ReasoningTokens: 10})
	if dst.CachedTokens != 200 {
		t.Fatalf("CachedTokens regressed: want 200, got %d", dst.CachedTokens)
	}
	if dst.CacheCreationTokens != 75 {
		t.Fatalf("CacheCreationTokens should grow: want 75, got %d", dst.CacheCreationTokens)
	}
	if dst.ReasoningTokens != 10 {
		t.Fatalf("ReasoningTokens should have been copied: want 10, got %d", dst.ReasoningTokens)
	}
}

// TestMergeKiroUsageSynthesisesTotalFromComponents covers the terminal case:
// the final `src` supplied no TotalTokens but components are non-zero. The
// merge must synthesise a total from input+output+reasoning.
func TestMergeKiroUsageSynthesisesTotalFromComponents(t *testing.T) {
	var dst usage.Detail
	mergeKiroUsage(&dst, usage.Detail{InputTokens: 30, OutputTokens: 40, ReasoningTokens: 10})
	if dst.TotalTokens != 80 {
		t.Fatalf("synthesised total wrong: want 80, got %d", dst.TotalTokens)
	}
}

// TestMergeKiroUsageNilDstSafe guards against NPE when the caller forgets
// to pass an initialized pointer. Production callers always pass &var, but
// this is a cheap guarantee worth having.
func TestMergeKiroUsageNilDstSafe(t *testing.T) {
	mergeKiroUsage(nil, usage.Detail{InputTokens: 10}) // must not panic
}
