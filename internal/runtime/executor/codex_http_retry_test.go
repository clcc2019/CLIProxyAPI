package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

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
	if codexShouldRetryStreamRead(context.Background(), err, false, false, nil, false, codexHTTPMaxRequestRetries) {
		t.Fatal("must not retry after max attempts")
	}
}
