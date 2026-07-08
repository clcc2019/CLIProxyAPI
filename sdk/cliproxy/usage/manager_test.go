package usage

import (
	"context"
	"testing"
	"time"
)

type pluginFunc func(context.Context, Record)

func (f pluginFunc) HandleUsage(ctx context.Context, record Record) {
	f(ctx, record)
}

func TestManagerFlushWaitsForQueueAndInFlightRecord(t *testing.T) {
	manager := NewManager(8)
	started := make(chan struct{})
	release := make(chan struct{})
	blocked := true

	manager.Register(pluginFunc(func(_ context.Context, _ Record) {
		if blocked {
			blocked = false
			close(started)
			<-release
		}
	}))

	manager.Publish(context.Background(), Record{Provider: "first"})
	<-started
	manager.Publish(context.Background(), Record{Provider: "second"})

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- manager.Flush(context.Background())
	}()

	select {
	case err := <-flushDone:
		t.Fatalf("Flush returned before queued record completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatalf("Flush returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Flush did not return after queued work completed")
	}
}

func TestManagerDropsOldestQueuedRecordWhenFull(t *testing.T) {
	manager := NewManager(2)
	started := make(chan struct{})
	release := make(chan struct{})
	seen := make(chan string, 4)

	manager.Register(pluginFunc(func(_ context.Context, record Record) {
		seen <- record.Provider
		if record.Provider == "first" {
			close(started)
			<-release
		}
	}))

	manager.Publish(context.Background(), Record{Provider: "first"})
	<-started

	manager.Publish(context.Background(), Record{Provider: "second"})
	manager.Publish(context.Background(), Record{Provider: "third"})
	manager.Publish(context.Background(), Record{Provider: "fourth"})
	close(release)

	if err := manager.Flush(context.Background()); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}
	close(seen)

	var records []string
	for record := range seen {
		records = append(records, record)
	}
	want := []string{"first", "third", "fourth"}
	if len(records) != len(want) {
		t.Fatalf("records = %v, want %v", records, want)
	}
	for i := range want {
		if records[i] != want[i] {
			t.Fatalf("records = %v, want %v", records, want)
		}
	}
}

func TestManagerDispatchUsesLatestRegisteredPluginSnapshot(t *testing.T) {
	manager := NewManager(4)
	firstSeen := make(chan string, 2)
	secondSeen := make(chan string, 1)

	manager.Register(pluginFunc(func(_ context.Context, record Record) {
		firstSeen <- record.Provider
	}))
	manager.Publish(context.Background(), Record{Provider: "before"})
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}

	manager.Register(pluginFunc(func(_ context.Context, record Record) {
		secondSeen <- record.Provider
	}))
	manager.Publish(context.Background(), Record{Provider: "after"})
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}

	if got := receiveUsageTestRecord(t, firstSeen); got != "before" {
		t.Fatalf("first plugin initial record = %q, want before", got)
	}
	if got := receiveUsageTestRecord(t, firstSeen); got != "after" {
		t.Fatalf("first plugin second record = %q, want after", got)
	}
	if got := receiveUsageTestRecord(t, secondSeen); got != "after" {
		t.Fatalf("second plugin record = %q, want after", got)
	}
}

func TestManagerStartContextCancellationClosesManager(t *testing.T) {
	manager := NewManager(4)
	ctx, cancel := context.WithCancel(context.Background())

	manager.Start(ctx)
	cancel()

	deadline := time.After(time.Second)
	for {
		manager.mu.Lock()
		closed := manager.closed
		manager.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("manager did not close after start context cancellation")
		case <-time.After(time.Millisecond):
		}
	}
}

func BenchmarkWithRequestedModelAliasSameValue(b *testing.B) {
	ctx := WithRequestedModelAlias(context.Background(), "gpt-test")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := WithRequestedModelAlias(ctx, "gpt-test"); got != ctx {
			b.Fatal("context was not reused")
		}
	}
}

func receiveUsageTestRecord(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
		return ""
	}
}
