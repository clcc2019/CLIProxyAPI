package usage

import (
	"context"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestAggregatedUsageSnapshotDoesNotMergeImportedRollingWindows(t *testing.T) {
	stats := NewRequestStatistics()
	stats.MergeImportedAggregatedSnapshot(AggregatedUsageSnapshot{
		GeneratedAt: time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC),
		ModelNames:  []string{"gpt-5.4"},
		Windows: map[string]AggregatedUsageWindow{
			"1h": {
				TotalRequests: 3,
				TotalTokens:   30,
				ModelNames:    []string{"gpt-5.4"},
			},
			"all": {
				TotalRequests: 3,
				TotalTokens:   30,
				ModelNames:    []string{"gpt-5.4"},
			},
		},
	})

	snapshot := stats.AggregatedUsageSnapshot(time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC))

	if got := snapshot.Windows["1h"].TotalRequests; got != 0 {
		t.Fatalf("1h total_requests = %d, want 0", got)
	}
	if got := snapshot.Windows["1h"].TotalTokens; got != 0 {
		t.Fatalf("1h total_tokens = %d, want 0", got)
	}
	if got := snapshot.Windows["all"].TotalRequests; got != 3 {
		t.Fatalf("all total_requests = %d, want 3", got)
	}
	if got := snapshot.Windows["all"].TotalTokens; got != 30 {
		t.Fatalf("all total_tokens = %d, want 30", got)
	}
}

func TestAggregatedUsageSnapshotSkipsDuplicateImportedAllWindow(t *testing.T) {
	stats := NewRequestStatistics()
	imported := AggregatedUsageSnapshot{
		GeneratedAt: time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC),
		ModelNames:  []string{"gpt-5.4"},
		Windows: map[string]AggregatedUsageWindow{
			"all": {
				TotalRequests: 2,
				TotalTokens:   20,
				ModelNames:    []string{"gpt-5.4"},
			},
		},
	}

	stats.MergeImportedAggregatedSnapshot(imported)
	stats.MergeImportedAggregatedSnapshot(imported)

	snapshot := stats.AggregatedUsageSnapshot(time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC))
	if got := snapshot.Windows["all"].TotalRequests; got != 2 {
		t.Fatalf("all total_requests = %d, want 2", got)
	}
	if got := snapshot.Windows["all"].TotalTokens; got != 20 {
		t.Fatalf("all total_tokens = %d, want 20", got)
	}
}

func TestAggregatedUsageSnapshotIgnoresDetailRetentionLimit(t *testing.T) {
	previousLimit := DetailRetentionLimit()
	SetDetailRetentionLimit(3)
	t.Cleanup(func() { SetDetailRetentionLimit(previousLimit) })

	stats := NewRequestStatistics()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "retention-test",
			Model:       "gpt-5.4",
			RequestedAt: now.Add(time.Duration(-55+i) * time.Minute),
			Detail: coreusage.Detail{
				InputTokens:  1,
				OutputTokens: 1,
				TotalTokens:  2,
			},
		})
	}

	details := stats.Snapshot().APIs["retention-test"].Models["gpt-5.4"].Details
	if got := len(details); got != 3 {
		t.Fatalf("retained details len = %d, want 3", got)
	}

	snapshot := stats.AggregatedUsageSnapshot(now)
	if got := snapshot.Windows["1h"].TotalRequests; got != 6 {
		t.Fatalf("1h total_requests = %d, want 6", got)
	}
	if got := snapshot.Windows["1h"].TotalTokens; got != 12 {
		t.Fatalf("1h total_tokens = %d, want 12", got)
	}
}

func TestAggregatedUsageSnapshotRollsUpExpiredAggregateRecords(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "rollup-test",
		Model:       "gpt-5.4",
		RequestedAt: now.Add(-8 * 24 * time.Hour),
		Detail: coreusage.Detail{
			InputTokens:  1,
			OutputTokens: 1,
			TotalTokens:  2,
		},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "rollup-test",
		Model:       "gpt-5.4",
		RequestedAt: now.Add(-30 * time.Minute),
		Detail: coreusage.Detail{
			InputTokens:  2,
			OutputTokens: 2,
			TotalTokens:  4,
		},
	})

	if got := len(stats.aggregateRecords); got != 1 {
		t.Fatalf("aggregateRecords len = %d, want 1", got)
	}

	snapshot := stats.AggregatedUsageSnapshot(now)
	if got := snapshot.Windows["1h"].TotalRequests; got != 1 {
		t.Fatalf("1h total_requests = %d, want 1", got)
	}
	if got := snapshot.Windows["7d"].TotalRequests; got != 1 {
		t.Fatalf("7d total_requests = %d, want 1", got)
	}
	if got := snapshot.Windows["all"].TotalRequests; got != 2 {
		t.Fatalf("all total_requests = %d, want 2", got)
	}
	if got := snapshot.Windows["all"].TotalTokens; got != 6 {
		t.Fatalf("all total_tokens = %d, want 6", got)
	}
}

