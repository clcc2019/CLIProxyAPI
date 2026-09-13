package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type boundedRefreshExecutor struct {
	current atomic.Int32
	maximum atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (e *boundedRefreshExecutor) Identifier() string { return "force-refresh-test" }
func (e *boundedRefreshExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *boundedRefreshExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *boundedRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *boundedRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}
func (e *boundedRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	current := e.current.Add(1)
	for {
		maximum := e.maximum.Load()
		if current <= maximum || e.maximum.CompareAndSwap(maximum, current) {
			break
		}
	}
	e.entered <- struct{}{}
	<-e.release
	e.current.Add(-1)
	return auth, nil
}

func TestForceRefreshAllUsesConfiguredWorkerLimit(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.runtimeConfig.Store(&internalconfig.Config{AuthAutoRefreshWorkers: 2})
	executor := &boundedRefreshExecutor{entered: make(chan struct{}, 6), release: make(chan struct{})}
	manager.RegisterExecutor(executor)
	for i := 0; i < 6; i++ {
		if _, err := manager.Register(context.Background(), &Auth{ID: string(rune('a' + i)), Provider: executor.Identifier()}); err != nil {
			t.Fatalf("register auth: %v", err)
		}
	}

	done := make(chan []ForceRefreshResult, 1)
	go func() { done <- manager.ForceRefreshAll(context.Background()) }()
	for i := 0; i < 2; i++ {
		select {
		case <-executor.entered:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for refresh worker")
		}
	}
	select {
	case <-executor.entered:
		t.Fatal("refresh-all exceeded configured worker limit")
	case <-time.After(50 * time.Millisecond):
	}
	for i := 0; i < 6; i++ {
		executor.release <- struct{}{}
	}
	select {
	case results := <-done:
		if len(results) != 6 || executor.maximum.Load() > 2 {
			t.Fatalf("results=%d maximum=%d, want 6 results and max <= 2", len(results), executor.maximum.Load())
		}
		for _, result := range results {
			if !result.Success {
				t.Fatalf("refresh result failed: %+v", result)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh-all")
	}
}
