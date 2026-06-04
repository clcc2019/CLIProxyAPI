package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexExecutorDoesNotReplayTurnScopedHeadersAcrossRequests(t *testing.T) {
	var requestCount atomic.Int32
	seenTurnState := make([]string, 0, 2)
	seenTurnMetadata := make([]string, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := requestCount.Add(1) - 1
		seenTurnState = append(seenTurnState, r.Header.Get(codexHeaderTurnState))
		seenTurnMetadata = append(seenTurnMetadata, r.Header.Get(codexHeaderTurnMetadata))

		w.Header().Set("Content-Type", "text/event-stream")
		if index == 0 {
			w.Header().Set(codexHeaderTurnState, "turn-state-1")
			w.Header().Set(codexHeaderTurnMetadata, `{"turn_id":"upstream-turn-1","thread_source":"user","sandbox":"none"}`)
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "auth-1",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test",
		},
	}
	request := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"cache-key","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	}

	if _, err := executor.Execute(context.Background(), auth, request, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if _, err := executor.Execute(context.Background(), auth, request, opts); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if got := requestCount.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	if len(seenTurnState) != 2 || len(seenTurnMetadata) != 2 {
		t.Fatalf("unexpected captured headers: turn_state=%v turn_metadata=%v", seenTurnState, seenTurnMetadata)
	}
	if seenTurnState[0] != "" {
		t.Fatalf("first request turn state = %q, want empty", seenTurnState[0])
	}
	if seenTurnState[1] != "" {
		t.Fatalf("second request turn state = %q, want empty for a new request", seenTurnState[1])
	}
	if seenTurnMetadata[0] == "" {
		t.Fatal("first request should carry generated turn metadata")
	}
	if seenTurnMetadata[1] == "" {
		t.Fatal("second request should carry generated turn metadata")
	}
	if seenTurnMetadata[1] == `{"turn_id":"upstream-turn-1","thread_source":"user","sandbox":"none"}` {
		t.Fatalf("second request should not replay upstream turn metadata: %q", seenTurnMetadata[1])
	}
}

func TestCodexExecutorReplaysTurnStateWithinExplicitHTTPExecutionTurn(t *testing.T) {
	var requestCount atomic.Int32
	seenTurnState := make([]string, 0, 2)
	seenTurnMetadata := make([]string, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := requestCount.Add(1) - 1
		seenTurnState = append(seenTurnState, r.Header.Get(codexHeaderTurnState))
		seenTurnMetadata = append(seenTurnMetadata, r.Header.Get(codexHeaderTurnMetadata))

		w.Header().Set("Content-Type", "text/event-stream")
		if index == 0 {
			w.Header().Set(codexHeaderTurnState, "turn-state-1")
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test",
		},
	}
	request := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"cache-key","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-1",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderTurnMetadata: `{"session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","sandbox":"none"}`,
	})

	if _, err := executor.Execute(ctx, auth, request, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if _, err := executor.Execute(ctx, auth, request, opts); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if got := requestCount.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	if len(seenTurnState) != 2 || len(seenTurnMetadata) != 2 {
		t.Fatalf("unexpected captured headers: turn_state=%v turn_metadata=%v", seenTurnState, seenTurnMetadata)
	}
	if seenTurnState[0] != "" {
		t.Fatalf("first request turn state = %q, want empty", seenTurnState[0])
	}
	if seenTurnState[1] != "turn-state-1" {
		t.Fatalf("second request turn state = %q, want turn-state-1", seenTurnState[1])
	}
	assertCodexTurnMetadataString(t, seenTurnMetadata[0], "turn_id", "turn-1")
	assertCodexTurnMetadataString(t, seenTurnMetadata[1], "turn_id", "turn-1")
}

