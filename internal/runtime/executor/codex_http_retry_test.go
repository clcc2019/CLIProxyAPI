package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestDoCodexHTTPRequestRetriesZstdEOFWithoutCompression(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct_123"},
	}

	var attempts int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		gotBody, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("ReadAll(request body) error = %v", errRead)
		}
		switch attempts {
		case 1:
			if got := req.Header.Get("Content-Encoding"); got != "zstd" {
				t.Fatalf("first Content-Encoding = %q, want zstd", got)
			}
			if bytes.Equal(gotBody, body) {
				t.Fatalf("first request body should be compressed")
			}
			return nil, io.EOF
		case 2:
			if got := req.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("retry Content-Encoding = %q, want empty", got)
			}
			if got := req.ContentLength; got != int64(len(body)) {
				t.Fatalf("retry ContentLength = %d, want %d", got, len(body))
			}
			if len(req.TransferEncoding) != 0 {
				t.Fatalf("retry TransferEncoding = %#v, want empty", req.TransferEncoding)
			}
			if req.GetBody == nil {
				t.Fatalf("retry GetBody is nil")
			}
			resetBody, errGetBody := req.GetBody()
			if errGetBody != nil {
				t.Fatalf("retry GetBody() error = %v", errGetBody)
			}
			resetBytes, errReadReset := io.ReadAll(resetBody)
			if errCloseReset := resetBody.Close(); errCloseReset != nil {
				t.Fatalf("retry GetBody Close() error = %v", errCloseReset)
			}
			if errReadReset != nil {
				t.Fatalf("ReadAll(retry GetBody) error = %v", errReadReset)
			}
			if !bytes.Equal(resetBytes, body) {
				t.Fatalf("retry GetBody bytes = %s, want %s", resetBytes, body)
			}
			if !bytes.Equal(gotBody, body) {
				t.Fatalf("retry body = %s, want %s", gotBody, body)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		default:
			t.Fatalf("unexpected attempt %d", attempts)
			return nil, nil
		}
	})))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := maybeEnableCodexRequestCompressionWithConfig(req, auth, nil, body); err != nil {
		t.Fatalf("maybeEnableCodexRequestCompressionWithConfig() error = %v", err)
	}

	resp, err := (&CodexExecutor{}).doCodexHTTPRequest(ctx, auth, codexPreparedRequest{httpReq: req, body: body})
	if err != nil {
		t.Fatalf("doCodexHTTPRequest() error = %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, want 200", resp)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestCodexHTTPRequestForAttemptTreatsZstdEncodingCaseInsensitively(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex"}`)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Encoding", "ZsTd")

	retryReq, err := codexHTTPRequestForAttempt(codexPreparedRequest{
		httpReq: req,
		body:    body,
	}, "ZsTd", 1)
	if err != nil {
		t.Fatalf("codexHTTPRequestForAttempt() error = %v", err)
	}
	if got := retryReq.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("retry Content-Encoding = %q, want empty", got)
	}
}

