package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type codexRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type codexUnexpectedEOFReadCloser struct {
	reader *strings.Reader
}

func (r *codexUnexpectedEOFReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *codexUnexpectedEOFReadCloser) Close() error { return nil }

func newCodexUnexpectedEOFBody(data string) io.ReadCloser {
	return &codexUnexpectedEOFReadCloser{reader: strings.NewReader(data)}
}

const codexCompletedAfterOutputItemDoneSSE = "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"

func TestCodexExecutorExecute_EmptyStreamCompletionOutputUsesOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"Say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	gotContent := gjson.GetBytes(resp.Payload, "choices.0.message.content").String()
	if gotContent != "ok" {
		t.Fatalf("choices.0.message.content = %q, want %q; payload=%s", gotContent, "ok", string(resp.Payload))
	}
}

func TestCodexExecutorExecute_IgnoresUnexpectedEOFAfterCompleted(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": "https://codex.test/backend-api/codex",
		"api_key":  "test",
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       newCodexUnexpectedEOFBody(codexCompletedAfterOutputItemDoneSSE),
			Request:    req,
		}, nil
	})))

	resp, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"Say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	gotContent := gjson.GetBytes(resp.Payload, "choices.0.message.content").String()
	if gotContent != "ok" {
		t.Fatalf("choices.0.message.content = %q, want %q; payload=%s", gotContent, "ok", string(resp.Payload))
	}
}

func TestCodexExecutorExecuteSurfacesTerminalStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}` + "\n\n"))
		_, _ = w.Write([]byte("event: response.failed\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err == nil {
		t.Fatal("expected terminal stream error, got nil")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_too_large")
	if !strings.Contains(err.Error(), "Your input exceeds the context window") {
		t.Fatalf("error message missing upstream context text: %v", err)
	}
}

func TestCodexExecutorExecuteStreamSurfacesTerminalStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("missing stream terminal error")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, streamErr)
	}
	assertCodexErrorCode(t, streamErr.Error(), "invalid_request_error", "context_too_large")
}

func TestCodexTerminalStreamContextLengthErrFromResponseFailed(t *testing.T) {
	err, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}`))
	if !ok {
		t.Fatal("expected context length terminal error")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_too_large")
}

func TestCodexTerminalStreamContextLengthErrFromTopLevelError(t *testing.T) {
	err, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","sequence_number":2}`))
	if !ok {
		t.Fatal("expected top-level context length terminal error")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_too_large")
	if !strings.Contains(err.Error(), "Your input exceeds the context window") {
		t.Fatalf("error message missing upstream context text: %v", err)
	}
}

func TestCodexTerminalStreamContextLengthErrIgnoresOtherTerminalErrors(t *testing.T) {
	_, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}`))
	if ok {
		t.Fatal("rate limit terminal error should not be handled by context length fix")
	}
}

func TestParseCodexStreamTerminalErrorMapsResponseFailedCodes(t *testing.T) {
	tests := []struct {
		name       string
		errorJSON  string
		wantStatus int
		wantText   string
	}{
		{
			name:       "invalid prompt",
			errorJSON:  `{"code":"invalid_prompt","message":"Invalid prompt: blocked."}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "Invalid prompt: blocked.",
		},
		{
			name:       "cyber policy fallback",
			errorJSON:  `{"code":"cyber_policy","message":"   "}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "possible cybersecurity risk",
		},
		{
			name:       "server overloaded",
			errorJSON:  `{"code":"server_is_overloaded","message":"server overloaded"}`,
			wantStatus: http.StatusServiceUnavailable,
			wantText:   "server overloaded",
		},
		{
			name:       "quota",
			errorJSON:  `{"code":"insufficient_quota","message":"quota exceeded"}`,
			wantStatus: http.StatusTooManyRequests,
			wantText:   "quota exceeded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event := []byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":` + tc.errorJSON + `}}`)

			err, ok := parseCodexStreamTerminalError("response.failed", event)
			if !ok {
				t.Fatal("expected terminal error")
			}
			if got := statusCodeFromTestError(t, err); got != tc.wantStatus {
				t.Fatalf("status code = %d, want %d; err=%v", got, tc.wantStatus, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantText)
			}
		})
	}
}