func TestCodexExecutorRetriesAggregateWithoutStaleTurnState(t *testing.T) {
	var requestCount atomic.Int32
	seenTurnState := make([]string, 0, 3)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := requestCount.Add(1) - 1
		turnState := r.Header.Get(codexHeaderTurnState)
		seenTurnState = append(seenTurnState, turnState)

		w.Header().Set("Content-Type", "text/event-stream")
		switch index {
		case 0:
			w.Header().Set(codexHeaderTurnState, "turn-state-1")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		case 1:
			if turnState == "" {
				t.Fatal("second request should carry replayed turn state")
			}
			_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_2\",\"status\":\"failed\",\"error\":{\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp_1' not found.\",\"param\":\"previous_response_id\",\"type\":\"invalid_request_error\",\"status\":400}}}\n\n"))
		default:
			if turnState != "" {
				t.Fatalf("retry %s = %q, want empty", codexHeaderTurnState, turnState)
			}
			w.Header().Set(codexHeaderTurnState, "turn-state-2")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_3\",\"object\":\"response\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test",
		},
	}
	request := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"cache-key","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-1",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderTurnMetadata: `{"session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","sandbox":"none"}`,
	})

	if _, err := executor.Execute(ctx, auth, request, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if _, err := executor.Execute(ctx, auth, request, opts); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if got := requestCount.Load(); got != 3 {
		t.Fatalf("request count = %d, want 3", got)
	}
	if len(seenTurnState) != 3 {
		t.Fatalf("unexpected captured turn_state=%v", seenTurnState)
	}
	if seenTurnState[0] != "" {
		t.Fatalf("first request turn state = %q, want empty", seenTurnState[0])
	}
	if seenTurnState[1] != "turn-state-1" {
		t.Fatalf("second request turn state = %q, want turn-state-1", seenTurnState[1])
	}
	if seenTurnState[2] != "" {
		t.Fatalf("retry request turn state = %q, want empty", seenTurnState[2])
	}
}

func TestCodexExecutorDoesNotReplayTurnStateWhenExplicitHTTPTurnChanges(t *testing.T) {
	var requestCount atomic.Int32
	seenTurnState := make([]string, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := requestCount.Add(1) - 1
		seenTurnState = append(seenTurnState, r.Header.Get(codexHeaderTurnState))

		w.Header().Set("Content-Type", "text/event-stream")
		if index == 0 {
			w.Header().Set(codexHeaderTurnState, "turn-state-1")
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test",
		},
	}
	request := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"cache-key","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-1",
		},
	}

	firstCtx := contextWithGinHeaders(map[string]string{
		codexHeaderTurnMetadata: `{"session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","sandbox":"none"}`,
	})
	secondCtx := contextWithGinHeaders(map[string]string{
		codexHeaderTurnMetadata: `{"session_id":"session-1","thread_id":"thread-1","turn_id":"turn-2","sandbox":"none"}`,
	})

	if _, err := executor.Execute(firstCtx, auth, request, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if _, err := executor.Execute(secondCtx, auth, request, opts); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if got := requestCount.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	if len(seenTurnState) != 2 {
		t.Fatalf("unexpected captured turn_state=%v", seenTurnState)
	}
	if seenTurnState[0] != "" {
		t.Fatalf("first request turn state = %q, want empty", seenTurnState[0])
	}
	if seenTurnState[1] != "" {
		t.Fatalf("second request turn state = %q, want empty for a new turn", seenTurnState[1])
	}
}

func TestCodexExecutorResetExecutionSessionClearsHTTPTurnState(t *testing.T) {
	var requestCount atomic.Int32
	seenTurnState := make([]string, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := requestCount.Add(1) - 1
		seenTurnState = append(seenTurnState, r.Header.Get(codexHeaderTurnState))

		w.Header().Set("Content-Type", "text/event-stream")
		if index == 0 {
			w.Header().Set(codexHeaderTurnState, "turn-state-1")
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test",
		},
	}
	request := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"cache-key","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-1",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderTurnMetadata: `{"session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","sandbox":"none"}`,
	})

	if _, err := executor.Execute(ctx, auth, request, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	executor.ResetExecutionSession("exec-1")
	if _, err := executor.Execute(ctx, auth, request, opts); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if got := requestCount.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	if len(seenTurnState) != 2 {
		t.Fatalf("unexpected captured turn_state=%v", seenTurnState)
	}
	if seenTurnState[0] != "" {
		t.Fatalf("first request turn state = %q, want empty", seenTurnState[0])
	}
	if seenTurnState[1] != "" {
		t.Fatalf("second request turn state = %q, want empty after session reset", seenTurnState[1])
	}
}