func TestAggregatedUsageSnapshotRollsUpOutOfOrderExpiredRecord(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "rollup-test",
		Model:       "gpt-5.4",
		RequestedAt: now,
		Detail: coreusage.Detail{
			InputTokens:  2,
			OutputTokens: 2,
			TotalTokens:  4,
		},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "rollup-test",
		Model:       "gpt-5.4",
		RequestedAt: now.Add(-8 * 24 * time.Hour),
		Detail: coreusage.Detail{
			InputTokens:  1,
			OutputTokens: 1,
			TotalTokens:  2,
		},
	})

	if got := len(stats.aggregateRecords); got != 1 {
		t.Fatalf("aggregateRecords len = %d, want 1", got)
	}

	snapshot := stats.AggregatedUsageSnapshot(now)
	if got := snapshot.Windows["7d"].TotalRequests; got != 1 {
		t.Fatalf("7d total_requests = %d, want 1", got)
	}
	if got := snapshot.Windows["all"].TotalRequests; got != 2 {
		t.Fatalf("all total_requests = %d, want 2", got)
	}
}

func TestAggregatedUsageSnapshotPrunesDeferredOutOfOrderExpiredRecord(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "rollup-test",
		Model:       "gpt-5.4",
		RequestedAt: now,
		Detail: coreusage.Detail{
			InputTokens:  2,
			OutputTokens: 2,
			TotalTokens:  4,
		},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "rollup-test",
		Model:       "gpt-5.4",
		RequestedAt: now.Add(-6 * 24 * time.Hour),
		Detail: coreusage.Detail{
			InputTokens:  1,
			OutputTokens: 1,
			TotalTokens:  2,
		},
	})
	if got := len(stats.aggregateRecords); got != 2 {
		t.Fatalf("aggregateRecords len after second record = %d, want 2", got)
	}

	later := now.Add(48 * time.Hour)
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "rollup-test",
		Model:       "gpt-5.4",
		RequestedAt: later,
		Detail: coreusage.Detail{
			InputTokens:  3,
			OutputTokens: 3,
			TotalTokens:  6,
		},
	})

	if got := len(stats.aggregateRecords); got != 2 {
		t.Fatalf("aggregateRecords len after pruning = %d, want 2", got)
	}

	snapshot := stats.AggregatedUsageSnapshot(later)
	if got := snapshot.Windows["7d"].TotalRequests; got != 2 {
		t.Fatalf("7d total_requests = %d, want 2", got)
	}
	if got := snapshot.Windows["all"].TotalRequests; got != 3 {
		t.Fatalf("all total_requests = %d, want 3", got)
	}
	if got := snapshot.Windows["all"].TotalTokens; got != 12 {
		t.Fatalf("all total_tokens = %d, want 12", got)
	}
}

