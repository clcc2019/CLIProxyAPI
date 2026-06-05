package usage

import (
	"context"
	"sync"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestRequestStatisticsRecordIncludesLatency(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].LatencyMs != 1500 {
		t.Fatalf("latency_ms = %d, want 1500", details[0].LatencyMs)
	}
	if details[0].APIKey != "test-key" {
		t.Fatalf("api_key = %q, want %q", details[0].APIKey, "test-key")
	}

	model := snapshot.APIs["test-key"].Models["gpt-5.4"]
	if model.TokenBreakdown.InputTokens != 10 || model.TokenBreakdown.OutputTokens != 20 || model.TokenBreakdown.TotalTokens != 30 {
		t.Fatalf("token breakdown = %+v, want input=10 output=20 total=30", model.TokenBreakdown)
	}
	if model.Latency.Count != 1 || model.Latency.TotalMs != 1500 || model.Latency.MinMs != 1500 || model.Latency.MaxMs != 1500 {
		t.Fatalf("latency summary = %+v, want count=1 total=min=max=1500", model.Latency)
	}
}

func TestRequestStatisticsNormalisesOpenAIReasoningAsOutputDetail(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		Provider:    "openai",
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:     100,
			OutputTokens:    50,
			ReasoningTokens: 15,
		},
	})

	model := stats.Snapshot().APIs["test-key"].Models["gpt-5.4"]
	if model.TotalTokens != 150 || model.TokenBreakdown.TotalTokens != 150 {
		t.Fatalf("total tokens = model:%d breakdown:%d, want 150 without double-counting reasoning", model.TotalTokens, model.TokenBreakdown.TotalTokens)
	}
	if model.TokenBreakdown.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", model.TokenBreakdown.ReasoningTokens)
	}
}

func TestRequestStatisticsNormalisesSeparateReasoningProviders(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		Provider:    "kimi",
		APIKey:      "test-key",
		Model:       "gpt-5",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:     100,
			OutputTokens:    50,
			ReasoningTokens: 15,
		},
	})

	model := stats.Snapshot().APIs["test-key"].Models["gpt-5"]
	if model.TotalTokens != 165 || model.TokenBreakdown.TotalTokens != 165 {
		t.Fatalf("total tokens = model:%d breakdown:%d, want 165 with separate reasoning", model.TotalTokens, model.TokenBreakdown.TotalTokens)
	}
}

func TestRequestStatisticsRecordIncludesErrorMessageForFailures(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:       "test-key",
		Model:        "gpt-5.4",
		RequestedAt:  time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Failed:       true,
		ErrorMessage: " upstream quota exhausted\ntry later ",
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].ErrorMessage != "upstream quota exhausted try later" {
		t.Fatalf("error_message = %q, want normalized error", details[0].ErrorMessage)
	}
}

func TestRequestStatisticsRecordOmitsErrorMessageForSuccess(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:       "test-key",
		Model:        "gpt-5.4",
		RequestedAt:  time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		ErrorMessage: "should not leak",
	})

	detail := stats.Snapshot().APIs["test-key"].Models["gpt-5.4"].Details[0]
	if detail.ErrorMessage != "" {
		t.Fatalf("error_message = %q, want empty for success", detail.ErrorMessage)
	}
}

func TestRequestStatisticsMergeSnapshotDedupIgnoresLatency(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	first := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 0,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}
	second := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 2500,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}

	result := stats.MergeSnapshot(first)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=1 skipped=0", result)
	}

	result = stats.MergeSnapshot(second)
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("second merge = %+v, want added=0 skipped=1", result)
	}

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
}

func TestRequestStatisticsMergeSnapshotDedupIncludesDetailAPIKey(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	first := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"shared-api": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							APIKey:    "client-key-a",
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}
	second := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"shared-api": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							APIKey:    "client-key-b",
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}

	result := stats.MergeSnapshot(first)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=1 skipped=0", result)
	}

	result = stats.MergeSnapshot(second)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("second merge = %+v, want added=1 skipped=0", result)
	}

	snapshot := stats.Snapshot()
	details := snapshot.APIs["shared-api"].Models["gpt-5.4"].Details
	if len(details) != 2 {
		t.Fatalf("details len = %d, want 2", len(details))
	}
}