func TestDoCodexHTTPRequestRetriesMixedCaseZstdStatusWithoutCompression(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct_123"},
	}

	var attempts int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		gotBody, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("ReadAll(request body) error = %v", errRead)
		}
		switch attempts {
		case 1:
			if got := req.Header.Get("Content-Encoding"); got != "ZsTd" {
				t.Fatalf("first Content-Encoding = %q, want ZsTd", got)
			}
			if !bytes.Equal(gotBody, []byte("compressed-body")) {
				t.Fatalf("first body = %s, want compressed-body", gotBody)
			}
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"temporary"}}`)),
			}, nil
		case 2:
			if got := req.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("retry Content-Encoding = %q, want empty", got)
			}
			if !bytes.Equal(gotBody, body) {
				t.Fatalf("retry body = %s, want %s", gotBody, body)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		default:
			t.Fatalf("unexpected attempt %d", attempts)
			return nil, nil
		}
	})))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader([]byte("compressed-body")))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Encoding", "ZsTd")
	codexResetRequestBody(req, []byte("compressed-body"))

	resp, err := (&CodexExecutor{}).doCodexHTTPRequest(ctx, auth, codexPreparedRequest{httpReq: req, body: body})
	if err != nil {
		t.Fatalf("doCodexHTTPRequest() error = %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, want 200", resp)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func BenchmarkCodexHTTPEncodingIsZstd(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if !codexHTTPEncodingIsZstd(" ZsTd ") {
			b.Fatal("expected zstd encoding")
		}
	}
}

func TestDoCodexHTTPRequestRetriesZstdRejectionWithoutCompression(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct_123"},
	}

	var attempts int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		gotBody, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("ReadAll(request body) error = %v", errRead)
		}
		switch attempts {
		case 1:
			if got := req.Header.Get("Content-Encoding"); got != "zstd" {
				t.Fatalf("first Content-Encoding = %q, want zstd", got)
			}
			if bytes.Equal(gotBody, body) {
				t.Fatalf("first request body should be compressed")
			}
			return &http.Response{
				StatusCode: http.StatusUnsupportedMediaType,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"unsupported content encoding"}}`)),
			}, nil
		case 2:
			if got := req.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("retry Content-Encoding = %q, want empty", got)
			}
			if !bytes.Equal(gotBody, body) {
				t.Fatalf("retry body = %s, want %s", gotBody, body)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		default:
			t.Fatalf("unexpected attempt %d", attempts)
			return nil, nil
		}
	})))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := maybeEnableCodexRequestCompressionWithConfig(req, auth, nil, body); err != nil {
		t.Fatalf("maybeEnableCodexRequestCompressionWithConfig() error = %v", err)
	}

	resp, err := (&CodexExecutor{}).doCodexHTTPRequest(ctx, auth, codexPreparedRequest{httpReq: req, body: body})
	if err != nil {
		t.Fatalf("doCodexHTTPRequest() error = %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, want 200", resp)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestDoCodexHTTPRequestDoesNotRetryZstdApplicationBadRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct_123"},
	}
	errBody := `{"error":{"code":"previous_response_not_found","message":"Previous response with id 'resp_1' not found.","param":"previous_response_id","type":"invalid_request_error"}}`

	var attempts int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts != 1 {
			t.Fatalf("unexpected retry attempt %d", attempts)
		}
		if got := req.Header.Get("Content-Encoding"); got != "zstd" {
			t.Fatalf("Content-Encoding = %q, want zstd", got)
		}
		if _, errRead := io.ReadAll(req.Body); errRead != nil {
			t.Fatalf("ReadAll(request body) error = %v", errRead)
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(errBody)),
		}, nil
	})))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := maybeEnableCodexRequestCompressionWithConfig(req, auth, nil, body); err != nil {
		t.Fatalf("maybeEnableCodexRequestCompressionWithConfig() error = %v", err)
	}

	resp, err := (&CodexExecutor{}).doCodexHTTPRequest(ctx, auth, codexPreparedRequest{httpReq: req, body: body})
	if err != nil {
		t.Fatalf("doCodexHTTPRequest() error = %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("response = %#v, want 400", resp)
	}
	gotBody, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatalf("ReadAll(response body) error = %v", errRead)
	}
	if string(gotBody) != errBody {
		t.Fatalf("response body = %s, want %s", gotBody, errBody)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestCodexShouldRetryHTTPStatusWithoutCompressionSniffsBoundedPrefixAndPreservesBody(t *testing.T) {
	body := append([]byte(`{"error":{"message":"ordinary application bad request"}}`), bytes.Repeat([]byte("x"), helps.MaxErrorResponseBodyBytes+64)...)
	source := &codexCountingReadCloser{reader: bytes.NewReader(body)}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       source,
	}

	if codexShouldRetryHTTPStatusWithoutCompression(resp) {
		t.Fatal("ordinary application error should not be treated as zstd rejection")
	}
	if source.bytesRead > helps.MaxErrorResponseBodyBytes {
		t.Fatalf("sniff read %d bytes, want at most %d", source.bytesRead, helps.MaxErrorResponseBodyBytes)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(restored response body) error = %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("restored response body length = %d, want %d", len(got), len(body))
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("Close(restored response body) error = %v", errClose)
	}
	if !source.closed {
		t.Fatal("restored response body Close() did not close original body")
	}
}