func TestAggregatedUsageSnapshotCompactsEquivalentRecordsByMinute(t *testing.T) {
	previousLimit := DetailRetentionLimit()
	SetDetailRetentionLimit(1)
	t.Cleanup(func() { SetDetailRetentionLimit(previousLimit) })

	stats := NewRequestStatistics()
	minute := time.Date(2026, 7, 10, 8, 15, 0, 0, time.UTC)
	const requestCount = 1000
	for i := 0; i < requestCount; i++ {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "bucket-api",
			Model:       "gpt-5.4",
			Source:      "user@example.com",
			AuthIndex:   "auth-1",
			RequestedAt: minute.Add(time.Duration(i%60) * time.Second),
			Latency:     time.Duration(100+i%5) * time.Millisecond,
			Detail: coreusage.Detail{
				InputTokens:  2,
				OutputTokens: 3,
				CachedTokens: 1,
				TotalTokens:  6,
			},
		})
	}

	if got := len(stats.aggregateRecords); got != 1 {
		t.Fatalf("aggregateRecords len = %d, want 1", got)
	}
	record := stats.aggregateRecords[0]
	if got := record.Requests; got != requestCount {
		t.Fatalf("bucket requests = %d, want %d", got, requestCount)
	}
	if got := record.Latency.Count; got != requestCount {
		t.Fatalf("bucket latency count = %d, want %d", got, requestCount)
	}

	snapshot := stats.AggregatedUsageSnapshot(minute.Add(59 * time.Second))
	window := snapshot.Windows["1h"]
	if got := window.TotalRequests; got != requestCount {
		t.Fatalf("1h total requests = %d, want %d", got, requestCount)
	}
	if got := window.SuccessCount; got != requestCount {
		t.Fatalf("1h success count = %d, want %d", got, requestCount)
	}
	if got := window.TotalTokens; got != requestCount*6 {
		t.Fatalf("1h total tokens = %d, want %d", got, requestCount*6)
	}
	if got := window.Latency.Count; got != requestCount {
		t.Fatalf("1h latency count = %d, want %d", got, requestCount)
	}
	if window.Latency.MinMs != 100 || window.Latency.MaxMs != 104 {
		t.Fatalf("1h latency range = [%d,%d], want [100,104]", window.Latency.MinMs, window.Latency.MaxMs)
	}
	if window.Rate30m.RequestCount != requestCount || window.Rate30m.TokenCount != requestCount*6 {
		t.Fatalf("1h rate = %+v", window.Rate30m)
	}
	var sparkRequests, sparkTokens int64
	for i := range window.Sparklines.Requests {
		sparkRequests += window.Sparklines.Requests[i]
		sparkTokens += window.Sparklines.Tokens[i]
	}
	if sparkRequests != requestCount || sparkTokens != requestCount*6 {
		t.Fatalf("1h sparkline totals = requests %d tokens %d", sparkRequests, sparkTokens)
	}
	if len(window.APIs) != 1 || window.APIs[0].TotalRequests != requestCount {
		t.Fatalf("1h API aggregates = %+v", window.APIs)
	}
	if len(window.Models) != 1 || window.Models[0].Requests != requestCount {
		t.Fatalf("1h model aggregates = %+v", window.Models)
	}
	if len(window.Credentials) != 1 || window.Credentials[0].TotalRequests != requestCount {
		t.Fatalf("1h credential aggregates = %+v", window.Credentials)
	}
}

func BenchmarkRequestStatisticsRecordSameMinute(b *testing.B) {
	previousLimit := DetailRetentionLimit()
	SetDetailRetentionLimit(1)
	b.Cleanup(func() { SetDetailRetentionLimit(previousLimit) })

	stats := NewRequestStatistics()
	record := coreusage.Record{
		APIKey:      "benchmark-api",
		Model:       "gpt-5.4",
		Source:      "user@example.com",
		AuthIndex:   "auth-1",
		RequestedAt: time.Date(2026, 7, 10, 8, 15, 0, 0, time.UTC),
		Latency:     100 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  2,
			OutputTokens: 3,
			TotalTokens:  5,
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stats.Record(context.Background(), record)
	}
}

func BenchmarkAggregatedUsageSnapshotCompacted(b *testing.B) {
	previousLimit := DetailRetentionLimit()
	SetDetailRetentionLimit(1)
	b.Cleanup(func() { SetDetailRetentionLimit(previousLimit) })

	stats := NewRequestStatistics()
	now := time.Date(2026, 7, 10, 8, 15, 59, 0, time.UTC)
	record := coreusage.Record{
		APIKey:      "benchmark-api",
		Model:       "gpt-5.4",
		Source:      "user@example.com",
		AuthIndex:   "auth-1",
		RequestedAt: now.Truncate(time.Minute),
		Latency:     100 * time.Millisecond,
		Detail:      coreusage.Detail{InputTokens: 2, OutputTokens: 3, TotalTokens: 5},
	}
	for i := 0; i < 100_000; i++ {
		stats.Record(context.Background(), record)
	}
	if len(stats.aggregateRecords) != 1 {
		b.Fatalf("aggregateRecords len = %d, want 1", len(stats.aggregateRecords))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = stats.AggregatedUsageSnapshot(now)
	}
}