func statusCodeFromTestError(t *testing.T, err error) int {
	t.Helper()

	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode(): %v", err, err)
	}
	return statusErr.StatusCode()
}

func TestCodexExecutorExecuteStream_EmptyStreamCompletionOutputUsesOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var completed []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if !bytes.HasPrefix(payload, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(payload[5:])
		if gjson.GetBytes(data, "type").String() == "response.completed" {
			completed = append([]byte(nil), data...)
		}
	}

	if len(completed) == 0 {
		t.Fatal("missing response.completed chunk")
	}

	gotContent := gjson.GetBytes(completed, "response.output.0.content.0.text").String()
	if gotContent != "ok" {
		t.Fatalf("response.output[0].content[0].text = %q, want %q; completed=%s", gotContent, "ok", string(completed))
	}
}

func TestCodexExecutorExecuteStreamSuppressesUsageWarningBeforeForwarding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","item_id":"msg-warning","delta":"` + codexUsageLimitHeadsUpText + `"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},"output_index":0}` + "\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1775555723,"status":"completed","model":"gpt-5.4-mini-2026-03-17","output":[],"usage":{"input_tokens":8,"output_tokens":28,"total_tokens":36}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var received bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		_, _ = received.Write(chunk.Payload)
	}

	if strings.Contains(received.String(), "5h limit left") {
		t.Fatalf("usage warning leaked to downstream stream: %s", received.String())
	}
	if !strings.Contains(received.String(), "ok") {
		t.Fatalf("normal assistant output missing from downstream stream: %s", received.String())
	}
}

