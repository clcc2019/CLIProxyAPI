package auth

import (
	"context"
	"fmt"
	"sync"
)

// RefreshGroup deduplicates in-flight token refreshes while preserving each
// caller's ability to stop waiting. The first caller's context owns the shared
// upstream exchange so a cancelled refresh cannot continue rotating a token
// after its result no longer has a caller to persist it.
type RefreshGroup[T any] struct {
	mu    sync.Mutex
	calls map[string]*refreshCall[T]
}

type refreshCall[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// Do runs one refresh per key. Waiters may return when their own context is
// cancelled without cancelling the leader's request.
func (g *RefreshGroup[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if g == nil {
		return zero, fmt.Errorf("refresh group is nil")
	}
	if fn == nil {
		return zero, fmt.Errorf("refresh function is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*refreshCall[T])
	}
	if call := g.calls[key]; call != nil {
		g.mu.Unlock()
		return waitRefreshCall(ctx, call)
	}
	call := &refreshCall[T]{done: make(chan struct{})}
	g.calls[key] = call
	g.mu.Unlock()

	return g.runLeader(ctx, key, call, fn)
}

func (g *RefreshGroup[T]) runLeader(ctx context.Context, key string, call *refreshCall[T], fn func(context.Context) (T, error)) (value T, err error) {
	finished := false
	defer func() {
		if finished {
			return
		}
		var zero T
		g.finishCall(key, call, zero, fmt.Errorf("refresh function terminated without returning"))
	}()

	value, err = fn(ctx)
	g.finishCall(key, call, value, err)
	finished = true
	return value, err
}

func (g *RefreshGroup[T]) finishCall(key string, call *refreshCall[T], value T, err error) {
	g.mu.Lock()
	call.value = value
	call.err = err
	if g.calls[key] == call {
		delete(g.calls, key)
	}
	close(call.done)
	g.mu.Unlock()
}

func waitRefreshCall[T any](ctx context.Context, call *refreshCall[T]) (T, error) {
	// Prefer a completed exchange when completion and cancellation become
	// observable together. This avoids discarding a rotated token that is
	// already available to the caller for persistence.
	select {
	case <-call.done:
		return call.value, call.err
	default:
	}

	select {
	case <-call.done:
		return call.value, call.err
	case <-ctx.Done():
		select {
		case <-call.done:
			return call.value, call.err
		default:
			var zero T
			return zero, ctx.Err()
		}
	}
}