func TestRequestStatisticsRetainsAllDetails(t *testing.T) {
	previousLimit := DetailRetentionLimit()
	SetDetailRetentionLimit(0)
	t.Cleanup(func() { SetDetailRetentionLimit(previousLimit) })

	stats := NewRequestStatistics()
	start := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	const count = 517
	for i := 0; i < count; i++ {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: start.Add(time.Duration(i) * time.Second),
			Detail: coreusage.Detail{
				InputTokens:  1,
				OutputTokens: 1,
				TotalTokens:  2,
			},
		})
	}

	snapshot := stats.Snapshot()
	model := snapshot.APIs["test-key"].Models["gpt-5.4"]
	if model.TotalRequests != int64(count) {
		t.Fatalf("total requests = %d, want %d", model.TotalRequests, count)
	}
	if len(model.Details) != count {
		t.Fatalf("details len = %d, want %d", len(model.Details), count)
	}

	wantFirst := start
	if !model.Details[0].Timestamp.Equal(wantFirst) {
		t.Fatalf("first retained timestamp = %s, want %s", model.Details[0].Timestamp, wantFirst)
	}
	wantLast := start.Add(time.Duration(count-1) * time.Second)
	if !model.Details[len(model.Details)-1].Timestamp.Equal(wantLast) {
		t.Fatalf("last retained timestamp = %s, want %s", model.Details[len(model.Details)-1].Timestamp, wantLast)
	}
}

func TestRequestStatisticsLimitsRetainedDetails(t *testing.T) {
	previousLimit := DetailRetentionLimit()
	SetDetailRetentionLimit(3)
	t.Cleanup(func() { SetDetailRetentionLimit(previousLimit) })

	stats := NewRequestStatistics()
	start := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	const count = 6
	for i := 0; i < count; i++ {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: start.Add(time.Duration(i) * time.Second),
			Detail: coreusage.Detail{
				InputTokens:  1,
				OutputTokens: 1,
				TotalTokens:  2,
			},
		})
	}

	snapshot := stats.Snapshot()
	model := snapshot.APIs["test-key"].Models["gpt-5.4"]
	if model.TotalRequests != count {
		t.Fatalf("total requests = %d, want %d", model.TotalRequests, count)
	}
	if len(model.Details) != 3 {
		t.Fatalf("details len = %d, want 3", len(model.Details))
	}
	if !model.Details[0].Timestamp.Equal(start.Add(3 * time.Second)) {
		t.Fatalf("first retained timestamp = %s, want %s", model.Details[0].Timestamp, start.Add(3*time.Second))
	}
	if !model.Details[2].Timestamp.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("last retained timestamp = %s, want %s", model.Details[2].Timestamp, start.Add(5*time.Second))
	}
}

func TestRequestStatisticsApplyDetailRetentionLimitTrimsExistingDetails(t *testing.T) {
	stats := NewRequestStatistics()
	start := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	const count = 6
	for i := 0; i < count; i++ {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: start.Add(time.Duration(i) * time.Second),
			Detail: coreusage.Detail{
				InputTokens:  1,
				OutputTokens: 1,
				TotalTokens:  2,
			},
		})
	}

	stats.ApplyDetailRetentionLimit(3)

	snapshot := stats.Snapshot()
	model := snapshot.APIs["test-key"].Models["gpt-5.4"]
	if model.TotalRequests != count {
		t.Fatalf("total requests = %d, want %d", model.TotalRequests, count)
	}
	if len(model.Details) != 3 {
		t.Fatalf("details len = %d, want 3", len(model.Details))
	}
	if !model.Details[0].Timestamp.Equal(start.Add(3 * time.Second)) {
		t.Fatalf("first retained timestamp = %s, want %s", model.Details[0].Timestamp, start.Add(3*time.Second))
	}
	if !model.Details[2].Timestamp.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("last retained timestamp = %s, want %s", model.Details[2].Timestamp, start.Add(5*time.Second))
	}
}

func TestRequestStatisticsApplyDetailRetentionLimitTrimsImportedDetailedSources(t *testing.T) {
	stats := NewRequestStatistics()
	start := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	result := stats.UpsertImportedDetailedSnapshot("source-a", StatisticsSnapshot{
		TotalRequests: 4,
		APIs: map[string]APISnapshot{
			"imported-api": {
				TotalRequests: 4,
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						TotalRequests: 4,
						Details: []RequestDetail{
							{Timestamp: start, Tokens: TokenStats{TotalTokens: 1}},
							{Timestamp: start.Add(1 * time.Second), Tokens: TokenStats{TotalTokens: 2}},
							{Timestamp: start.Add(2 * time.Second), Tokens: TokenStats{TotalTokens: 3}},
							{Timestamp: start.Add(3 * time.Second), Tokens: TokenStats{TotalTokens: 4}},
						},
					},
				},
			},
		},
	})
	if result.Added != 4 {
		t.Fatalf("import result added = %d, want 4", result.Added)
	}

	stats.ApplyDetailRetentionLimit(2)

	snapshot := stats.Snapshot()
	model := snapshot.APIs["imported-api"].Models["gpt-5.4"]
	if len(model.Details) != 2 {
		t.Fatalf("details len = %d, want 2", len(model.Details))
	}
	if !model.Details[0].Timestamp.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("first retained timestamp = %s, want %s", model.Details[0].Timestamp, start.Add(2*time.Second))
	}
	if !model.Details[1].Timestamp.Equal(start.Add(3 * time.Second)) {
		t.Fatalf("last retained timestamp = %s, want %s", model.Details[1].Timestamp, start.Add(3*time.Second))
	}
	if snapshot.TotalRequests != 4 {
		t.Fatalf("total requests = %d, want 4", snapshot.TotalRequests)
	}
}