func TestCodexExecutorExecuteStreamSuppressesSplitUsageWarningBeforeForwarding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range []string{
			`\u26a0 Heads up, you have `,
			`less than 10% of your `,
			`5h limit left. Run /status for a breakdown.`,
		} {
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-warning\",\"delta\":\"%s\"}\n\n", delta)
		}
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},"output_index":0}` + "\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1775555723,"status":"completed","model":"gpt-5.4-mini-2026-03-17","output":[],"usage":{"input_tokens":8,"output_tokens":28,"total_tokens":36}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var received bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		_, _ = received.Write(chunk.Payload)
	}

	if strings.Contains(received.String(), "Heads up") || strings.Contains(received.String(), "5h limit left") {
		t.Fatalf("split usage warning leaked to downstream stream: %s", received.String())
	}
	if !strings.Contains(received.String(), "ok") {
		t.Fatalf("normal assistant output missing from downstream stream: %s", received.String())
	}
}

func TestCodexExecutorExecuteStream_IgnoresUnexpectedEOFAfterCompleted(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": "https://codex.test/backend-api/codex",
		"api_key":  "test",
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       newCodexUnexpectedEOFBody(codexCompletedAfterOutputItemDoneSSE),
			Request:    req,
		}, nil
	})))

	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var completed []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if !bytes.HasPrefix(payload, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(payload[5:])
		if gjson.GetBytes(data, "type").String() == "response.completed" {
			completed = append([]byte(nil), data...)
		}
	}

	if len(completed) == 0 {
		t.Fatal("missing response.completed chunk")
	}
	gotContent := gjson.GetBytes(completed, "response.output.0.content.0.text").String()
	if gotContent != "ok" {
		t.Fatalf("response.output[0].content[0].text = %q, want %q; completed=%s", gotContent, "ok", string(completed))
	}
}

func TestCodexExecutorExecuteStreamRetriesWithoutStaleTurnState(t *testing.T) {
	var attempts int
	seenTurnState := make([]string, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		turnState := r.Header.Get(codexHeaderTurnState)
		seenTurnState = append(seenTurnState, turnState)
		w.Header().Set("Content-Type", "text/event-stream")
		switch attempts {
		case 1:
			if turnState != "turn-state-1" {
				t.Fatalf("first %s = %q, want turn-state-1", codexHeaderTurnState, turnState)
			}
			_, _ = w.Write([]byte("data: {\"type\":\"error\",\"status\":400,\"error\":{\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp_1' not found.\",\"param\":\"previous_response_id\",\"type\":\"invalid_request_error\"}}\n\n"))
		case 2:
			if turnState != "" {
				t.Fatalf("retry %s = %q, want empty", codexHeaderTurnState, turnState)
			}
			_, _ = w.Write([]byte(codexCompletedAfterOutputItemDoneSSE))
		default:
			t.Fatalf("unexpected attempt %d", attempts)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderTurnState: "turn-state-1",
	})

	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var completed []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if !bytes.HasPrefix(payload, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(payload[5:])
		if gjson.GetBytes(data, "type").String() == "response.completed" {
			completed = append([]byte(nil), data...)
		}
	}

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(seenTurnState) != 2 || seenTurnState[0] != "turn-state-1" || seenTurnState[1] != "" {
		t.Fatalf("unexpected turn states: %v", seenTurnState)
	}
	if len(completed) == 0 {
		t.Fatal("missing response.completed chunk after retry")
	}
}

func TestCodexExecutorExecuteStreamSurfacesEOFBeforeCompleted(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": "https://codex.test/backend-api/codex",
		"api_key":  "test",
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n")),
			Request:    req,
		}, nil
	})))

	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("expected stream error before response.completed")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusRequestTimeout {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusRequestTimeout, streamErr)
	}
	if !strings.Contains(streamErr.Error(), "response.completed") {
		t.Fatalf("error should mention response.completed, got %v", streamErr)
	}
}

func TestCodexExecutorExecuteStreamDrainsBrieflyWhenContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < helps.StreamChunkBufferSize*2; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"%d\"}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{
		SDKConfig: config.SDKConfig{
			Streaming: config.StreamingConfig{
				UpstreamDrainAfterDownstreamCancelMS: 20,
			},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	ctx, cancel := context.WithCancel(context.Background())
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	cancel()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-result.Chunks:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream did not stop after context cancellation")
		}
	}
}

func TestCodexExecutorExecuteStream_ResponseFailedBeforePayloadReturnsStatusErrorChunk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"You've hit your usage limit. Upgrade to Plus to continue using Codex.\",\"resets_in_seconds\":30}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	chunk, ok := <-result.Chunks
	if !ok {
		t.Fatal("expected first chunk")
	}
	if chunk.Err == nil {
		t.Fatalf("expected error chunk, got payload=%q", string(chunk.Payload))
	}
	statusProvider, ok := chunk.Err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status provider, got %T", chunk.Err)
	}
	if got := statusProvider.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestCodexExecutorExecute_ResponseFailedAggregateReturnsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"error\":{\"message\":\"You've hit your usage limit. Upgrade to Plus to continue using Codex.\",\"resets_in_seconds\":30}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"Say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err == nil {
		t.Fatal("expected Execute error")
	}

	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status provider, got %T", err)
	}
	if got := statusProvider.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestPatchCodexCompletedOutputRecoversFunctionCall(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`))
	streamState.recordEvent([]byte(`{"type":"response.function_call_arguments.done","item_id":"fc_item_1","output_index":0,"arguments":"{\"q\":\"hello\"}"}`))

	patched, recoveredCount := streamState.patchCompletedOutputIfEmpty([]byte(`{"response":{"output":[]}}`))
	if recoveredCount != 1 {
		t.Fatalf("recovered count = %d, want %d", recoveredCount, 1)
	}
	if got := gjson.GetBytes(patched, "response.output.0.type").String(); got != "function_call" {
		t.Fatalf("response.output.0.type = %q, want %q", got, "function_call")
	}
	if got := gjson.GetBytes(patched, "response.output.0.call_id").String(); got != "call_1" {
		t.Fatalf("response.output.0.call_id = %q, want %q", got, "call_1")
	}
	if got := gjson.GetBytes(patched, "response.output.0.arguments").String(); got != `{"q":"hello"}` {
		t.Fatalf("response.output.0.arguments = %q, want %q", got, `{"q":"hello"}`)
	}
}

