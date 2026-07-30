package management

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestAggregatedUsageResponseCacheInvalidatesOnRecord(t *testing.T) {
	stats := usage.NewRequestStatistics()
	now := time.Now().UTC().Truncate(time.Second)
	record := coreusage.Record{
		APIKey:      "aggregate-cache-test",
		Model:       "gpt-5.4",
		RequestedAt: now,
		Detail: coreusage.Detail{
			InputTokens:  3,
			OutputTokens: 5,
			TotalTokens:  8,
		},
	}
	stats.Record(context.Background(), record)

	handler := &Handler{usageStats: stats}
	first, err := handler.aggregatedUsageResponse(now)
	if err != nil {
		t.Fatalf("first aggregate response: %v", err)
	}
	second, err := handler.aggregatedUsageResponse(now.Add(250 * time.Millisecond))
	if err != nil {
		t.Fatalf("cached aggregate response: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("unchanged aggregate response was not served from cache")
	}

	stats.Record(context.Background(), record)
	third, err := handler.aggregatedUsageResponse(now.Add(500 * time.Millisecond))
	if err != nil {
		t.Fatalf("invalidated aggregate response: %v", err)
	}
	if bytes.Equal(first, third) {
		t.Fatal("aggregate response did not refresh after recording usage")
	}

	var payload aggregatedUsageResponse
	if err := json.Unmarshal(third, &payload); err != nil {
		t.Fatalf("decode aggregate response: %v", err)
	}
	if got := payload.Usage.Windows["all"].TotalRequests; got != 2 {
		t.Fatalf("all-window total_requests = %d, want 2", got)
	}
	if got := handler.aggregatedUsageCache.entry.revision; got != stats.AggregatedUsageRevision() {
		t.Fatalf("cached revision = %d, want %d", got, stats.AggregatedUsageRevision())
	}
}

func TestAggregatedUsageResponseCacheExpires(t *testing.T) {
	stats := usage.NewRequestStatistics()
	now := time.Now().UTC().Truncate(time.Second)
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "aggregate-cache-expiry-test",
		Model:       "gpt-5.4",
		RequestedAt: now,
		Detail:      coreusage.Detail{TotalTokens: 1},
	})

	handler := &Handler{usageStats: stats}
	first, err := handler.aggregatedUsageResponse(now)
	if err != nil {
		t.Fatalf("first aggregate response: %v", err)
	}
	second, err := handler.aggregatedUsageResponse(now.Add(aggregatedUsageCacheTTL + time.Millisecond))
	if err != nil {
		t.Fatalf("expired aggregate response: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("expired aggregate response was reused")
	}
}

func BenchmarkAggregatedUsageResponseCacheHit(b *testing.B) {
	stats := usage.NewRequestStatistics()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 1000; i++ {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "aggregate-cache-benchmark",
			Model:       "gpt-5.4",
			RequestedAt: now,
			Detail:      coreusage.Detail{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
		})
	}

	handler := &Handler{usageStats: stats}
	if _, err := handler.aggregatedUsageResponse(now); err != nil {
		b.Fatalf("warm cache: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := handler.aggregatedUsageResponse(now); err != nil {
			b.Fatalf("cached aggregate response: %v", err)
		}
	}
}