func TestRequestStatisticsMergeSnapshotSummaryOnly(t *testing.T) {
	stats := NewRequestStatistics()
	summary := StatisticsSnapshot{
		TotalRequests: 5,
		SuccessCount:  4,
		FailureCount:  1,
		TotalTokens:   500,
		APIs: map[string]APISnapshot{
			"summary-api": {
				TotalRequests: 5,
				TotalTokens:   500,
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						TotalRequests:  5,
						TotalTokens:    500,
						TokenBreakdown: TokenStats{InputTokens: 200, OutputTokens: 250, ReasoningTokens: 50, TotalTokens: 500},
						Latency:        LatencyStats{Count: 4, TotalMs: 1600, MinMs: 250, MaxMs: 550},
					},
				},
			},
		},
		RequestsByDay: map[string]int64{
			"2026-04-10": 5,
		},
		RequestsByHour: map[string]int64{
			"13": 5,
		},
		TokensByDay: map[string]int64{
			"2026-04-10": 500,
		},
		TokensByHour: map[string]int64{
			"13": 500,
		},
	}

	result := stats.MergeSnapshot(summary)
	if result.Added != 5 || result.Skipped != 0 {
		t.Fatalf("merge result = %+v, want added=5 skipped=0", result)
	}

	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 5 {
		t.Fatalf("total requests = %d, want 5", snapshot.TotalRequests)
	}
	if snapshot.SuccessCount != 4 {
		t.Fatalf("success count = %d, want 4", snapshot.SuccessCount)
	}
	if snapshot.FailureCount != 1 {
		t.Fatalf("failure count = %d, want 1", snapshot.FailureCount)
	}
	if snapshot.TotalTokens != 500 {
		t.Fatalf("total tokens = %d, want 500", snapshot.TotalTokens)
	}

	api := snapshot.APIs["summary-api"]
	if api.TotalRequests != 5 || api.TotalTokens != 500 {
		t.Fatalf("api totals = %+v, want requests=5 tokens=500", api)
	}
	model := api.Models["gpt-5.4"]
	if model.TotalRequests != 5 || model.TotalTokens != 500 {
		t.Fatalf("model totals = %+v, want requests=5 tokens=500", model)
	}
	if model.TokenBreakdown.InputTokens != 200 || model.TokenBreakdown.OutputTokens != 250 || model.TokenBreakdown.ReasoningTokens != 50 || model.TokenBreakdown.TotalTokens != 500 {
		t.Fatalf("token breakdown = %+v, want input=200 output=250 reasoning=50 total=500", model.TokenBreakdown)
	}
	if model.Latency.Count != 4 || model.Latency.TotalMs != 1600 || model.Latency.MinMs != 250 || model.Latency.MaxMs != 550 {
		t.Fatalf("latency summary = %+v, want count=4 total=1600 min=250 max=550", model.Latency)
	}
	if len(model.Details) != 0 {
		t.Fatalf("details len = %d, want 0", len(model.Details))
	}

	if got := snapshot.RequestsByDay["2026-04-10"]; got != 5 {
		t.Fatalf("requests_by_day[2026-04-10] = %d, want 5", got)
	}
	if got := snapshot.RequestsByHour["13"]; got != 5 {
		t.Fatalf("requests_by_hour[13] = %d, want 5", got)
	}
	if got := snapshot.TokensByDay["2026-04-10"]; got != 500 {
		t.Fatalf("tokens_by_day[2026-04-10] = %d, want 500", got)
	}
	if got := snapshot.TokensByHour["13"]; got != 500 {
		t.Fatalf("tokens_by_hour[13] = %d, want 500", got)
	}
}

