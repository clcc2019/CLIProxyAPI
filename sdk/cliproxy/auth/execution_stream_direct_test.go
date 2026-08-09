package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type streamResultRecordingHook struct {
	mu      sync.Mutex
	results []Result
}

func (*streamResultRecordingHook) OnAuthRegistered(context.Context, *Auth) {}
func (*streamResultRecordingHook) OnAuthUpdated(context.Context, *Auth)    {}

func (h *streamResultRecordingHook) OnResult(_ context.Context, result Result) {
	h.mu.Lock()
	h.results = append(h.results, result)
	h.mu.Unlock()
}

func (h *streamResultRecordingHook) snapshot() []Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Result(nil), h.results...)
}

func TestDirectStreamResultSuccessfulCompletionFinalizesOnce(t *testing.T) {
	hook := &streamResultRecordingHook{}
	manager := NewManager(nil, nil, hook)
	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	remaining <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-1"}}`)}
	close(remaining)
	var releases atomic.Int32
	result := manager.directStreamResult(
		context.Background(),
		"auth-1",
		"codex",
		"gpt-5.4",
		nil,
		[]cliproxyexecutor.StreamChunk{{Payload: []byte(`{"type":"response.created"}`)}},
		remaining,
		func() { releases.Add(1) },
	)

	for _, chunk := range result.Buffered {
		result.Observe(chunk)
	}
	for chunk := range result.Chunks {
		result.Observe(chunk)
	}
	result.Finalize(true)
	result.Finalize(true)

	results := hook.snapshot()
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("results = %#v, want one success", results)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release count = %d, want 1", got)
	}
}

func TestDirectStreamResultFailureMarkedOnce(t *testing.T) {
	hook := &streamResultRecordingHook{}
	manager := NewManager(nil, nil, hook)
	wantErr := errors.New("stream failed")
	var releases atomic.Int32
	result := manager.directStreamResult(
		context.Background(),
		"auth-1",
		"codex",
		"gpt-5.4",
		nil,
		nil,
		nil,
		func() { releases.Add(1) },
	)

	result.Observe(cliproxyexecutor.StreamChunk{Err: wantErr})
	result.Observe(cliproxyexecutor.StreamChunk{Err: wantErr})
	result.Finalize(true)
	result.Finalize(false)

	results := hook.snapshot()
	if len(results) != 1 || results[0].Success || results[0].Error == nil {
		t.Fatalf("results = %#v, want one failure", results)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release count = %d, want 1", got)
	}
}

func TestDirectStreamResultCancellationOnlyReleases(t *testing.T) {
	hook := &streamResultRecordingHook{}
	manager := NewManager(nil, nil, hook)
	var releases atomic.Int32
	result := manager.directStreamResult(
		context.Background(),
		"auth-1",
		"codex",
		"gpt-5.4",
		nil,
		nil,
		nil,
		func() { releases.Add(1) },
	)

	result.Finalize(false)
	result.Finalize(false)

	if results := hook.snapshot(); len(results) != 0 {
		t.Fatalf("results = %#v, want no success or failure on cancellation", results)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release count = %d, want 1", got)
	}
}