func TestPatchCodexCompletedOutputRecoversFunctionCallDeltaArguments(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`))
	streamState.recordEvent([]byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"{\"q\":"}`))
	streamState.recordEvent([]byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"\"hello\"}"}`))

	patched, recoveredCount := streamState.patchCompletedOutputIfEmpty([]byte(`{"response":{"output":[]}}`))
	if recoveredCount != 1 {
		t.Fatalf("recovered count = %d, want %d", recoveredCount, 1)
	}
	if got := gjson.GetBytes(patched, "response.output.0.arguments").String(); got != `{"q":"hello"}` {
		t.Fatalf("response.output.0.arguments = %q, want %q", got, `{"q":"hello"}`)
	}
}

func TestCodexEventTypeUsesTopLevelType(t *testing.T) {
	eventData := []byte(`{"nested":{"type":"response.completed"},"type":"response.function_call_arguments.delta"}`)
	if got := codexEventType(eventData); got != codexEventFunctionCallArgumentsDelta {
		t.Fatalf("codexEventType() = %q, want %q", got, codexEventFunctionCallArgumentsDelta)
	}

	if got := codexEventType([]byte(`{"nested":{"type":"response.completed"}}`)); got != "" {
		t.Fatalf("codexEventType() for nested-only type = %q, want empty", got)
	}

	unknownType := "response.function_call_arguments.delta.invalid"
	if got := codexEventType([]byte(`{"type":"response.function_call_arguments.delta.invalid","delta":"chunk"}`)); got != unknownType {
		t.Fatalf("codexEventType() = %q, want %q", got, unknownType)
	}
}

func TestCodexStreamArgumentDeltaRecordsEscapedWhitespacePayload(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`))
	streamState.recordEventWithType(codexEventFunctionCallArgumentsDelta, []byte(`{ "output_index" : 0, "delta" : "{\"q\":\"hello world\"}" }`))

	if got := streamState.functionCallsByItem["fc_item_1"].arguments(); got != `{"q":"hello world"}` {
		t.Fatalf("arguments = %q, want escaped JSON payload", got)
	}
}

func TestCodexStreamArgumentDeltaRecordsCompactSequenceNumberPayload(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`))
	streamState.recordEvent([]byte(`{"type":"response.function_call_arguments.delta","sequence_number":42,"item_id":"fc_item_1","output_index":0,"delta":"{\"q\":\"hello world\"}"}`))
	streamState.recordEvent([]byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"\n","sequence_number":43}`))

	if got := streamState.functionCallsByItem["fc_item_1"].arguments(); got != "{\"q\":\"hello world\"}\n" {
		t.Fatalf("arguments = %q, want escaped JSON payload", got)
	}
}

func TestCodexStreamArgumentDeltaFallsBackToItemIDForUnknownOutputIndex(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`))
	streamState.recordEvent([]byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":99,"delta":"chunk"}`))

	if got := streamState.functionCallsByItem["fc_item_1"].arguments(); got != "chunk" {
		t.Fatalf("arguments = %q, want fallback item_id lookup", got)
	}
}

func BenchmarkCodexEventTypeFirstField(b *testing.B) {
	eventData := []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"chunk"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := codexEventType(eventData); got != codexEventFunctionCallArgumentsDelta {
			b.Fatalf("codexEventType() = %q", got)
		}
	}
}

func BenchmarkCodexEventTypeOutputTextFirstField(b *testing.B) {
	eventData := []byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"chunk"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := codexEventType(eventData); got != "response.output_text.delta" {
			b.Fatalf("codexEventType() = %q", got)
		}
	}
}

func BenchmarkCodexStreamTextOnlyCompletion(b *testing.B) {
	deltaEvent := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	completedEvent := []byte(`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		streamState := newCodexStreamCompletionState()
		for j := 0; j < 64; j++ {
			streamState.recordEventWithType("response.output_text.delta", deltaEvent)
		}
		completed, ok := streamState.processEventDataWithType(codexEventCompleted, completedEvent, true)
		if !ok || len(completed.data) == 0 {
			b.Fatal("missing completed event")
		}
	}
}

func TestPatchCodexCompletedOutputRecoversCustomToolCallInputDeltas(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":""}}`))
	streamState.recordEvent([]byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","call_id":"call_1","output_index":0,"delta":"*** Begin Patch\n"}`))
	streamState.recordEvent([]byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","call_id":"call_1","output_index":0,"delta":"*** End Patch"}`))

	patched, recoveredCount := streamState.patchCompletedOutputIfEmpty([]byte(`{"response":{"output":[]}}`))
	if recoveredCount != 1 {
		t.Fatalf("recovered count = %d, want %d", recoveredCount, 1)
	}
	if got := gjson.GetBytes(patched, "response.output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("response.output.0.type = %q, want %q; body=%s", got, "custom_tool_call", patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.input").String(); got != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("response.output.0.input = %q; body=%s", got, patched)
	}
	if gjson.GetBytes(patched, "response.output.0.arguments").Exists() {
		t.Fatalf("custom_tool_call should recover input, not arguments; body=%s", patched)
	}
}

