package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	codexHTTPMaxRequestRetries    = 4
	codexHTTPMaxStreamReadRetries = 5
	codexHTTPRetryBaseDelay       = 200 * time.Millisecond
)

var codexHTTPRetryableTransportMarkers = []string{
	"eof",
	"connection reset",
	"connection refused",
	"server closed idle connection",
	"use of closed network connection",
	"http2: stream closed",
	"http2: server sent goaway",
	"stream error",
	"internal_error",
}

func (e *CodexExecutor) doCodexHTTPRequest(ctx context.Context, auth *cliproxyauth.Auth, prepared codexPreparedRequest) (*http.Response, error) {
	if prepared.httpReq == nil {
		return nil, errors.New("codex executor: request is nil")
	}
	httpClient := helps.NewCodexHTTPClient(ctx, e.cfg, auth, 0)
	encoding := strings.ToLower(strings.TrimSpace(prepared.httpReq.Header.Get("Content-Encoding")))
	turnStateRetryUsed := false
	for attempt := 0; ; attempt++ {
		req, errBuild := codexHTTPRequestForAttempt(prepared, encoding, attempt)
		if errBuild != nil {
			return nil, errBuild
		}

		httpResp, err := httpClient.Do(req)
		if err == nil && encoding == "zstd" && attempt == 0 && codexShouldRetryHTTPStatusWithoutCompression(httpResp) {
			statusCode := 0
			if httpResp != nil {
				statusCode = httpResp.StatusCode
			}
			if httpResp != nil && httpResp.Body != nil {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close zstd rejection response body: %v", errClose)
				}
			}
			helps.LogWithRequestID(ctx).Debugf("codex executor: retrying HTTP request without zstd after status=%d", statusCode)
			continue
		}
		if err == nil && !turnStateRetryUsed && (encoding == "" || encoding == "zstd") && codexShouldRetryHTTPResponseWithoutTurnState(httpResp, prepared) {
			turnStateRetryUsed = true
			statusCode := 0
			if httpResp != nil {
				statusCode = httpResp.StatusCode
			}
			if httpResp != nil && httpResp.Body != nil {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close stale turn state response body: %v", errClose)
				}
			}
			e.dropCodexHTTPTurnStateForRetry(ctx, auth, prepared, "HTTP status", statusCode)
			continue
		}
		if err == nil && !codexShouldRetryHTTPStatus(httpResp) {
			return httpResp, nil
		}
		if attempt >= codexHTTPMaxRequestRetries {
			return httpResp, err
		}
		if encoding != "" && encoding != "zstd" {
			return httpResp, err
		}
		if err != nil {
			if !codexShouldRetryHTTPTransportError(ctx, err) {
				return httpResp, err
			}
			if httpResp != nil && httpResp.Body != nil {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close response body after transport error: %v", errClose)
				}
			}
			helps.LogWithRequestID(ctx).Debugf("codex executor: retrying HTTP request after transport error (attempt=%d/%d, content_encoding=%q): %v", attempt+1, codexHTTPMaxRequestRetries, encoding, err)
		} else {
			statusCode := 0
			if httpResp != nil {
				statusCode = httpResp.StatusCode
			}
			if httpResp != nil && httpResp.Body != nil {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close retryable response body: %v", errClose)
				}
			}
			helps.LogWithRequestID(ctx).Debugf("codex executor: retrying HTTP request after retryable status (attempt=%d/%d, status=%d, content_encoding=%q)", attempt+1, codexHTTPMaxRequestRetries, statusCode, encoding)
		}
		if errSleep := codexSleepBeforeHTTPRetry(ctx, attempt+1); errSleep != nil {
			return nil, errSleep
		}
	}
}

func codexShouldRetryHTTPResponseWithoutTurnState(resp *http.Response, prepared codexPreparedRequest) bool {
	if resp == nil || resp.StatusCode < 400 || resp.StatusCode >= 500 || resp.Body == nil {
		return false
	}
	data, err := codexReadHTTPResponseBodyPrefix(resp, helps.MaxErrorResponseBodyBytes)
	if err != nil {
		return false
	}
	return codexShouldRetryHTTPWithoutTurnState(prepared, data)
}

func codexShouldRetryHTTPWithoutTurnState(prepared codexPreparedRequest, errorBody []byte) bool {
	if prepared.httpReq == nil || len(errorBody) == 0 {
		return false
	}
	if strings.TrimSpace(prepared.httpReq.Header.Get(codexHeaderTurnState)) == "" {
		return false
	}
	if !codexWebsocketPreviousResponseNotFound(errorBody) &&
		!codexWebsocketNoToolCallFoundForFunctionOutput(errorBody) {
		return false
	}
	return codexHTTPBodyHasReplayableContext(prepared.body)
}

func (e *CodexExecutor) dropCodexHTTPTurnStateForRetry(ctx context.Context, auth *cliproxyauth.Auth, prepared codexPreparedRequest, reason string, statusCode int) {
	if prepared.httpReq == nil {
		return
	}
	e.forgetCodexHTTPTurnState(auth, prepared)
	prepared.httpReq.Header.Del(codexHeaderTurnState)
	helps.LogWithRequestID(ctx).Debugf("codex executor: retrying without stale HTTP turn state after %s (status=%d)", reason, statusCode)
}

