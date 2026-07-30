package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const codexControlRateLimitEvent = `{"type":"codex.rate_limits","plan_type":"plus","rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`
const codexControlTimingEvent = `{"type":"responsesapi.websocket_timing","ttft_ms":12.5}`
const codexControlOutputEvent = `{"type":"response.output_text.delta","delta":"hello"}`
const codexControlCompletedEvent = `{"type":"response.completed","response":{"id":"resp_control","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`

func TestCodexConsumesUpstreamControlEvent(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-control", Provider: "codex"}
	updates := make(chan []cliproxyauth.RateLimitSnapshot, 1)
	ctx := cliproxyauth.WithRateLimitUpdateCallback(context.Background(), func(_ context.Context, authID string, snapshots []cliproxyauth.RateLimitSnapshot) {
		if authID != auth.ID {
			t.Errorf("auth ID = %q, want %q", authID, auth.ID)
			return
		}
		updates <- snapshots
	})

	if !codexConsumesUpstreamControlEvent(ctx, auth, []byte(codexControlRateLimitEvent)) {
		t.Fatal("rate-limit event was not consumed")
	}
	select {
	case snapshots := <-updates:
		if len(snapshots) != 1 || snapshots[0].Primary == nil || snapshots[0].Primary.UsedPercent != 42 {
			t.Fatalf("rate-limit snapshots = %#v", snapshots)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rate-limit update")
	}

	if !codexConsumesUpstreamControlEvent(ctx, auth, []byte(codexControlTimingEvent)) {
		t.Fatal("websocket timing event was not consumed")
	}
	if codexConsumesUpstreamControlEvent(ctx, auth, []byte(codexControlOutputEvent)) {
		t.Fatal("ordinary Responses event must remain transparent")
	}
}

func TestCodexHTTPStreamConsumesControlEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			codexControlTimingEvent,
			codexControlRateLimitEvent,
			codexControlOutputEvent,
			codexControlCompletedEvent,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer server.Close()

	result, updates := executeCodexControlHTTPStream(t, server.URL)
	got := collectCodexControlStream(t, result)
	assertCodexControlStream(t, got)
	assertCodexControlRateLimitUpdate(t, updates)
}

func TestCodexWebsocketStreamConsumesControlEvents(t *testing.T) {
	server := newCodexControlWebsocketServer(t)
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := codexControlAuth(server.URL)
	updates := make(chan []cliproxyauth.RateLimitSnapshot, 1)
	ctx := codexControlRateLimitContext(updates)
	result, err := executor.ExecuteStream(ctx, auth, codexControlRequest(), codexControlOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	got := collectCodexControlStream(t, result)
	assertCodexControlStream(t, got)
	assertCodexControlRateLimitUpdate(t, updates)
}

func TestCodexWebsocketExecuteConsumesControlEvents(t *testing.T) {
	server := newCodexControlWebsocketServer(t)
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := codexControlAuth(server.URL)
	updates := make(chan []cliproxyauth.RateLimitSnapshot, 1)
	ctx := codexControlRateLimitContext(updates)
	resp, err := executor.Execute(ctx, auth, codexControlRequest(), codexControlOptions(false))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "resp_control" {
		t.Fatalf("response id = %q, want resp_control; payload=%s", got, resp.Payload)
	}
	assertCodexControlRateLimitUpdate(t, updates)
}

func executeCodexControlHTTPStream(t *testing.T, baseURL string) (*cliproxyexecutor.StreamResult, <-chan []cliproxyauth.RateLimitSnapshot) {
	t.Helper()
	updates := make(chan []cliproxyauth.RateLimitSnapshot, 1)
	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(codexControlRateLimitContext(updates), codexControlAuth(baseURL), codexControlRequest(), codexControlOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	return result, updates
}

func newCodexControlWebsocketServer(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}
		for _, event := range []string{
			codexControlTimingEvent,
			codexControlRateLimitEvent,
			codexControlOutputEvent,
			codexControlCompletedEvent,
		} {
			if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				t.Errorf("WriteMessage() error = %v", err)
				return
			}
		}
	}))
}

func codexControlAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "auth-control",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": baseURL,
		},
	}
}

func codexControlRateLimitContext(updates chan<- []cliproxyauth.RateLimitSnapshot) context.Context {
	return cliproxyauth.WithRateLimitUpdateCallback(context.Background(), func(_ context.Context, authID string, snapshots []cliproxyauth.RateLimitSnapshot) {
		if authID == "auth-control" {
			updates <- snapshots
		}
	})
}

func codexControlRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}
}

func codexControlOptions(stream bool) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       stream,
	}
}

func collectCodexControlStream(t *testing.T, result *cliproxyexecutor.StreamResult) []byte {
	t.Helper()
	if result == nil {
		t.Fatal("stream result is nil")
	}
	var got bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	return got.Bytes()
}

func assertCodexControlStream(t *testing.T, payload []byte) {
	t.Helper()
	text := string(payload)
	for _, eventType := range []string{codexEventRateLimits, codexEventResponsesWebsocketTiming} {
		if strings.Contains(text, eventType) {
			t.Fatalf("stream leaked internal event %q: %s", eventType, text)
		}
	}
	if !strings.Contains(text, "response.output_text.delta") || !strings.Contains(text, "response.completed") {
		t.Fatalf("stream missed ordinary Responses events: %s", text)
	}
}

func assertCodexControlRateLimitUpdate(t *testing.T, updates <-chan []cliproxyauth.RateLimitSnapshot) {
	t.Helper()
	select {
	case snapshots := <-updates:
		if len(snapshots) != 1 || snapshots[0].Primary == nil || snapshots[0].Primary.UsedPercent != 42 {
			t.Fatalf("rate-limit snapshots = %#v", snapshots)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rate-limit update")
	}
}