func TestPatchCodexCompletedOutputRecoversCustomToolCallByCallID(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_apply","name":"apply_patch","input":""}}`))
	streamState.recordEvent([]byte(`{"type":"response.custom_tool_call_input.delta","call_id":"call_apply","delta":"*** Begin Patch\n"}`))
	streamState.recordEvent([]byte(`{"type":"response.custom_tool_call_input.delta","call_id":"call_apply","delta":"+hello\n*** End Patch"}`))

	patched, recoveredCount := streamState.patchCompletedOutputIfEmpty([]byte(`{"response":{"output":[]}}`))
	if recoveredCount != 1 {
		t.Fatalf("recovered count = %d, want %d", recoveredCount, 1)
	}
	if got := gjson.GetBytes(patched, "response.output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("response.output.0.type = %q, want custom_tool_call; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.call_id").String(); got != "call_apply" {
		t.Fatalf("response.output.0.call_id = %q, want call_apply; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.input").String(); got != "*** Begin Patch\n+hello\n*** End Patch" {
		t.Fatalf("response.output.0.input = %q; body=%s", got, patched)
	}
}

func TestPatchCodexCompletedOutputRecoversLocalShellCallFromAdded(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"lsc_1","type":"local_shell_call","call_id":"call_shell","status":"in_progress","action":{"type":"exec","command":["pwd"],"timeout_ms":1000}}}`))

	patched, recoveredCount := streamState.patchCompletedOutputIfEmpty([]byte(`{"response":{"output":[]}}`))
	if recoveredCount != 1 {
		t.Fatalf("recovered count = %d, want %d", recoveredCount, 1)
	}
	if got := gjson.GetBytes(patched, "response.output.0.type").String(); got != "local_shell_call" {
		t.Fatalf("response.output.0.type = %q, want local_shell_call; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.call_id").String(); got != "call_shell" {
		t.Fatalf("response.output.0.call_id = %q, want call_shell; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.status").String(); got != "completed" {
		t.Fatalf("response.output.0.status = %q, want completed; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.action.command.0").String(); got != "pwd" {
		t.Fatalf("response.output.0.action.command.0 = %q, want pwd; body=%s", got, patched)
	}
}

func TestPatchCodexCompletedOutputRecoversToolSearchCallFromAdded(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"tsc_1","type":"tool_search_call","call_id":"search_1","status":"completed","execution":"client","arguments":{"query":"calendar","limit":1}}}`))

	patched, recoveredCount := streamState.patchCompletedOutputIfEmpty([]byte(`{"response":{"output":[]}}`))
	if recoveredCount != 1 {
		t.Fatalf("recovered count = %d, want %d", recoveredCount, 1)
	}
	if got := gjson.GetBytes(patched, "response.output.0.type").String(); got != "tool_search_call" {
		t.Fatalf("response.output.0.type = %q, want tool_search_call; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.call_id").String(); got != "search_1" {
		t.Fatalf("response.output.0.call_id = %q, want search_1; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.execution").String(); got != "client" {
		t.Fatalf("response.output.0.execution = %q, want client; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.arguments.query").String(); got != "calendar" {
		t.Fatalf("response.output.0.arguments.query = %q, want calendar; body=%s", got, patched)
	}
}

func TestPatchCodexCompletedOutputRecoversServerToolSearchCallWithoutCallID(t *testing.T) {
	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"tool_search_call","call_id":null,"status":"completed","execution":"server","arguments":{"paths":["crm"]}}}`))

	patched, recoveredCount := streamState.patchCompletedOutputIfEmpty([]byte(`{"response":{"output":[]}}`))
	if recoveredCount != 1 {
		t.Fatalf("recovered count = %d, want %d", recoveredCount, 1)
	}
	if got := gjson.GetBytes(patched, "response.output.0.type").String(); got != "tool_search_call" {
		t.Fatalf("response.output.0.type = %q, want tool_search_call; body=%s", got, patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.execution").String(); got != "server" {
		t.Fatalf("response.output.0.execution = %q, want server; body=%s", got, patched)
	}
	if gjson.GetBytes(patched, "response.output.0.call_id").Exists() {
		t.Fatalf("server tool_search_call without call_id should not synthesize one; body=%s", patched)
	}
	if got := gjson.GetBytes(patched, "response.output.0.arguments.paths.0").String(); got != "crm" {
		t.Fatalf("response.output.0.arguments.paths.0 = %q, want crm; body=%s", got, patched)
	}
}

func BenchmarkCodexStreamFunctionCallArgumentDeltas(b *testing.B) {
	addedEvent := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`)
	deltaEvent := []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"chunk"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		streamState := newCodexStreamCompletionState()
		streamState.recordEvent(addedEvent)
		for j := 0; j < 512; j++ {
			streamState.recordEvent(deltaEvent)
		}
		if got := streamState.functionCallsByItem["fc_item_1"].arguments(); len(got) == 0 {
			b.Fatal("arguments are empty")
		}
	}
}