func TestCodexShouldRetryHTTPStatusWithoutCompressionMatchesMixedCaseEncodingError(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Unsupported Content-Encoding ZSTD"}}`)),
	}

	if !codexShouldRetryHTTPStatusWithoutCompression(resp) {
		t.Fatal("mixed-case content-encoding rejection should retry without compression")
	}
}

func BenchmarkCodexShouldRetryHTTPStatusWithoutCompressionEncodingError(b *testing.B) {
	body := []byte(`{"error":{"message":"Unsupported Content-Encoding ZSTD"}}`)
	for b.Loop() {
		resp := &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(bytes.NewReader(body)),
		}
		if !codexShouldRetryHTTPStatusWithoutCompression(resp) {
			b.Fatal("expected retry without compression")
		}
		_ = resp.Body.Close()
	}
}

func TestCodexShouldRetryHTTPResponseWithoutTurnStateSniffsBoundedPrefixAndPreservesBody(t *testing.T) {
	body := append([]byte(`{"error":{"code":"unrelated","message":"ordinary application bad request"}}`), bytes.Repeat([]byte("x"), helps.MaxErrorResponseBodyBytes+64)...)
	source := &codexCountingReadCloser{reader: bytes.NewReader(body)}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       source,
	}
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set(codexHeaderTurnState, "turn-state-1")
	prepared := codexPreparedRequest{
		httpReq: req,
		body:    []byte(`{"model":"gpt-5-codex","input":"hello"}`),
	}

	if codexShouldRetryHTTPResponseWithoutTurnState(resp, prepared) {
		t.Fatal("unrelated application error should not retry without turn state")
	}
	if source.bytesRead > helps.MaxErrorResponseBodyBytes {
		t.Fatalf("sniff read %d bytes, want at most %d", source.bytesRead, helps.MaxErrorResponseBodyBytes)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(restored response body) error = %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("restored response body length = %d, want %d", len(got), len(body))
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("Close(restored response body) error = %v", errClose)
	}
	if !source.closed {
		t.Fatal("restored response body Close() did not close original body")
	}
}

func TestDoCodexHTTPRequestRetriesPreviousResponseNotFoundWithoutTurnState(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	errBody := `{"error":{"code":"previous_response_not_found","message":"Previous response with id 'resp_1' not found.","param":"previous_response_id","type":"invalid_request_error"}}`

	var attempts int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		gotBody, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("ReadAll(request body) error = %v", errRead)
		}
		if !bytes.Equal(gotBody, body) {
			t.Fatalf("attempt %d body = %s, want %s", attempts, gotBody, body)
		}
		switch attempts {
		case 1:
			if got := req.Header.Get(codexHeaderTurnState); got != "turn-state-1" {
				t.Fatalf("first %s = %q, want turn-state-1", codexHeaderTurnState, got)
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(errBody)),
			}, nil
		case 2:
			if got := req.Header.Get(codexHeaderTurnState); got != "" {
				t.Fatalf("retry %s = %q, want empty", codexHeaderTurnState, got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		default:
			t.Fatalf("unexpected attempt %d", attempts)
			return nil, nil
		}
	})))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(codexHeaderTurnState, "turn-state-1")

	resp, err := (&CodexExecutor{}).doCodexHTTPRequest(ctx, nil, codexPreparedRequest{httpReq: req, body: body})
	if err != nil {
		t.Fatalf("doCodexHTTPRequest() error = %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, want 200", resp)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if got := req.Header.Get(codexHeaderTurnState); got != "" {
		t.Fatalf("prepared request %s = %q, want empty after retry", codexHeaderTurnState, got)
	}
}