func codexErrorBodyForTurnStateRetry(err error) []byte {
	if err == nil {
		return nil
	}
	var status statusErr
	if errors.As(err, &status) && strings.TrimSpace(status.msg) != "" {
		return []byte(status.msg)
	}
	return nil
}

func statusCodeFromCodexError(err error) int {
	var status statusErr
	if errors.As(err, &status) {
		return status.code
	}
	return 0
}

func codexHTTPBodyHasReplayableContext(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(body, "prompt").String()) != "" {
		return true
	}
	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() && len(messages.Array()) > 0 {
		return true
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || input.Type == gjson.Null {
		return false
	}
	if input.Type == gjson.String {
		return strings.TrimSpace(input.String()) != ""
	}
	if !input.IsArray() {
		return false
	}
	items, ok := codexRawArrayItems(input)
	if !ok || len(items) == 0 {
		return false
	}
	return codexWebsocketDeltaToolOutputsAnchorable(nil, nil, items)
}

func codexHTTPRequestForAttempt(prepared codexPreparedRequest, encoding string, attempt int) (*http.Request, error) {
	if prepared.httpReq == nil {
		return nil, errors.New("codex executor: request is nil")
	}
	if attempt <= 0 {
		return codexClonePreparedHTTPRequestForFirstAttempt(prepared)
	}
	req := codexClonePreparedHTTPRequestForRetry(prepared)
	if req == nil {
		return nil, errors.New("codex executor: cannot rebuild request for retry")
	}
	if encoding == "zstd" {
		req.Header.Del("Content-Encoding")
	} else if encoding != "" {
		return nil, errors.New("codex executor: cannot retry request with pre-encoded body")
	}
	return req, nil
}

func codexClonePreparedHTTPRequestForFirstAttempt(prepared codexPreparedRequest) (*http.Request, error) {
	if prepared.httpReq == nil {
		return nil, errors.New("codex executor: request is nil")
	}
	req := prepared.httpReq.Clone(prepared.httpReq.Context())
	req.Header = prepared.httpReq.Header.Clone()
	req.Host = prepared.httpReq.Host
	if prepared.httpReq.GetBody != nil {
		body, err := prepared.httpReq.GetBody()
		if err == nil {
			req.Body = body
			req.GetBody = prepared.httpReq.GetBody
			req.ContentLength = prepared.httpReq.ContentLength
			return req, nil
		}
	}
	if strings.TrimSpace(prepared.httpReq.Header.Get("Content-Encoding")) == "" {
		codexResetRequestBody(req, prepared.body)
		return req, nil
	}
	return nil, errors.New("codex executor: cannot rebuild pre-encoded request body")
}

func codexClonePreparedHTTPRequestForRetry(prepared codexPreparedRequest) *http.Request {
	if prepared.httpReq == nil {
		return nil
	}
	retryReq := prepared.httpReq.Clone(prepared.httpReq.Context())
	retryReq.Header = prepared.httpReq.Header.Clone()
	retryReq.Host = prepared.httpReq.Host
	codexResetRequestBody(retryReq, prepared.body)
	return retryReq
}

func codexShouldRetryHTTPStatus(resp *http.Response) bool {
	return resp != nil && resp.StatusCode >= 500 && resp.StatusCode <= 599
}

func codexShouldRetryHTTPStatusWithoutCompression(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusUnsupportedMediaType:
		return true
	case http.StatusBadRequest:
		if resp.Body == nil {
			return false
		}
		data, err := codexReadHTTPResponseBodyPrefix(resp, helps.MaxErrorResponseBodyBytes)
		if err != nil {
			return false
		}
		lower := strings.ToLower(string(data))
		return strings.Contains(lower, "content-encoding") ||
			strings.Contains(lower, "content encoding") ||
			strings.Contains(lower, "unsupported encoding") ||
			strings.Contains(lower, "unsupported compression") ||
			strings.Contains(lower, "zstd")
	default:
		return false
	}
}

func codexReadHTTPResponseBodyPrefix(resp *http.Response, maxBytes int64) ([]byte, error) {
	if resp == nil || resp.Body == nil || maxBytes <= 0 {
		return nil, nil
	}
	original := resp.Body
	data, err := io.ReadAll(io.LimitReader(original, maxBytes))
	resp.Body = &codexReplayHTTPResponseBody{
		reader: io.MultiReader(bytes.NewReader(data), original),
		closer: original,
	}
	return data, err
}

type codexReplayHTTPResponseBody struct {
	reader io.Reader
	closer io.Closer
}

func (b *codexReplayHTTPResponseBody) Read(p []byte) (int, error) {
	if b == nil || b.reader == nil {
		return 0, io.EOF
	}
	return b.reader.Read(p)
}

func (b *codexReplayHTTPResponseBody) Close() error {
	if b == nil || b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

func codexSleepBeforeHTTPRetry(ctx context.Context, attempt int) error {
	if attempt <= 0 {
		return nil
	}
	delay := codexHTTPRetryBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	if ctx == nil {
		time.Sleep(delay)
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func codexShouldRetryHTTPTransportError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	lower := strings.ToLower(err.Error())
	for _, marker := range codexHTTPRetryableTransportMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