func BenchmarkCodexStreamFunctionCallArgumentDeltasWithSequenceNumber(b *testing.B) {
	addedEvent := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`)
	deltaEvent := []byte(`{"type":"response.function_call_arguments.delta","sequence_number":42,"item_id":"fc_item_1","output_index":0,"delta":"chunk"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		streamState := newCodexStreamCompletionState()
		streamState.recordEvent(addedEvent)
		for j := 0; j < 512; j++ {
			streamState.recordEvent(deltaEvent)
		}
		if got := streamState.functionCallsByItem["fc_item_1"].arguments(); len(got) == 0 {
			b.Fatal("arguments are empty")
		}
	}
}

func BenchmarkCodexStreamFunctionCallSingleArgumentDelta(b *testing.B) {
	addedEvent := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`)
	deltaEvent := []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"chunk"}`)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		streamState := newCodexStreamCompletionState()
		streamState.recordEvent(addedEvent)
		streamState.recordEvent(deltaEvent)
		if got := streamState.functionCallsByItem["fc_item_1"].arguments(); len(got) == 0 {
			b.Fatal("arguments are empty")
		}
	}
}

func BenchmarkPatchCodexCompletedOutputIfEmptySingleOutputItem(b *testing.B) {
	b.ReportAllocs()

	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},"output_index":0}`))
	completedData := []byte(`{"response":{"id":"resp_1","output":[]}}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		patched, recoveredCount := streamState.patchCompletedOutputIfEmpty(completedData)
		if recoveredCount != 1 || len(patched) == 0 {
			b.Fatalf("recoveredCount=%d len=%d", recoveredCount, len(patched))
		}
	}
}

func BenchmarkPatchCodexCompletedOutputIfEmptyFunctionCallRecovery(b *testing.B) {
	b.ReportAllocs()

	streamState := newCodexStreamCompletionState()
	streamState.recordEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_1","name":"search"}}`))
	streamState.recordEvent([]byte(`{"type":"response.function_call_arguments.done","item_id":"fc_item_1","output_index":0,"arguments":"{\"q\":\"hello\"}"}`))
	completedData := []byte(`{"response":{"id":"resp_1","output":[]}}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		patched, recoveredCount := streamState.patchCompletedOutputIfEmpty(completedData)
		if recoveredCount != 1 || len(patched) == 0 {
			b.Fatalf("recoveredCount=%d len=%d", recoveredCount, len(patched))
		}
	}
}

func TestCollectCodexResponseAggregatePatchesCompletedOutputButKeepsCapturedBody(t *testing.T) {
	stream := strings.NewReader(
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n",
	)

	result, err := collectCodexResponseAggregate(stream, true)
	if err != nil {
		t.Fatalf("collectCodexResponseAggregate() error = %v", err)
	}
	if got := gjson.GetBytes(result.completedData, "response.output.0.content.0.text").String(); got != "ok" {
		t.Fatalf("patched completed output text = %q, want %q", got, "ok")
	}
	if !strings.Contains(string(result.body), `"response":{"id":"resp_1","output":[]}`) {
		t.Fatalf("captured body did not preserve original completed event: %s", string(result.body))
	}
}