func TestDoCodexHTTPRequestRebuildsBodyAcrossSeparateCalls(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct_123"},
	}

	var attempts int
	var firstBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		gotBody, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("ReadAll(request body) error = %v", errRead)
		}
		if len(gotBody) == 0 {
			t.Fatalf("attempt %d sent empty body", attempts)
		}
		if got := req.Header.Get("Content-Encoding"); got != "zstd" {
			t.Fatalf("attempt %d Content-Encoding = %q, want zstd", attempts, got)
		}
		if bytes.Equal(gotBody, body) {
			t.Fatalf("attempt %d request body should be compressed", attempts)
		}
		if attempts == 1 {
			firstBody = append([]byte(nil), gotBody...)
		} else if !bytes.Equal(gotBody, firstBody) {
			t.Fatalf("attempt %d body differs from first compressed body", attempts)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	})))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := maybeEnableCodexRequestCompressionWithConfig(req, auth, nil, body); err != nil {
		t.Fatalf("maybeEnableCodexRequestCompressionWithConfig() error = %v", err)
	}

	prepared := codexPreparedRequest{httpReq: req, body: body}
	for i := 0; i < 2; i++ {
		resp, err := (&CodexExecutor{}).doCodexHTTPRequest(ctx, auth, prepared)
		if err != nil {
			t.Fatalf("doCodexHTTPRequest(%d) error = %v", i+1, err)
		}
		if resp == nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("response %d = %#v, want 200", i+1, resp)
		}
		if resp.Body != nil {
			if errClose := resp.Body.Close(); errClose != nil {
				t.Fatalf("response %d Close() error = %v", i+1, errClose)
			}
		}
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestDoCodexHTTPRequestRejectsNilPreparedRequest(t *testing.T) {
	_, err := (&CodexExecutor{}).doCodexHTTPRequest(context.Background(), nil, codexPreparedRequest{})
	if err == nil || !strings.Contains(err.Error(), "request is nil") {
		t.Fatalf("error = %v, want request is nil", err)
	}
}

func TestCodexShouldRetryHTTPTransportErrorHonorsCanceledParentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if codexShouldRetryHTTPTransportError(ctx, io.EOF) {
		t.Fatal("EOF should not retry after parent context is canceled")
	}
	if !codexShouldRetryHTTPTransportError(context.Background(), context.Canceled) {
		t.Fatal("context.Canceled from transport should retry when parent context is still active")
	}
}

func TestCodexShouldRetryHTTPTransportErrorMatchesMixedCaseMarker(t *testing.T) {
	err := errors.New("HTTP2: Stream Closed")
	if !codexShouldRetryHTTPTransportError(context.Background(), err) {
		t.Fatal("mixed-case HTTP/2 stream marker should be retryable")
	}
}

func BenchmarkCodexShouldRetryHTTPTransportErrorMarker(b *testing.B) {
	err := errors.New("HTTP2: Stream Closed")
	for b.Loop() {
		if !codexShouldRetryHTTPTransportError(context.Background(), err) {
			b.Fatal("expected retryable transport error")
		}
	}
}