func TestRequestStatisticsMergeSnapshotSummaryOnlySkipsDuplicateImport(t *testing.T) {
	stats := NewRequestStatistics()
	summary := StatisticsSnapshot{
		TotalRequests: 3,
		SuccessCount:  2,
		FailureCount:  1,
		TotalTokens:   90,
		APIs: map[string]APISnapshot{
			"summary-api": {
				TotalRequests: 3,
				TotalTokens:   90,
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						TotalRequests:  3,
						TotalTokens:    90,
						TokenBreakdown: TokenStats{InputTokens: 40, OutputTokens: 45, ReasoningTokens: 5, TotalTokens: 90},
						Latency:        LatencyStats{Count: 3, TotalMs: 900, MinMs: 200, MaxMs: 400},
					},
				},
			},
		},
		RequestsByDay: map[string]int64{"2026-04-10": 3},
		RequestsByHour: map[string]int64{
			"13": 3,
		},
		TokensByDay: map[string]int64{"2026-04-10": 90},
		TokensByHour: map[string]int64{
			"13": 90,
		},
	}

	result := stats.MergeSnapshot(summary)
	if result.Added != 3 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=3 skipped=0", result)
	}

	result = stats.MergeSnapshot(summary)
	if result.Added != 0 || result.Skipped != 3 {
		t.Fatalf("second merge = %+v, want added=0 skipped=3", result)
	}

	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 3 {
		t.Fatalf("total requests = %d, want 3", snapshot.TotalRequests)
	}
	if snapshot.SuccessCount != 2 {
		t.Fatalf("success count = %d, want 2", snapshot.SuccessCount)
	}
	if snapshot.FailureCount != 1 {
		t.Fatalf("failure count = %d, want 1", snapshot.FailureCount)
	}
	if snapshot.TotalTokens != 90 {
		t.Fatalf("total tokens = %d, want 90", snapshot.TotalTokens)
	}
}

func TestSnapshotSummaryOmitsDetailsButPreservesAggregates(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "summary-test",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 4, 10, 13, 0, 0, 0, time.UTC),
		Latency:     1200 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:     10,
			OutputTokens:    20,
			ReasoningTokens: 5,
			CachedTokens:    2,
			TotalTokens:     35,
		},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "summary-test",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 4, 10, 13, 1, 0, 0, time.UTC),
		Latency:     800 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  3,
			OutputTokens: 7,
			TotalTokens:  10,
		},
	})

	snapshot := stats.SnapshotSummary()
	model := snapshot.APIs["summary-test"].Models["gpt-5.4"]

	if len(model.Details) != 0 {
		t.Fatalf("details len = %d, want 0", len(model.Details))
	}
	if model.TokenBreakdown.InputTokens != 13 || model.TokenBreakdown.OutputTokens != 27 || model.TokenBreakdown.ReasoningTokens != 5 || model.TokenBreakdown.CachedTokens != 2 || model.TokenBreakdown.TotalTokens != 45 {
		t.Fatalf("token breakdown = %+v, want input=13 output=27 reasoning=5 cached=2 total=45", model.TokenBreakdown)
	}
	if model.Latency.Count != 2 || model.Latency.TotalMs != 2000 || model.Latency.MinMs != 800 || model.Latency.MaxMs != 1200 {
		t.Fatalf("latency summary = %+v, want count=2 total=2000 min=800 max=1200", model.Latency)
	}
}

// TestRequestStatisticsConcurrentRecordSnapshotIsRaceFree exercises the
// atomic-counter contract under heavy concurrent Record/Snapshot to surface
// any races introduced by moving the four global counters out of the lock.
// Run with -race to catch regressions.
func TestRequestStatisticsConcurrentRecordSnapshotIsRaceFree(t *testing.T) {
	prevEnabled := StatisticsEnabled()
	SetStatisticsEnabled(true)
	t.Cleanup(func() { SetStatisticsEnabled(prevEnabled) })

	stats := NewRequestStatistics()
	const writers = 8
	const writesPerWriter = 200
	const readers = 4
	const readsPerReader = 200

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	now := time.Now().UTC()
	for w := 0; w < writers; w++ {
		go func(idx int) {
			defer wg.Done()
			for i := 0; i < writesPerWriter; i++ {
				stats.Record(context.Background(), coreusage.Record{
					APIKey:      "k",
					Model:       "m",
					Source:      "src",
					RequestedAt: now,
					Latency:     time.Millisecond,
					Detail: coreusage.Detail{
						InputTokens:  1,
						OutputTokens: 1,
						TotalTokens:  2,
					},
				})
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < readsPerReader; i++ {
				_ = stats.Snapshot()
			}
		}()
	}
	wg.Wait()

	final := stats.Snapshot()
	expected := int64(writers * writesPerWriter)
	if final.TotalRequests != expected {
		t.Fatalf("TotalRequests=%d, want %d", final.TotalRequests, expected)
	}
	if final.SuccessCount != expected {
		t.Fatalf("SuccessCount=%d, want %d", final.SuccessCount, expected)
	}
	if final.TotalTokens != expected*2 {
		t.Fatalf("TotalTokens=%d, want %d", final.TotalTokens, expected*2)
	}
}
