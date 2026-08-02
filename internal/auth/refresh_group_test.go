package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshGroupWaiterDeadlineDoesNotCancelLeader(t *testing.T) {
	var group RefreshGroup[int]
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	type callResult struct {
		value int
		err   error
	}
	leaderResult := make(chan callResult, 1)
	go func() {
		value, err := group.Do(context.Background(), "shared-token", func(ctx context.Context) (int, error) {
			calls.Add(1)
			close(started)
			select {
			case <-release:
				return 42, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		})
		leaderResult <- callResult{value: value, err: err}
	}()
	<-started

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelWaiter()
	_, err := group.Do(waiterCtx, "shared-token", func(context.Context) (int, error) {
		calls.Add(1)
		return 0, errors.New("waiter unexpectedly became refresh leader")
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error = %v, want context deadline exceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	select {
	case result := <-leaderResult:
		t.Fatalf("leader returned before release: %+v", result)
	default:
	}

	close(release)
	select {
	case result := <-leaderResult:
		if result.err != nil || result.value != 42 {
			t.Fatalf("leader result = %+v, want value 42", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh leader")
	}
}

func TestRefreshGroupLeaderCancellationStopsWork(t *testing.T) {
	var group RefreshGroup[int]
	started := make(chan struct{})
	workCancelled := make(chan struct{})
	result := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		_, err := group.Do(ctx, "cancelled-token", func(workCtx context.Context) (int, error) {
			close(started)
			<-workCtx.Done()
			close(workCancelled)
			return 0, workCtx.Err()
		})
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled leader")
	}
	select {
	case <-workCancelled:
	case <-time.After(time.Second):
		t.Fatal("refresh work did not observe leader cancellation")
	}
}

func TestWaitRefreshCallPrefersReadyResultOverCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	call := &refreshCall[int]{done: make(chan struct{}), value: 42}
	close(call.done)

	value, err := waitRefreshCall(ctx, call)
	if err != nil || value != 42 {
		t.Fatalf("waitRefreshCall() = (%d, %v), want (42, nil)", value, err)
	}
}