func TestCodexRequestContextDoneRequiresCanceledParent(t *testing.T) {
	err := errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": context canceled`)
	if codexRequestContextDone(context.Background(), err) {
		t.Fatal("active parent context must not be treated as downstream cancellation")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !codexRequestContextDone(ctx, context.Canceled) {
		t.Fatal("canceled parent context should be treated as downstream cancellation")
	}
	if !codexRequestContextDone(ctx, err) {
		t.Fatal("canceled parent context should classify context-canceled POST errors as downstream cancellation")
	}
	mixedCaseErr := errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": Context Deadline Exceeded`)
	if !codexRequestContextDone(ctx, mixedCaseErr) {
		t.Fatal("canceled parent context should classify mixed-case context deadline errors as downstream cancellation")
	}
	if codexRequestContextDone(ctx, errors.New("stream error: stream ID 33; INTERNAL_ERROR; received from peer")) {
		t.Fatal("non-context transport errors must remain visible even after cancellation")
	}
}

func BenchmarkErrorsIsContextDoneText(b *testing.B) {
	err := errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": Context Deadline Exceeded`)
	for b.Loop() {
		if !errorsIsContextDone(err) {
			b.Fatal("expected context done error")
		}
	}
}

func TestCodexUpstreamDrainAfterDownstreamCancelConfig(t *testing.T) {
	if got := codexUpstreamDrainAfterDownstreamCancel(nil); got != defaultCodexUpstreamDrainAfterDownstreamCancel {
		t.Fatalf("nil config drain = %s, want %s", got, defaultCodexUpstreamDrainAfterDownstreamCancel)
	}
	if got := codexUpstreamDrainAfterDownstreamCancel(&config.Config{}); got != defaultCodexUpstreamDrainAfterDownstreamCancel {
		t.Fatalf("zero config drain = %s, want %s", got, defaultCodexUpstreamDrainAfterDownstreamCancel)
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{Streaming: config.StreamingConfig{UpstreamDrainAfterDownstreamCancelMS: 250}}}
	if got := codexUpstreamDrainAfterDownstreamCancel(cfg); got != 250*time.Millisecond {
		t.Fatalf("configured drain = %s, want 250ms", got)
	}
	cfg.Streaming.UpstreamDrainAfterDownstreamCancelMS = -1
	if got := codexUpstreamDrainAfterDownstreamCancel(cfg); got != 0 {
		t.Fatalf("disabled drain = %s, want 0", got)
	}
}

func TestCodexDetachUpstreamContextDoesNotDrainDeadline(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer parentCancel()

	upstreamCtx, release := codexDetachUpstreamContext(parent, &config.Config{
		SDKConfig: config.SDKConfig{
			Streaming: config.StreamingConfig{UpstreamDrainAfterDownstreamCancelMS: 1000},
		},
	})
	defer release()

	select {
	case <-upstreamCtx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("upstream context should cancel promptly on parent deadline")
	}
	if !errors.Is(upstreamCtx.Err(), context.Canceled) {
		t.Fatalf("upstream context err = %v, want context canceled", upstreamCtx.Err())
	}
}

func TestCodexShouldRetryStreamReadBeforePayloadOnHTTP2InternalError(t *testing.T) {
	err := errors.New("stream error: stream ID 33; INTERNAL_ERROR; received from peer")
	if !codexShouldRetryStreamRead(context.Background(), err, false, false, nil, false, 0) {
		t.Fatal("expected HTTP/2 internal stream read error before payload to be retryable")
	}
	if codexShouldRetryStreamRead(context.Background(), err, true, false, nil, false, 0) {
		t.Fatal("must not retry after payload was emitted")
	}
	if codexShouldRetryStreamRead(context.Background(), err, false, true, nil, false, 0) {
		t.Fatal("must not retry after response.completed was observed")
	}
	if !codexShouldRetryStreamRead(context.Background(), err, false, false, nil, false, codexHTTPMaxStreamReadRetries-1) {
		t.Fatal("must still retry before max stream attempts")
	}
	if codexShouldRetryStreamRead(context.Background(), err, false, false, nil, false, codexHTTPMaxStreamReadRetries) {
		t.Fatal("must not retry after max attempts")
	}
	if codexHTTPMaxStreamReadRetries != 5 {
		t.Fatalf("stream retry budget = %d, want official default 5", codexHTTPMaxStreamReadRetries)
	}
}

type codexCountingReadCloser struct {
	reader    *bytes.Reader
	bytesRead int
	closed    bool
}

func (r *codexCountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += n
	return n, err
}

func (r *codexCountingReadCloser) Close() error {
	r.closed = true
	return nil
}
