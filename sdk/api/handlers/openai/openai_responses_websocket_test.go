package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type websocketCaptureExecutor struct {
	streamCalls int
	payloads    [][]byte
}

type websocketRetryFullTranscriptExecutor struct {
	mu                sync.Mutex
	payloads          [][]byte
	sessions          map[string]chan error
	secondCallPayload []byte
	rejectIncremental bool
}

type websocketStatusError struct {
	status int
	msg    string
}

func (e websocketStatusError) Error() string   { return e.msg }
func (e websocketStatusError) StatusCode() int { return e.status }

type websocketCompactionCaptureExecutor struct {
	mu             sync.Mutex
	streamPayloads [][]byte
	compactPayload []byte
}

type orderedWebsocketSelector struct {
	mu     sync.Mutex
	order  []string
	cursor int
}

func (s *orderedWebsocketSelector) Pick(_ context.Context, _ string, _ string, _ coreexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(auths) == 0 {
		return nil, errors.New("no auth available")
	}
	for len(s.order) > 0 && s.cursor < len(s.order) {
		authID := strings.TrimSpace(s.order[s.cursor])
		s.cursor++
		for _, auth := range auths {
			if auth != nil && auth.ID == authID {
				return auth, nil
			}
		}
	}
	for _, auth := range auths {
		if auth != nil {
			return auth, nil
		}
	}
	return nil, errors.New("no auth available")
}

type websocketAuthCaptureExecutor struct {
	mu       sync.Mutex
	authIDs  []string
	payloads [][]byte
}

type websocketPinnedAuthFailureExecutor struct {
	mu      sync.Mutex
	authIDs []string
	resetID []string
}

type websocketExecutionSessionCaptureExecutor struct {
	mu                  sync.Mutex
	executionSessionIDs []string
	resetIDs            []string
	payloads            [][]byte
}

type websocketUpstreamDisconnectExecutor struct {
	mu         sync.Mutex
	subscribed chan string
	sessions   map[string]chan error
}

func (e *websocketUpstreamDisconnectExecutor) Identifier() string { return "codex" }

func (e *websocketUpstreamDisconnectExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	ch := e.ensureDisconnectSession(sessionID)
	e.publishDisconnectSubscription(sessionID)
	return ch
}

func (e *websocketUpstreamDisconnectExecutor) UpstreamDisconnectChanIfExists(sessionID string) <-chan error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	e.mu.Lock()
	ch := e.sessions[sessionID]
	e.mu.Unlock()
	if ch == nil {
		return nil
	}
	e.publishDisconnectSubscription(sessionID)
	return ch
}

func (e *websocketUpstreamDisconnectExecutor) ensureDisconnectSession(sessionID string) chan error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessions == nil {
		e.sessions = make(map[string]chan error)
	}
	ch, ok := e.sessions[sessionID]
	if !ok {
		ch = make(chan error, 1)
		e.sessions[sessionID] = ch
	}
	return ch
}

func (e *websocketUpstreamDisconnectExecutor) publishDisconnectSubscription(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	e.mu.Lock()
	subscribed := e.subscribed
	e.mu.Unlock()
	if subscribed != nil {
		select {
		case subscribed <- sessionID:
		default:
		}
	}
}

func (e *websocketUpstreamDisconnectExecutor) TriggerDisconnect(sessionID string, err error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	e.mu.Lock()
	ch := e.sessions[sessionID]
	delete(e.sessions, sessionID)
	e.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
	close(ch)
}

func (e *websocketUpstreamDisconnectExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketUpstreamDisconnectExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if sessionID, ok := opts.Metadata[coreexecutor.ExecutionSessionMetadataKey].(string); ok {
		e.ensureDisconnectSession(sessionID)
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-upstream","output":[{"type":"message","id":"out-1"}]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketUpstreamDisconnectExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketUpstreamDisconnectExecutor) ResetExecutionSession(sessionID string) {
	e.TriggerDisconnect(sessionID, errors.New("session reset"))
}

func (e *websocketUpstreamDisconnectExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketUpstreamDisconnectExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketAuthCaptureExecutor) Identifier() string { return "test-provider" }

func (e *websocketAuthCaptureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketAuthCaptureExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return e.executeStream(auth, &req)
}

func (e *websocketAuthCaptureExecutor) executeStream(auth *coreauth.Auth, req *coreexecutor.Request) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	if auth != nil {
		e.authIDs = append(e.authIDs, auth.ID)
	}
	if req != nil {
		e.payloads = append(e.payloads, append([]byte(nil), req.Payload...))
	}
	e.mu.Unlock()

	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-upstream","output":[{"type":"message","id":"out-1"}]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketAuthCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketAuthCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketAuthCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketAuthCaptureExecutor) AuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

func (e *websocketAuthCaptureExecutor) Payloads() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]byte, 0, len(e.payloads))
	for _, payload := range e.payloads {
		out = append(out, append([]byte(nil), payload...))
	}
	return out
}

func (e *websocketPinnedAuthFailureExecutor) Identifier() string { return "codex" }

func (e *websocketPinnedAuthFailureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketPinnedAuthFailureExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.mu.Lock()
	e.authIDs = append(e.authIDs, authID)
	authWSCalls := 0
	for _, id := range e.authIDs {
		if id == "auth-ws" {
			authWSCalls++
		}
	}
	e.mu.Unlock()

	chunks := make(chan coreexecutor.StreamChunk, 1)
	if authID == "auth-ws" && authWSCalls >= 2 {
		chunks <- coreexecutor.StreamChunk{Err: websocketStatusError{status: http.StatusUnauthorized, msg: "expired token"}}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-upstream","output":[{"type":"message","id":"out-1"}]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketPinnedAuthFailureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketPinnedAuthFailureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketPinnedAuthFailureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketPinnedAuthFailureExecutor) ResetExecutionSession(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetID = append(e.resetID, sessionID)
}

func (e *websocketPinnedAuthFailureExecutor) AuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

func (e *websocketPinnedAuthFailureExecutor) ResetIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.resetID...)
}

func replaceDefaultWebsocketToolCachesForTest(outputCache, callCache *websocketToolOutputCache, refs *websocketToolSessionRefCounter) func() {
	defaultWebsocketToolCachesMu.Lock()
	previousOutputCache := defaultWebsocketToolOutputCache
	previousCallCache := defaultWebsocketToolCallCache
	previousRefs := defaultWebsocketToolSessionRefs
	defaultWebsocketToolOutputCache = outputCache
	defaultWebsocketToolCallCache = callCache
	defaultWebsocketToolSessionRefs = refs
	defaultWebsocketToolCachesMu.Unlock()

	return func() {
		defaultWebsocketToolCachesMu.Lock()
		defaultWebsocketToolOutputCache = previousOutputCache
		defaultWebsocketToolCallCache = previousCallCache
		defaultWebsocketToolSessionRefs = previousRefs
		defaultWebsocketToolCachesMu.Unlock()
	}
}

func (e *websocketExecutionSessionCaptureExecutor) Identifier() string { return "test-provider" }

func (e *websocketExecutionSessionCaptureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketExecutionSessionCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	if sessionID, ok := opts.Metadata[coreexecutor.ExecutionSessionMetadataKey].(string); ok {
		e.executionSessionIDs = append(e.executionSessionIDs, sessionID)
	}
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	e.mu.Unlock()

	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-upstream","output":[{"type":"message","id":"out-1"}]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketExecutionSessionCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketExecutionSessionCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketExecutionSessionCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketExecutionSessionCaptureExecutor) ResetExecutionSession(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetIDs = append(e.resetIDs, sessionID)
}

func (e *websocketExecutionSessionCaptureExecutor) ExecutionSessionIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.executionSessionIDs...)
}

func (e *websocketExecutionSessionCaptureExecutor) ResetIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.resetIDs...)
}

func (e *websocketExecutionSessionCaptureExecutor) Payloads() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]byte, 0, len(e.payloads))
	for _, payload := range e.payloads {
		out = append(out, bytes.Clone(payload))
	}
	return out
}

func (e *websocketCaptureExecutor) Identifier() string { return "test-provider" }

func (e *websocketCaptureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.streamCalls++
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-upstream","output":[{"type":"message","id":"out-1"}]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketRetryFullTranscriptExecutor) Identifier() string { return "codex" }

func (e *websocketRetryFullTranscriptExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketRetryFullTranscriptExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if sessionID, ok := opts.Metadata[coreexecutor.ExecutionSessionMetadataKey].(string); ok {
		e.ensureDisconnectSession(sessionID)
	}
	e.mu.Lock()
	callIndex := len(e.payloads)
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	e.mu.Unlock()

	chunks := make(chan coreexecutor.StreamChunk, 1)
	if e.rejectIncremental && strings.TrimSpace(gjson.GetBytes(req.Payload, "previous_response_id").String()) != "" {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"error","status":400,"error":{"code":"previous_response_not_found","message":"Previous response with id 'resp-1' not found.","param":"previous_response_id","type":"invalid_request_error"}}`)}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}
	switch callIndex {
	case 0:
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","id":"fc-1","call_id":"call_Rx1FW4RrRF9C1SyH2xxBVtEn","name":"tool","arguments":"{}"}]}}`)}
	case 1:
		if len(e.secondCallPayload) > 0 {
			chunks <- coreexecutor.StreamChunk{Payload: bytes.Clone(e.secondCallPayload)}
		} else {
			chunks <- coreexecutor.StreamChunk{Err: websocketStatusError{status: http.StatusBadRequest, msg: `{"error":{"message":"No tool call found for function call output with call_id call_Rx1FW4RrRF9C1SyH2xxBVtEn.","param":"input","type":"invalid_request_error"}}`}}
		}
	default:
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[{"type":"message","id":"assistant-2"}]}}`)}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketRetryFullTranscriptExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketRetryFullTranscriptExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketRetryFullTranscriptExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketRetryFullTranscriptExecutor) UpstreamDisconnectChanIfExists(sessionID string) <-chan error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[sessionID]
}

func (e *websocketRetryFullTranscriptExecutor) ResetExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	e.mu.Lock()
	ch := e.sessions[sessionID]
	delete(e.sessions, sessionID)
	e.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- errors.New("session reset"):
	default:
	}
	close(ch)
}

func (e *websocketRetryFullTranscriptExecutor) ensureDisconnectSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessions == nil {
		e.sessions = make(map[string]chan error)
	}
	if e.sessions[sessionID] == nil {
		e.sessions[sessionID] = make(chan error, 1)
	}
}

func (e *websocketRetryFullTranscriptExecutor) Payloads() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]byte, 0, len(e.payloads))
	for _, payload := range e.payloads {
		out = append(out, bytes.Clone(payload))
	}
	return out
}

func (e *websocketCompactionCaptureExecutor) Identifier() string { return "test-provider" }

func (e *websocketCompactionCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.compactPayload = bytes.Clone(req.Payload)
	e.mu.Unlock()
	if opts.Alt != "responses/compact" {
		return coreexecutor.Response{}, fmt.Errorf("unexpected non-compact execute alt: %q", opts.Alt)
	}
	return coreexecutor.Response{Payload: []byte(`{"id":"cmp-1","object":"response.compaction"}`)}, nil
}

func (e *websocketCompactionCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	callIndex := len(e.streamPayloads)
	e.streamPayloads = append(e.streamPayloads, bytes.Clone(req.Payload))
	e.mu.Unlock()

	var payload []byte
	switch callIndex {
	case 0:
		payload = []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"tool"}]}}`)
	case 1:
		payload = []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[{"type":"message","id":"assistant-1"}]}}`)
	default:
		payload = []byte(`{"type":"response.completed","response":{"id":"resp-3","output":[{"type":"message","id":"assistant-2"}]}}`)
	}

	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: payload}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketCompactionCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketCompactionCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketCompactionCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestNormalizeResponsesWebsocketRequestCreate(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"test-model","stream":false,"input":[{"type":"message","id":"msg-1"}]}`)

	normalized, last, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized create request must not include type field")
	}
	if !gjson.GetBytes(normalized, "stream").Bool() {
		t.Fatalf("normalized create request must force stream=true")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}
	if !bytes.Equal(last, normalized) {
		t.Fatalf("last request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestCreateRepairsNullInput(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"test-model","input":null}`)

	normalized, _, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	input := gjson.GetBytes(normalized, "input")
	if !input.IsArray() || len(input.Array()) != 0 {
		t.Fatalf("null input should normalize to input=[]; body=%s", normalized)
	}
}

func TestNormalizeResponsesWebsocketRequestCreateNormalizesCodexInputItems(t *testing.T) {
	raw := []byte(`{
		"type":"response.create",
		"model":"test-model",
		"stream":false,
		"input":[
			{"type":"message","id":"msg-1"},
			{"type":"mcp_tool_call_output","call_id":"call-mcp","output":{"content":[{"type":"text","text":"fallback"}],"structuredContent":{"ok":true}}},
			{"type":"compaction_summary","encrypted_content":"enc-summary"},
			{"type":"compaction_trigger","reason":"token_limit"}
		]
	}`)

	normalized, last, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 3 {
		t.Fatalf("input len = %d, want 3 after dropping compaction_trigger: %s", len(items), normalized)
	}
	if got := items[1].Get("type").String(); got != "function_call_output" {
		t.Fatalf("mcp output type = %q, want function_call_output: %s", got, normalized)
	}
	if got := items[1].Get("output").String(); got != "Wall time: 0.0000 seconds\nOutput:\n"+`{"ok":true}` {
		t.Fatalf("mcp output = %q, want structured content JSON: %s", got, normalized)
	}
	if got := items[2].Get("type").String(); got != "compaction" {
		t.Fatalf("compaction_summary type = %q, want compaction: %s", got, normalized)
	}
	if !bytes.Equal(last, normalized) {
		t.Fatalf("last request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestCreateWithHistory(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized subsequent create request must not include type field")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}

	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("merged input len = %d, want 4", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "fc-1" ||
		input[2].Get("id").String() != "assistant-1" ||
		input[3].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestSubsequentNormalizesCodexInputItems(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{
		"type":"response.create",
		"input":[
			{"type":"mcp_tool_call_output","call_id":"call-mcp","output":{"content":[{"type":"image","data":"BASE64","mimeType":"image/png","_meta":{"codex/imageDetail":"low"}}]}},
			{"type":"compaction_trigger","reason":"token_limit"}
		]
	}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 4 {
		t.Fatalf("merged input len = %d, want 4 after dropping compaction_trigger: %s", len(items), normalized)
	}
	if got := items[3].Get("type").String(); got != "function_call_output" {
		t.Fatalf("mcp output type = %q, want function_call_output: %s", got, normalized)
	}
	if got := items[3].Get("output.0.text").String(); got != "Wall time: 0.0000 seconds\nOutput:" {
		t.Fatalf("mcp wall-time header = %q, want header: %s", got, normalized)
	}
	if got := items[3].Get("output.1.type").String(); got != "input_image" {
		t.Fatalf("mcp image output type = %q, want input_image: %s", got, normalized)
	}
	if got := items[3].Get("output.1.detail").String(); got != "low" {
		t.Fatalf("mcp image detail = %q, want low: %s", got, normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDIncremental(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"function_call","id":"fc-mcp","call_id":"call-mcp"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized request must not include type field")
	}
	if gjson.GetBytes(normalized, "previous_response_id").String() != "resp-1" {
		t.Fatalf("previous_response_id must be preserved in incremental mode")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 {
		t.Fatalf("incremental input len = %d, want 1", len(input))
	}
	if input[0].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected incremental input item id: %s", input[0].Get("id").String())
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}
	if gjson.GetBytes(normalized, "instructions").String() != "be helpful" {
		t.Fatalf("unexpected instructions: %s", gjson.GetBytes(normalized, "instructions").String())
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDKeepsSameInputToolCallOutput(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"apply_patch"},{"type":"custom_tool_call_output","call_id":"call-1","id":"tool-out-1","output":"ok"},{"type":"message","id":"msg-2"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1: %s", got, normalized)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 3 {
		t.Fatalf("incremental input len = %d, want 3: %s", len(input), normalized)
	}
	if input[0].Get("type").String() != "custom_tool_call" ||
		input[1].Get("type").String() != "custom_tool_call_output" ||
		input[2].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected incremental input order: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDFallsBackWhenToolOutputTypeMismatched(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"apply_patch"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1","output":"wrong-type"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id"); got.Exists() {
		t.Fatalf("previous_response_id should be removed for mismatched tool output fallback: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDKeepsClientToolSearchOutputWithoutCallID(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"tool_search_output","execution":"client","status":"completed","tools":[]}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1: %s", got, normalized)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 || input[0].Get("type").String() != "tool_search_output" {
		t.Fatalf("unexpected incremental input: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDKeepsServerToolSearchOutputWithoutCallID(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"tool_search_output","execution":"server","status":"completed","tools":[]}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1: %s", got, normalized)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 || input[0].Get("type").String() != "tool_search_output" {
		t.Fatalf("unexpected incremental input: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDKeepsServerToolSearchOutputWithCallID(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"tool_search_output","call_id":"server-search","execution":"server","status":"completed","tools":[]}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1: %s", got, normalized)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 || input[0].Get("type").String() != "tool_search_output" {
		t.Fatalf("unexpected incremental input: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDNormalizesCodexInputItems(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"function_call","id":"fc-mcp","call_id":"call-mcp"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{
		"type":"response.create",
		"previous_response_id":"resp-1",
		"input":[
			{"type":"mcp_tool_call_output","call_id":"call-mcp","output":{"content":[{"type":"text","text":"hello"}]}},
			{"type":"compaction_summary","encrypted_content":"enc-summary"}
		]
	}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1", got)
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 2 {
		t.Fatalf("incremental input len = %d, want 2: %s", len(items), normalized)
	}
	if got := items[0].Get("type").String(); got != "function_call_output" {
		t.Fatalf("mcp output type = %q, want function_call_output: %s", got, normalized)
	}
	if got := items[0].Get("output").String(); got != "Wall time: 0.0000 seconds\nOutput:\n"+`[{"text":"hello","type":"text"}]` {
		t.Fatalf("mcp output = %q, want serialized content array: %s", got, normalized)
	}
	if got := items[1].Get("type").String(); got != "compaction" {
		t.Fatalf("compaction_summary type = %q, want compaction: %s", got, normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDFallsBackWhenToolOutputUnknown(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-missing","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id"); got.Exists() {
		t.Fatalf("previous_response_id should be removed for unknown tool output fallback: %s", normalized)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 3 {
		t.Fatalf("merged input len = %d, want 3: %s", len(input), normalized)
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "assistant-1" ||
		input[2].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDMergedWhenIncrementalDisabled(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, false)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must be removed when incremental mode is disabled")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("merged input len = %d, want 4", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "fc-1" ||
		input[2].Get("id").String() != "assistant-1" ||
		input[3].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestAppend(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"},
		{"type":"function_call_output","id":"tool-out-1"}
	]`)
	raw := []byte(`{"type":"response.append","input":[{"type":"message","id":"msg-2"},{"type":"message","id":"msg-3"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 5 {
		t.Fatalf("merged input len = %d, want 5", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "assistant-1" ||
		input[2].Get("id").String() != "tool-out-1" ||
		input[3].Get("id").String() != "msg-2" ||
		input[4].Get("id").String() != "msg-3" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized append request")
	}
}

func TestNormalizeResponsesWebsocketRequestAppendWithoutCreate(t *testing.T) {
	raw := []byte(`{"type":"response.append","input":[]}`)

	_, _, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
	if errMsg == nil {
		t.Fatalf("expected error for append without previous request")
	}
	if errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusBadRequest)
	}
}

func TestMergeJSONArrayRawPreservesOrder(t *testing.T) {
	merged, err := mergeJSONArrayRaw(
		`[{"type":"message","id":"msg-1"}]`,
		`[{"type":"function_call","call_id":"call-1"},{"type":"message","id":"msg-2"}]`,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	items := gjson.Parse(merged).Array()
	if len(items) != 3 {
		t.Fatalf("merged len = %d, want 3", len(items))
	}
	if items[0].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected first item: %s", items[0].Raw)
	}
	if items[1].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected second item: %s", items[1].Raw)
	}
	if items[2].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected third item: %s", items[2].Raw)
	}
}

func TestDedupeFunctionCallsByCallIDRemovesDuplicateCalls(t *testing.T) {
	deduped, err := dedupeFunctionCallsByCallID(
		`[
			{"type":"message","id":"msg-1"},
			{"type":"function_call","call_id":"call-1","id":"fc-1"},
			{"type":"function_call","call_id":"call-1","id":"fc-dup"},
			{"type":"message","id":"msg-2"}
		]`,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	items := gjson.Parse(deduped).Array()
	if len(items) != 3 {
		t.Fatalf("deduped len = %d, want 3", len(items))
	}
	if items[0].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected first item: %s", items[0].Raw)
	}
	if items[1].Get("id").String() != "fc-1" {
		t.Fatalf("unexpected retained function call: %s", items[1].Raw)
	}
	if items[2].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected trailing item: %s", items[2].Raw)
	}
}

func TestWebsocketJSONPayloadsFromChunk(t *testing.T) {
	chunk := []byte("event: response.created\n\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\ndata: [DONE]\n")

	payloads := websocketJSONPayloadsFromChunk(chunk)
	if len(payloads) != 1 {
		t.Fatalf("payloads len = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.created" {
		t.Fatalf("unexpected payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
}

func TestWebsocketJSONPayloadsFromPlainJSONChunk(t *testing.T) {
	chunk := []byte(`{"type":"response.completed","response":{"id":"resp-1"}}`)

	payloads := websocketJSONPayloadsFromChunk(chunk)
	if len(payloads) != 1 {
		t.Fatalf("payloads len = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.completed" {
		t.Fatalf("unexpected payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
}

func TestResponseCompletedOutputFromPayload(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"message","id":"out-1"}]}}`)

	output := responseCompletedOutputFromPayload(payload)
	items := gjson.ParseBytes(output).Array()
	if len(items) != 1 {
		t.Fatalf("output len = %d, want 1", len(items))
	}
	if items[0].Get("id").String() != "out-1" {
		t.Fatalf("unexpected output id: %s", items[0].Get("id").String())
	}
}

func TestAppendWebsocketEvent(t *testing.T) {
	builder := newWebsocketTimelineBuilder(maxResponsesWebsocketTimelineBytes)

	appendWebsocketEvent(&builder, "request", []byte("  {\"type\":\"response.create\"}\n"))
	appendWebsocketEvent(&builder, "response", []byte("{\"type\":\"response.created\"}"))

	got := builder.String()
	if !strings.Contains(got, "websocket.request\n{\"type\":\"response.create\"}\n") {
		t.Fatalf("request event not found in body: %s", got)
	}
	if !strings.Contains(got, "websocket.response\n{\"type\":\"response.created\"}\n") {
		t.Fatalf("response event not found in body: %s", got)
	}
}

func TestAppendWebsocketTimelineEvent(t *testing.T) {
	builder := newWebsocketTimelineBuilder(maxResponsesWebsocketTimelineBytes)
	ts := time.Date(2026, time.April, 1, 12, 34, 56, 789000000, time.UTC)

	appendWebsocketTimelineEvent(&builder, "request", []byte("  {\"type\":\"response.create\"}\n"), ts)

	got := builder.String()
	if !strings.Contains(got, "Timestamp: 2026-04-01T12:34:56.789Z") {
		t.Fatalf("timeline timestamp not found: %s", got)
	}
	if !strings.Contains(got, "Event: websocket.request") {
		t.Fatalf("timeline event not found: %s", got)
	}
	if !strings.Contains(got, "{\"type\":\"response.create\"}") {
		t.Fatalf("timeline payload not found: %s", got)
	}
}

func TestAppendWebsocketTimelineEventRedactsSensitiveJSONFields(t *testing.T) {
	builder := newWebsocketTimelineBuilder(maxResponsesWebsocketTimelineBytes)
	ts := time.Date(2026, time.April, 1, 12, 34, 56, 789000000, time.UTC)

	appendWebsocketTimelineEvent(&builder, "request", []byte(`{"type":"response.create","client_metadata":{"access_token":"secret-token"},"input":[{"api_key":"sk-secret"}]}`), ts)

	got := builder.String()
	if strings.Contains(got, "secret-token") || strings.Contains(got, "sk-secret") {
		t.Fatalf("timeline leaked sensitive payload: %s", got)
	}
	if count := strings.Count(got, "[REDACTED]"); count != 2 {
		t.Fatalf("redacted marker count = %d, want 2; timeline=%s", count, got)
	}
}

func TestWriteResponsesWebsocketErrorFiltersAddonHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() {
			_ = conn.Close()
		}()

		_, err = writeResponsesWebsocketError(conn, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      errors.New("rate limit"),
			Addon: http.Header{
				"Connection":        {"X-Secret-Hop"},
				"Retry-After":       {"30"},
				"Set-Cookie":        {"sid=secret"},
				"Transfer-Encoding": {"chunked"},
				"X-Litellm-Call-Id": {"gateway"},
				"X-Request-Id":      {"req-1"},
				"X-Secret-Hop":      {"hop-secret"},
			},
		})
		serverErrCh <- err
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read websocket error payload: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "headers.Retry-After").String(); got != "30" {
		t.Fatalf("Retry-After header = %q, want %q; payload=%s", got, "30", payload)
	}
	if got := gjson.GetBytes(payload, "headers.X-Request-Id").String(); got != "req-1" {
		t.Fatalf("X-Request-Id header = %q, want %q; payload=%s", got, "req-1", payload)
	}
	for _, path := range []string{"headers.Connection", "headers.Set-Cookie", "headers.Transfer-Encoding", "headers.X-Litellm-Call-Id", "headers.X-Secret-Hop"} {
		if gjson.GetBytes(payload, path).Exists() {
			t.Fatalf("%s should be filtered; payload=%s", path, payload)
		}
	}
	if errServer := <-serverErrCh; errServer != nil {
		t.Fatalf("server error: %v", errServer)
	}
}

func TestResponsesWebsocketOriginAllowed(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		headers http.Header
		want    bool
	}{
		{
			name: "native client without origin",
			host: "127.0.0.1:8080",
			want: true,
		},
		{
			name: "same origin",
			host: "example.com:8443",
			headers: http.Header{
				"Origin": {"https://example.com:8443"},
			},
			want: true,
		},
		{
			name: "spoofed forwarded host rejected",
			host: "127.0.0.1:8080",
			headers: http.Header{
				"Origin":           {"https://api.example.com"},
				"X-Forwarded-Host": {"api.example.com"},
			},
			want: false,
		},
		{
			name: "https default port explicit in origin",
			host: "example.com",
			headers: http.Header{
				"Origin": {"https://example.com:443"},
			},
			want: true,
		},
		{
			name: "http explicit https port rejected without request port",
			host: "example.com",
			headers: http.Header{
				"Origin": {"http://example.com:443"},
			},
			want: false,
		},
		{
			name: "non default origin port rejected",
			host: "example.com",
			headers: http.Header{
				"Origin": {"https://example.com:8443"},
			},
			want: false,
		},
		{
			name: "ipv6 same origin",
			host: "[::1]:8080",
			headers: http.Header{
				"Origin": {"http://[::1]:8080"},
			},
			want: true,
		},
		{
			name: "ipv6 cross port",
			host: "[::1]:8080",
			headers: http.Header{
				"Origin": {"http://[::1]:3000"},
			},
			want: false,
		},
		{
			name: "origin with path rejected",
			host: "example.com",
			headers: http.Header{
				"Origin": {"https://example.com/path"},
			},
			want: false,
		},
		{
			name: "origin with userinfo rejected",
			host: "example.com",
			headers: http.Header{
				"Origin": {"https://user@example.com"},
			},
			want: false,
		},
		{
			name: "cross origin",
			host: "api.example.com",
			headers: http.Header{
				"Origin": {"https://evil.example"},
			},
			want: false,
		},
		{
			name: "local browser cross port",
			host: "127.0.0.1:8080",
			headers: http.Header{
				"Origin": {"http://127.0.0.1:3000"},
			},
			want: false,
		},
		{
			name: "null origin",
			host: "127.0.0.1:8080",
			headers: http.Header{
				"Origin": {"null"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			req.Host = tt.host
			for key, values := range tt.headers {
				for _, value := range values {
					req.Header.Add(key, value)
				}
			}
			if got := responsesWebsocketOriginAllowed(req); got != tt.want {
				t.Fatalf("responsesWebsocketOriginAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAppendWebsocketTimelineEventTruncatesOnce(t *testing.T) {
	builder := newWebsocketTimelineBuilder(maxResponsesWebsocketTimelineBytes)
	ts := time.Date(2026, time.April, 1, 12, 34, 56, 789000000, time.UTC)
	payload := bytes.Repeat([]byte("x"), maxResponsesWebsocketTimelineBytes)

	appendWebsocketTimelineEvent(&builder, "request", payload, ts)
	first := builder.String()
	if !strings.Contains(first, responsesWebsocketTimelineTruncatedMarker) {
		t.Fatalf("expected truncated marker in first payload")
	}

	appendWebsocketTimelineEvent(&builder, "response", []byte(`{"type":"response.created"}`), ts)
	second := builder.String()
	if second != first {
		t.Fatalf("expected timeline to stop growing after truncation")
	}
	if strings.Count(second, responsesWebsocketTimelineTruncatedMarker) != 1 {
		t.Fatalf("expected exactly one truncated marker, got %d", strings.Count(second, responsesWebsocketTimelineTruncatedMarker))
	}
}

func TestResponsesWebsocketTimelineErrorOnlyWhenRequestLogDisabled(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: false}, nil)
	h := NewOpenAIResponsesAPIHandler(base)
	builder := newResponsesWebsocketTimelineBuilder(h)
	ts := time.Date(2026, time.April, 1, 12, 34, 56, 789000000, time.UTC)

	appendWebsocketTimelineEvent(&builder, "request", []byte(`{"type":"response.create"}`), ts)
	appendWebsocketTimelineEvent(&builder, "response", []byte(`{"type":"response.created"}`), ts)
	if got := builder.String(); got != "" {
		t.Fatalf("normal websocket events should not be retained without request-log: %s", got)
	}

	appendWebsocketTimelineEvent(&builder, "response", []byte(`{"type":"error","error":{"message":"failed"}}`), ts)
	got := builder.String()
	if !strings.Contains(got, "Event: websocket.response") || !strings.Contains(got, `"type":"error"`) {
		t.Fatalf("error websocket event should be retained: %s", got)
	}
}

func TestSetWebsocketTimelineBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	setWebsocketTimelineBody(c, " \n ")
	if _, exists := c.Get(wsTimelineBodyKey); exists {
		t.Fatalf("timeline body key should not be set for empty body")
	}

	setWebsocketTimelineBody(c, "timeline body")
	value, exists := c.Get(wsTimelineBodyKey)
	if !exists {
		t.Fatalf("timeline body key not set")
	}
	bodyBytes, ok := value.([]byte)
	if !ok {
		t.Fatalf("timeline body key type mismatch")
	}
	if string(bodyBytes) != "timeline body" {
		t.Fatalf("timeline body = %q, want %q", string(bodyBytes), "timeline body")
	}
}

func TestWebsocketDownstreamSessionKeyPrefersStableSessionHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("X-Client-Request-Id", "request-ephemeral")
	req.Header.Set("Session_id", "session-stable")

	if got := websocketDownstreamSessionKey(req); got != "session-stable" {
		t.Fatalf("websocketDownstreamSessionKey() = %q, want session-stable", got)
	}
}

func TestWebsocketDownstreamSessionKeyPrefersThreadOverSession(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("X-Client-Request-Id", "thread-stable")
	req.Header.Set("Session_id", "session-stable")
	req.Header.Set("Thread_id", "thread-stable")

	if got := websocketDownstreamSessionKey(req); got != "thread-stable" {
		t.Fatalf("websocketDownstreamSessionKey() = %q, want thread-stable", got)
	}
}

func TestWebsocketDownstreamSessionKeyUsesTurnMetadataThreadBeforeRequestID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("X-Client-Request-Id", "request-ephemeral")
	req.Header.Set("X-Codex-Turn-Metadata", `{"session_id":"session-stable","thread_id":"thread-stable"}`)

	if got := websocketDownstreamSessionKey(req); got != "thread-stable" {
		t.Fatalf("websocketDownstreamSessionKey() = %q, want thread-stable", got)
	}
}

func TestRepairResponsesWebsocketToolCallsSkipsPlainMessages(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	raw := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)

	repaired := repairResponsesWebsocketToolCallsWithCache(cache, "session-plain", raw)
	if !bytes.Equal(repaired, raw) {
		t.Fatalf("plain message request should not be rewritten: %s", repaired)
	}
}

func TestRepairResponsesWebsocketToolCallsDoesNotCacheDroppedOrphanOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-orphan"

	orphan := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":"stale"}]}`)
	repairedOrphan := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, orphan)
	if got := len(gjson.GetBytes(repairedOrphan, "input").Array()); got != 0 {
		t.Fatalf("orphan output should be dropped, input len = %d: %s", got, repairedOrphan)
	}

	raw := []byte(`{"input":[{"type":"function_call","call_id":"call-1","name":"tool"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)
	output := gjson.GetBytes(repaired, "input.1")
	if output.Get("type").String() != "function_call_output" || output.Get("call_id").String() != "call-1" {
		t.Fatalf("missing synthetic output after call: %s", repaired)
	}
	if got := output.Get("output").String(); got != "aborted" {
		t.Fatalf("dropped orphan output leaked through cache, output=%q body=%s", got, repaired)
	}
}

func TestRepairResponsesWebsocketToolCallsInsertsCachedOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	cacheWarm := []byte(`{"previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
	warmed := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, cacheWarm)
	if gjson.GetBytes(warmed, "input.0.call_id").String() != "call-1" {
		t.Fatalf("expected warmup output to remain")
	}

	raw := []byte(`{"input":[{"type":"function_call","call_id":"call-1","name":"tool"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "function_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected first item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "function_call_output" || input[1].Get("call_id").String() != "call-1" {
		t.Fatalf("missing inserted output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsAddsMissingFunctionOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"function_call","call_id":"call-1","name":"tool"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "function_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected call item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "function_call_output" ||
		input[1].Get("call_id").String() != "call-1" ||
		input[1].Get("output").String() != "aborted" {
		t.Fatalf("missing synthesized function output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsInsertsCachedCallForOrphanOutput(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	callCache.record(sessionKey, "call-1", []byte(`{"type":"function_call","call_id":"call-1","name":"tool"}`))

	raw := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":"ok"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "function_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("missing inserted call: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "function_call_output" || input[1].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected output item: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsPreviousResponseOutputIncremental(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	callCache.record(sessionKey, "call-1", []byte(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"tool"}`))

	raw := []byte(`{"previous_response_id":"resp-latest","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1","output":"ok"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)

	if got := gjson.GetBytes(repaired, "previous_response_id").String(); got != "resp-latest" {
		t.Fatalf("previous_response_id = %q, want resp-latest", got)
	}
	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "function_call_output" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected output item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsPreviousResponseCallIncremental(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	outputCache.record(sessionKey, "call-1", []byte(`{"type":"function_call_output","call_id":"call-1","id":"tool-out-1","output":"ok"}`))

	raw := []byte(`{"previous_response_id":"resp-latest","input":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"tool"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)

	if got := gjson.GetBytes(repaired, "previous_response_id").String(); got != "resp-latest" {
		t.Fatalf("previous_response_id = %q, want resp-latest", got)
	}
	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "function_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected call item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsDropsOrphanOutputWhenCallMissing(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":"ok"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 1 {
		t.Fatalf("repaired input len = %d, want 1", len(input))
	}
	if input[0].Get("type").String() != "message" || input[0].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected remaining item: %s", input[0].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsInsertsCachedCustomToolOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	cacheWarm := []byte(`{"previous_response_id":"resp-1","input":[{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"}]}`)
	warmed := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, cacheWarm)
	if gjson.GetBytes(warmed, "input.0.call_id").String() != "call-1" {
		t.Fatalf("expected warmup output to remain")
	}

	raw := []byte(`{"input":[{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "custom_tool_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected first item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "custom_tool_call_output" || input[1].Get("call_id").String() != "call-1" {
		t.Fatalf("missing inserted output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsInsertsCachedLocalShellOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	cacheWarm := []byte(`{"previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-shell","output":"ok"}]}`)
	warmed := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, cacheWarm)
	if gjson.GetBytes(warmed, "input.0.call_id").String() != "call-shell" {
		t.Fatalf("expected warmup output to remain")
	}

	raw := []byte(`{"input":[{"type":"local_shell_call","call_id":"call-shell","status":"completed","action":{"type":"exec","command":["pwd"]}},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "local_shell_call" || input[0].Get("call_id").String() != "call-shell" {
		t.Fatalf("unexpected first item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "function_call_output" || input[1].Get("call_id").String() != "call-shell" {
		t.Fatalf("missing inserted output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsAddsMissingLocalShellOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"local_shell_call","call_id":"call-shell","status":"completed","action":{"type":"exec","command":["pwd"]}},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "local_shell_call" || input[0].Get("call_id").String() != "call-shell" {
		t.Fatalf("unexpected first item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "function_call_output" ||
		input[1].Get("call_id").String() != "call-shell" ||
		input[1].Get("output").String() != "aborted" {
		t.Fatalf("missing synthesized local shell output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsAddsMissingToolSearchOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"tool_search_call","call_id":"search-1","execution":"client","arguments":{"query":"calendar"}},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "tool_search_call" || input[0].Get("call_id").String() != "search-1" {
		t.Fatalf("unexpected first item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "tool_search_output" ||
		input[1].Get("call_id").String() != "search-1" ||
		input[1].Get("execution").String() != "client" ||
		!input[1].Get("tools").IsArray() {
		t.Fatalf("missing synthesized tool search output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsServerToolSearchWithoutCallID(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"tool_search_call","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["crm"]}},{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":[]},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "tool_search_call" || input[0].Get("execution").String() != "server" {
		t.Fatalf("unexpected server tool search call: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "tool_search_output" || input[1].Get("execution").String() != "server" {
		t.Fatalf("unexpected server tool search output: %s", input[1].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsServerToolSearchWithCallIDWithoutOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"tool_search_call","execution":"server","call_id":"server-search","status":"completed","arguments":{"paths":["crm"]}},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "tool_search_call" ||
		input[0].Get("execution").String() != "server" ||
		input[0].Get("call_id").String() != "server-search" {
		t.Fatalf("unexpected server tool search call: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsClientToolSearchOutputWithoutCallID(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"tool_search_output","execution":"client","call_id":null,"status":"completed","tools":[]},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "tool_search_output" || input[0].Get("execution").String() != "client" {
		t.Fatalf("unexpected tool search output: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsServerToolSearchOutputWithCallID(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"tool_search_output","execution":"server","call_id":"server-search","status":"completed","tools":[]},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "tool_search_output" ||
		input[0].Get("execution").String() != "server" ||
		input[0].Get("call_id").String() != "server-search" {
		t.Fatalf("server tool search output should be preserved: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
	if cached, ok := cache.get(sessionKey, "server-search"); ok {
		t.Fatalf("server tool search output should not be cached: %s", cached)
	}
}

func TestRepairResponsesWebsocketToolCallsAddsMissingCustomToolOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "custom_tool_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected call item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "custom_tool_call_output" ||
		input[1].Get("call_id").String() != "call-1" ||
		input[1].Get("output").String() != "aborted" {
		t.Fatalf("missing synthesized custom tool output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsDropsMismatchedCustomToolOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch"},{"type":"function_call_output","call_id":"call-1","output":"wrong"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "custom_tool_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected call item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "custom_tool_call_output" ||
		input[1].Get("call_id").String() != "call-1" ||
		input[1].Get("output").String() != "aborted" {
		t.Fatalf("missing synthesized custom tool output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsDropsMismatchedFunctionOutput(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"function_call","call_id":"call-1","name":"tool"},{"type":"custom_tool_call_output","call_id":"call-1","output":"wrong"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "function_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected call item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "function_call_output" ||
		input[1].Get("call_id").String() != "call-1" ||
		input[1].Get("output").String() != "aborted" {
		t.Fatalf("missing synthesized function output: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsInsertsCachedCustomToolCallForOrphanOutput(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	callCache.record(sessionKey, "call-1", []byte(`{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch"}`))

	raw := []byte(`{"input":[{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 3 {
		t.Fatalf("repaired input len = %d, want 3", len(input))
	}
	if input[0].Get("type").String() != "custom_tool_call" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("missing inserted call: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "custom_tool_call_output" || input[1].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected output item: %s", input[1].Raw)
	}
	if input[2].Get("type").String() != "message" || input[2].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[2].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsKeepsPreviousResponseCustomToolOutputIncremental(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	callCache.record(sessionKey, "call-1", []byte(`{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch"}`))

	raw := []byte(`{"previous_response_id":"resp-latest","input":[{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)

	if got := gjson.GetBytes(repaired, "previous_response_id").String(); got != "resp-latest" {
		t.Fatalf("previous_response_id = %q, want resp-latest", got)
	}
	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 2 {
		t.Fatalf("repaired input len = %d, want 2: %s", len(input), repaired)
	}
	if input[0].Get("type").String() != "custom_tool_call_output" || input[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected output item: %s", input[0].Raw)
	}
	if input[1].Get("type").String() != "message" || input[1].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected trailing item: %s", input[1].Raw)
	}
}

func TestRepairResponsesWebsocketToolCallsDropsOrphanCustomToolOutputWhenCallMissing(t *testing.T) {
	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	raw := []byte(`{"input":[{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"},{"type":"message","id":"msg-1"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, raw)

	input := gjson.GetBytes(repaired, "input").Array()
	if len(input) != 1 {
		t.Fatalf("repaired input len = %d, want 1", len(input))
	}
	if input[0].Get("type").String() != "message" || input[0].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected remaining item: %s", input[0].Raw)
	}
}

func TestRecordResponsesWebsocketToolCallsFromPayloadWithCache(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"tool","arguments":"{}"}]}}`)
	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, sessionKey, payload)

	cached, ok := cache.get(sessionKey, "call-1")
	if !ok {
		t.Fatalf("expected cached tool call")
	}
	if gjson.GetBytes(cached, "type").String() != "function_call" || gjson.GetBytes(cached, "call_id").String() != "call-1" {
		t.Fatalf("unexpected cached tool call: %s", cached)
	}
}

func TestRecordResponsesWebsocketCustomToolCallsFromCompletedPayloadWithCache(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"apply_patch","input":"*** Begin Patch"}]}}`)
	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, sessionKey, payload)

	cached, ok := cache.get(sessionKey, "call-1")
	if !ok {
		t.Fatalf("expected cached custom tool call")
	}
	if gjson.GetBytes(cached, "type").String() != "custom_tool_call" || gjson.GetBytes(cached, "call_id").String() != "call-1" {
		t.Fatalf("unexpected cached custom tool call: %s", cached)
	}
}

func TestRecordResponsesWebsocketCustomToolCallsFromOutputItemDoneWithCache(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	payload := []byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"apply_patch","input":"*** Begin Patch"}}`)
	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, sessionKey, payload)

	cached, ok := cache.get(sessionKey, "call-1")
	if !ok {
		t.Fatalf("expected cached custom tool call")
	}
	if gjson.GetBytes(cached, "type").String() != "custom_tool_call" || gjson.GetBytes(cached, "call_id").String() != "call-1" {
		t.Fatalf("unexpected cached custom tool call: %s", cached)
	}
}

func TestRecordResponsesWebsocketLocalShellCallsFromCompletedPayloadWithCache(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"local_shell_call","id":"lsc-1","call_id":"call-shell","status":"completed","action":{"type":"exec","command":["pwd"]}}]}}`)
	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, sessionKey, payload)

	cached, ok := cache.get(sessionKey, "call-shell")
	if !ok {
		t.Fatalf("expected cached local shell call")
	}
	if gjson.GetBytes(cached, "type").String() != "local_shell_call" || gjson.GetBytes(cached, "call_id").String() != "call-shell" {
		t.Fatalf("unexpected cached local shell call: %s", cached)
	}
}

func TestRecordResponsesWebsocketToolSearchCallsFromCompletedPayloadWithCache(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"tool_search_call","call_id":"search-1","execution":"client","arguments":{"query":"calendar"}}]}}`)
	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, sessionKey, payload)

	cached, ok := cache.get(sessionKey, "search-1")
	if !ok {
		t.Fatalf("expected cached tool search call")
	}
	if gjson.GetBytes(cached, "type").String() != "tool_search_call" || gjson.GetBytes(cached, "call_id").String() != "search-1" {
		t.Fatalf("unexpected cached tool search call: %s", cached)
	}
}

func TestRecordResponsesWebsocketServerToolSearchCallsAreNotCached(t *testing.T) {
	cache := newWebsocketToolOutputCache(time.Minute, 10)
	sessionKey := "session-1"

	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"tool_search_call","call_id":"server-search","execution":"server","status":"completed","arguments":{"paths":["crm"]}}]}}`)
	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, sessionKey, payload)

	if cached, ok := cache.get(sessionKey, "server-search"); ok {
		t.Fatalf("server tool search call should not be cached: %s", cached)
	}
}

func TestWebsocketToolOutputCacheSkipsOversizedItems(t *testing.T) {
	cache := newWebsocketToolOutputCacheWithLimits(time.Minute, 10, 96, 128)
	sessionKey := "session-1"

	cache.record(sessionKey, "call-1", []byte(`{"type":"function_call_output","call_id":"call-1","output":"ok"}`))
	if _, ok := cache.get(sessionKey, "call-1"); !ok {
		t.Fatal("expected initial cached item")
	}

	cache.record(sessionKey, "call-1", bytes.Repeat([]byte("x"), 97))
	if _, ok := cache.get(sessionKey, "call-1"); ok {
		t.Fatal("oversized replacement should remove cached item")
	}
}

func TestWebsocketToolOutputCacheUsesDefaultTTL(t *testing.T) {
	cache := newWebsocketToolOutputCache(0, 0)

	if cache.ttl != websocketToolOutputCacheTTL {
		t.Fatalf("ttl = %s, want %s", cache.ttl, websocketToolOutputCacheTTL)
	}
	if cache.maxPerSession != websocketToolOutputCacheMaxPerSession {
		t.Fatalf("maxPerSession = %d, want %d", cache.maxPerSession, websocketToolOutputCacheMaxPerSession)
	}
}

func TestWebsocketToolOutputCacheEvictsToByteLimit(t *testing.T) {
	cache := newWebsocketToolOutputCacheWithLimits(time.Minute, 10, 128, 96)
	sessionKey := "session-1"

	cache.record(sessionKey, "call-1", []byte(`{"type":"function_call_output","call_id":"call-1","output":"11111111111111111111"}`))
	cache.record(sessionKey, "call-2", []byte(`{"type":"function_call_output","call_id":"call-2","output":"22222222222222222222"}`))

	if _, ok := cache.get(sessionKey, "call-1"); ok {
		t.Fatal("oldest cached item should be evicted when session byte budget is exceeded")
	}
	if _, ok := cache.get(sessionKey, "call-2"); !ok {
		t.Fatal("newest cached item should remain")
	}
}

func TestForwardResponsesWebsocketPreservesCompletedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() {
			errClose := conn.Close()
			if errClose != nil {
				serverErrCh <- errClose
			}
		}()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r

		data := make(chan []byte, 1)
		errCh := make(chan *interfaces.ErrorMessage)
		data <- []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"message\",\"id\":\"out-1\"}]}}\n\n")
		close(data)
		close(errCh)

		timelineLog := newWebsocketTimelineBuilder(maxResponsesWebsocketTimelineBytes)
		handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}}
		completedOutput, completedResponseID, _, err := handler.forwardResponsesWebsocket(
			ctx,
			conn,
			func(...interface{}) {},
			data,
			errCh,
			&timelineLog,
			"session-1",
			"session-1",
		)
		if err != nil {
			serverErrCh <- err
			return
		}
		if gjson.GetBytes(completedOutput, "0.id").String() != "out-1" {
			serverErrCh <- errors.New("completed output not captured")
			return
		}
		if completedResponseID != "resp-1" {
			serverErrCh <- fmt.Errorf("completed response id = %q, want resp-1", completedResponseID)
			return
		}
		if !strings.Contains(timelineLog.String(), "Event: websocket.response") {
			serverErrCh <- errors.New("websocket timeline did not capture downstream response")
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		errClose := conn.Close()
		if errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read websocket message: %v", errReadMessage)
	}
	if gjson.GetBytes(payload, "type").String() != wsEventTypeCompleted {
		t.Fatalf("payload type = %s, want %s", gjson.GetBytes(payload, "type").String(), wsEventTypeCompleted)
	}
	if strings.Contains(string(payload), "response.done") {
		t.Fatalf("payload unexpectedly rewrote completed event: %s", payload)
	}

	if errServer := <-serverErrCh; errServer != nil {
		t.Fatalf("server error: %v", errServer)
	}
}

func TestForwardResponsesWebsocketRejectsNilDataAndErrorChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.Close() }()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r

		var cancelErr error
		handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}}
		_, _, _, err = handler.forwardResponsesWebsocket(
			ctx,
			conn,
			func(params ...interface{}) {
				if len(params) > 0 {
					if errParam, ok := params[0].(error); ok {
						cancelErr = errParam
					}
				}
			},
			nil,
			nil,
			nil,
			"session-1",
			"session-1",
		)
		if !errors.Is(err, errResponsesWebsocketNilStreamChannels) {
			serverErrCh <- fmt.Errorf("forward error = %v, want nil stream channels", err)
			return
		}
		if !errors.Is(cancelErr, errResponsesWebsocketNilStreamChannels) {
			serverErrCh <- fmt.Errorf("cancel error = %v, want nil stream channels", cancelErr)
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read websocket message: %v", errReadMessage)
	}
	if gjson.GetBytes(payload, "type").String() != wsEventTypeError {
		t.Fatalf("payload type = %s, want %s; payload=%s", gjson.GetBytes(payload, "type").String(), wsEventTypeError, payload)
	}
	if got := gjson.GetBytes(payload, "status").Int(); got != http.StatusBadGateway {
		t.Fatalf("payload status = %d, want %d; payload=%s", got, http.StatusBadGateway, payload)
	}

	select {
	case errServer := <-serverErrCh:
		if errServer != nil {
			t.Fatalf("server error: %v", errServer)
		}
	case <-time.After(time.Second):
		t.Fatal("forwardResponsesWebsocket hung with nil data and error channels")
	}
}

func TestForwardResponsesWebsocketRejectsNilDataAfterErrorChannelCloses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.Close() }()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r

		errCh := make(chan *interfaces.ErrorMessage)
		close(errCh)
		handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}}
		_, _, _, err = handler.forwardResponsesWebsocket(
			ctx,
			conn,
			func(...interface{}) {},
			nil,
			errCh,
			nil,
			"session-1",
			"session-1",
		)
		if !errors.Is(err, errResponsesWebsocketNilStreamChannels) {
			serverErrCh <- fmt.Errorf("forward error = %v, want nil stream channels", err)
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read websocket message: %v", errReadMessage)
	}
	if gjson.GetBytes(payload, "type").String() != wsEventTypeError {
		t.Fatalf("payload type = %s, want %s; payload=%s", gjson.GetBytes(payload, "type").String(), wsEventTypeError, payload)
	}

	select {
	case errServer := <-serverErrCh:
		if errServer != nil {
			t.Fatalf("server error: %v", errServer)
		}
	case <-time.After(time.Second):
		t.Fatal("forwardResponsesWebsocket hung after error channel closed with nil data")
	}
}

func TestForwardResponsesWebsocketForwardsErrorWithNilHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamErr := errors.New("upstream failed")
	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.Close() }()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r

		errCh := make(chan *interfaces.ErrorMessage, 1)
		errCh <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: upstreamErr}
		close(errCh)

		var cancelErr error
		handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}}
		_, _, _, err = handler.forwardResponsesWebsocket(
			ctx,
			conn,
			func(params ...interface{}) {
				if len(params) > 0 {
					if errParam, ok := params[0].(error); ok {
						cancelErr = errParam
					}
				}
			},
			nil,
			errCh,
			nil,
			"session-1",
			"session-1",
		)
		if err != nil {
			serverErrCh <- fmt.Errorf("forward error = %v, want nil", err)
			return
		}
		if !errors.Is(cancelErr, upstreamErr) {
			serverErrCh <- fmt.Errorf("cancel error = %v, want upstream error", cancelErr)
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read websocket message: %v", errReadMessage)
	}
	if gjson.GetBytes(payload, "type").String() != wsEventTypeError {
		t.Fatalf("payload type = %s, want %s; payload=%s", gjson.GetBytes(payload, "type").String(), wsEventTypeError, payload)
	}
	if got := gjson.GetBytes(payload, "status").Int(); got != http.StatusBadGateway {
		t.Fatalf("payload status = %d, want %d; payload=%s", got, http.StatusBadGateway, payload)
	}

	select {
	case errServer := <-serverErrCh:
		if errServer != nil {
			t.Fatalf("server error: %v", errServer)
		}
	case <-time.After(time.Second):
		t.Fatal("forwardResponsesWebsocket hung forwarding upstream error")
	}
}

func TestForwardResponsesWebsocketDoesNotRetryAfterEmittingPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamErr := websocketStatusError{status: http.StatusBadRequest, msg: `{"error":{"message":"No tool call found for function call output with call_id call-1.","param":"input","type":"invalid_request_error"}}`}
	firstRead := make(chan struct{})
	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.Close() }()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r

		data := make(chan []byte)
		errCh := make(chan *interfaces.ErrorMessage)
		forwardDone := make(chan error, 1)
		var cancelErr error
		handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}}
		go func() {
			_, _, _, errForward := handler.forwardResponsesWebsocket(
				ctx,
				conn,
				func(params ...interface{}) {
					if len(params) > 0 {
						if errParam, ok := params[0].(error); ok {
							cancelErr = errParam
						}
					}
				},
				data,
				errCh,
				nil,
				"session-1",
				"session-1",
			)
			forwardDone <- errForward
		}()

		data <- []byte(`{"type":"response.created","response":{"id":"resp-1"}}`)
		<-firstRead
		errCh <- &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: upstreamErr}
		close(data)
		close(errCh)

		errForward := <-forwardDone
		if errForward != nil {
			serverErrCh <- fmt.Errorf("forward error = %v, want nil", errForward)
			return
		}
		if !errors.Is(cancelErr, upstreamErr) {
			serverErrCh <- fmt.Errorf("cancel error = %v, want upstream error", cancelErr)
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read first websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != "response.created" {
		t.Fatalf("first payload type = %s, want response.created; payload=%s", got, payload)
	}
	close(firstRead)

	_, payload, errReadMessage = conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read error websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeError {
		t.Fatalf("payload type = %s, want %s; payload=%s", got, wsEventTypeError, payload)
	}
	if got := gjson.GetBytes(payload, "error.message").String(); !strings.Contains(got, "No tool call found") {
		t.Fatalf("error message = %q, want upstream tool-call error; payload=%s", got, payload)
	}

	select {
	case errServer := <-serverErrCh:
		if errServer != nil {
			t.Fatalf("server error: %v", errServer)
		}
	case <-time.After(time.Second):
		t.Fatal("forwardResponsesWebsocket hung forwarding post-payload error")
	}
}

func TestForwardResponsesWebsocketDoesNotRetryDataErrorAfterEmittingPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	firstRead := make(chan struct{})
	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.Close() }()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r

		data := make(chan []byte)
		errCh := make(chan *interfaces.ErrorMessage)
		forwardDone := make(chan error, 1)
		cancelCalled := false
		var cancelErr error
		handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}}
		go func() {
			_, _, _, errForward := handler.forwardResponsesWebsocket(
				ctx,
				conn,
				func(params ...interface{}) {
					cancelCalled = true
					if len(params) > 0 {
						if errParam, ok := params[0].(error); ok {
							cancelErr = errParam
						}
					}
				},
				data,
				errCh,
				nil,
				"session-1",
				"session-1",
			)
			forwardDone <- errForward
		}()

		data <- []byte(`{"type":"response.created","response":{"id":"resp-1"}}`)
		<-firstRead
		data <- []byte(`{"type":"error","status":400,"error":{"message":"No tool call found for function call output with call_id call-1.","param":"input","type":"invalid_request_error"}}`)
		close(data)
		close(errCh)

		errForward := <-forwardDone
		if errForward != nil {
			serverErrCh <- fmt.Errorf("forward error = %v, want nil", errForward)
			return
		}
		if !cancelCalled || cancelErr != nil {
			serverErrCh <- fmt.Errorf("cancel called=%v err=%v, want called with nil", cancelCalled, cancelErr)
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read first websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != "response.created" {
		t.Fatalf("first payload type = %s, want response.created; payload=%s", got, payload)
	}
	close(firstRead)

	_, payload, errReadMessage = conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read data error websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeError {
		t.Fatalf("payload type = %s, want %s; payload=%s", got, wsEventTypeError, payload)
	}
	if got := gjson.GetBytes(payload, "error.message").String(); !strings.Contains(got, "No tool call found") {
		t.Fatalf("error message = %q, want upstream tool-call error; payload=%s", got, payload)
	}

	select {
	case errServer := <-serverErrCh:
		if errServer != nil {
			t.Fatalf("server error: %v", errServer)
		}
	case <-time.After(time.Second):
		t.Fatal("forwardResponsesWebsocket hung forwarding post-payload data error")
	}
}

func TestResponsesWebsocketPayloadShouldRetryFullTranscript(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{
			name:    "previous response not found",
			payload: []byte(`{"type":"error","status":400,"error":{"code":"previous_response_not_found","message":"Previous response with id 'resp_0806d41b86f2084b016a1908c1edac819181dc011e6fffd7ce' not found.","param":"previous_response_id","type":"invalid_request_error"}}`),
			want:    true,
		},
		{
			name:    "previous response not found message only",
			payload: []byte(`{"type":"error","status":400,"error":{"message":"Previous response with id 'resp_0806d41b86f2084b016a1908c1edac819181dc011e6fffd7ce' not found.","type":"invalid_request_error"}}`),
			want:    true,
		},
		{
			name:    "missing tool call",
			payload: []byte(`{"type":"error","status":400,"error":{"message":"No tool call found for function call output with call_id call_Rx1FW4RrRF9C1SyH2xxBVtEn.","param":"input","type":"invalid_request_error"}}`),
			want:    true,
		},
		{
			name:    "server error",
			payload: []byte(`{"type":"error","status":500,"error":{"code":"previous_response_not_found","param":"previous_response_id"}}`),
			want:    false,
		},
		{
			name:    "non error payload",
			payload: []byte(`{"type":"response.completed","response":{"id":"resp-1"}}`),
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responsesWebsocketPayloadShouldRetryFullTranscript(tt.payload); got != tt.want {
				t.Fatalf("responsesWebsocketPayloadShouldRetryFullTranscript() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResponsesWebsocketShouldRetryFullTranscriptPlainPreviousResponseError(t *testing.T) {
	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New("HTTP 400: Previous response with id 'resp_038d5107ec6cc78c016a1fb143ac088191b14e6ca3097c696e' not found."),
	}
	if !responsesWebsocketShouldRetryFullTranscript(errMsg) {
		t.Fatal("plain previous response not found error should retry full transcript")
	}
}

func TestForwardResponsesWebsocketLogsAttemptedResponseOnWriteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r

		data := make(chan []byte, 1)
		errCh := make(chan *interfaces.ErrorMessage)
		data <- []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"message\",\"id\":\"out-1\"}]}}\n\n")
		close(data)
		close(errCh)

		timelineLog := newWebsocketTimelineBuilder(maxResponsesWebsocketTimelineBytes)
		if errClose := conn.Close(); errClose != nil {
			serverErrCh <- errClose
			return
		}

		handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}}
		_, _, _, err = handler.forwardResponsesWebsocket(
			ctx,
			conn,
			func(...interface{}) {},
			data,
			errCh,
			&timelineLog,
			"session-1",
			"session-1",
		)
		if err == nil {
			serverErrCh <- errors.New("expected websocket write failure")
			return
		}
		if !strings.Contains(timelineLog.String(), "Event: websocket.response") {
			serverErrCh <- errors.New("websocket timeline did not capture attempted downstream response")
			return
		}
		if !strings.Contains(timelineLog.String(), "\"type\":\"response.completed\"") {
			serverErrCh <- errors.New("websocket timeline did not retain attempted payload")
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	if errServer := <-serverErrCh; errServer != nil {
		t.Fatalf("server error: %v", errServer)
	}
}

func TestResponsesWebsocketTimelineRecordsDisconnectEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)

	timelineCh := make(chan string, 1)
	router := gin.New()
	router.GET("/v1/responses/ws", func(c *gin.Context) {
		h.ResponsesWebsocket(c)
		timeline := ""
		if value, exists := c.Get(wsTimelineBodyKey); exists {
			if body, ok := value.([]byte); ok {
				timeline = string(body)
			}
		}
		timelineCh <- timeline
	})

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	closePayload := websocket.FormatCloseMessage(websocket.CloseGoingAway, "client closing")
	if err = conn.WriteControl(websocket.CloseMessage, closePayload, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("write close control: %v", err)
	}
	_ = conn.Close()

	select {
	case timeline := <-timelineCh:
		if !strings.Contains(timeline, "Event: websocket.disconnect") {
			t.Fatalf("websocket timeline missing disconnect event: %s", timeline)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket timeline")
	}
}

func TestResponsesWebsocketClosesOnCodexUpstreamDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketUpstreamDisconnectExecutor{subscribed: make(chan string, 1)}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)

	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	sessionID := "existing-session"
	executor.ensureDisconnectSession(sessionID)
	requestHeader := http.Header{"Session_id": []string{sessionID}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, requestHeader)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	waitForWebsocketSubscription(t, executor.subscribed, sessionID)

	executor.TriggerDisconnect(sessionID, errors.New("upstream disconnected"))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatalf("expected downstream websocket to close after upstream disconnect")
	}
}

func TestResponsesWebsocketSubscribesPayloadExecutionSessionDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketUpstreamDisconnectExecutor{subscribed: make(chan string, 4)}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-codex-ws-disconnect", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","prompt_cache_key":"conv-1","input":[{"type":"message","id":"msg-1"}]}`)); errWrite != nil {
		t.Fatalf("write first websocket message: %v", errWrite)
	}
	waitForWebsocketSubscription(t, executor.subscribed, "conv-1")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read first websocket message: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("first payload type = %s, want %s: %s", got, wsEventTypeCompleted, payload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","prompt_cache_key":"conv-1","input":[{"type":"message","id":"msg-2"}]}`)); errWrite != nil {
		t.Fatalf("write same session follow-up: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, errRead = conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read same session follow-up: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s: %s", got, wsEventTypeCompleted, payload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","prompt_cache_key":"conv-2","input":[{"type":"message","id":"msg-3"}]}`)); errWrite != nil {
		t.Fatalf("write after session switch: %v", errWrite)
	}
	waitForWebsocketSubscription(t, executor.subscribed, "conv-2")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, errRead = conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read after session switch reset: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("third payload type = %s, want %s: %s", got, wsEventTypeCompleted, payload)
	}

	executor.TriggerDisconnect("conv-1", errors.New("stale upstream disconnected"))
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","prompt_cache_key":"conv-2","input":[{"type":"message","id":"msg-4"}]}`)); errWrite != nil {
		t.Fatalf("write after stale session disconnect: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, errRead = conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read after stale session disconnect: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("fourth payload type = %s, want %s: %s", got, wsEventTypeCompleted, payload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","prompt_cache_key":"conv-1","input":[{"type":"message","id":"msg-5"}]}`)); errWrite != nil {
		t.Fatalf("write after switching back to stale session: %v", errWrite)
	}
	waitForWebsocketSubscription(t, executor.subscribed, "conv-1")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, errRead = conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read after switching back to stale session: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("fifth payload type = %s, want %s: %s", got, wsEventTypeCompleted, payload)
	}

	executor.TriggerDisconnect("conv-1", errors.New("active upstream disconnected"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, errRead = conn.ReadMessage()
	if errRead == nil {
		t.Fatalf("expected downstream websocket to close after resubscribed active payload session disconnect")
	}
}

func waitForWebsocketSubscription(t *testing.T, subscribed <-chan string, want string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-subscribed:
			if want == "" || got == want {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out waiting for websocket subscription %q", want)
		}
	}
}

func TestWebsocketUpstreamSupportsIncrementalInputForModel(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "auth-ws",
		Provider:   "test-provider",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	if !h.websocketUpstreamSupportsIncrementalInputForModel("test-model") {
		t.Fatalf("expected websocket-capable upstream for test-model")
	}
}

func TestWebsocketUpstreamSupportsIncrementalInputAllowsWebsocketAuth(t *testing.T) {
	if !websocketUpstreamSupportsIncrementalInput(
		map[string]string{"websockets": "true"},
		nil,
	) {
		t.Fatal("websocket auth should be treated as incremental-input capable")
	}
	if !websocketUpstreamSupportsIncrementalInput(
		nil,
		map[string]any{"websocket": true},
	) {
		t.Fatal("legacy websocket metadata should be treated as incremental-input capable")
	}
}

func TestCachedResponsesWebsocketIncrementalInputSupport(t *testing.T) {
	cache := make(map[string]bool)
	calls := 0
	resolve := func(modelName string) bool {
		calls++
		return modelName == "test-model"
	}

	if !cachedResponsesWebsocketIncrementalInputSupport(cache, "test-model", resolve) {
		t.Fatalf("expected cached lookup to return true")
	}
	if !cachedResponsesWebsocketIncrementalInputSupport(cache, "test-model", resolve) {
		t.Fatalf("expected cached lookup to return true on second call")
	}
	if calls != 1 {
		t.Fatalf("expected resolver to be called once, got %d", calls)
	}

	if cachedResponsesWebsocketIncrementalInputSupport(cache, "", resolve) {
		t.Fatalf("expected empty model lookup to return false")
	}
	if calls != 2 {
		t.Fatalf("expected empty model lookup to bypass cache, got %d resolver calls", calls)
	}
}

func TestResponsesWebsocketPrewarmHandledLocallyForSSEUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-sse", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		errClose := conn.Close()
		if errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","generate":false}`))
	if errWrite != nil {
		t.Fatalf("write prewarm websocket message: %v", errWrite)
	}

	_, createdPayload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read prewarm created message: %v", errReadMessage)
	}
	if gjson.GetBytes(createdPayload, "type").String() != "response.created" {
		t.Fatalf("created payload type = %s, want response.created", gjson.GetBytes(createdPayload, "type").String())
	}
	prewarmResponseID := gjson.GetBytes(createdPayload, "response.id").String()
	if prewarmResponseID == "" {
		t.Fatalf("prewarm response id is empty")
	}
	if executor.streamCalls != 0 {
		t.Fatalf("stream calls after prewarm = %d, want 0", executor.streamCalls)
	}

	_, completedPayload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read prewarm completed message: %v", errReadMessage)
	}
	if gjson.GetBytes(completedPayload, "type").String() != wsEventTypeCompleted {
		t.Fatalf("completed payload type = %s, want %s", gjson.GetBytes(completedPayload, "type").String(), wsEventTypeCompleted)
	}
	if gjson.GetBytes(completedPayload, "response.id").String() != prewarmResponseID {
		t.Fatalf("completed response id = %s, want %s", gjson.GetBytes(completedPayload, "response.id").String(), prewarmResponseID)
	}
	if gjson.GetBytes(completedPayload, "response.usage.total_tokens").Int() != 0 {
		t.Fatalf("prewarm total tokens = %d, want 0", gjson.GetBytes(completedPayload, "response.usage.total_tokens").Int())
	}

	secondRequest := fmt.Sprintf(`{"type":"response.create","previous_response_id":%q,"input":[{"type":"message","id":"msg-1"}]}`, prewarmResponseID)
	errWrite = conn.WriteMessage(websocket.TextMessage, []byte(secondRequest))
	if errWrite != nil {
		t.Fatalf("write follow-up websocket message: %v", errWrite)
	}

	_, upstreamPayload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read upstream completed message: %v", errReadMessage)
	}
	if gjson.GetBytes(upstreamPayload, "type").String() != wsEventTypeCompleted {
		t.Fatalf("upstream payload type = %s, want %s", gjson.GetBytes(upstreamPayload, "type").String(), wsEventTypeCompleted)
	}
	if executor.streamCalls != 1 {
		t.Fatalf("stream calls after follow-up = %d, want 1", executor.streamCalls)
	}
	if len(executor.payloads) != 1 {
		t.Fatalf("captured upstream payloads = %d, want 1", len(executor.payloads))
	}
	forwarded := executor.payloads[0]
	if gjson.GetBytes(forwarded, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id leaked upstream: %s", forwarded)
	}
	if gjson.GetBytes(forwarded, "generate").Exists() {
		t.Fatalf("generate leaked upstream: %s", forwarded)
	}
	if gjson.GetBytes(forwarded, "model").String() != "test-model" {
		t.Fatalf("forwarded model = %s, want test-model", gjson.GetBytes(forwarded, "model").String())
	}
	input := gjson.GetBytes(forwarded, "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected forwarded input: %s", forwarded)
	}
}

func TestResponsesWebsocketIgnoresResponseProcessedControlAck(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-sse", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1"}]}`)); errWrite != nil {
		t.Fatalf("write first create: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, firstPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read first completed: %v", errRead)
	}
	if got := gjson.GetBytes(firstPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("first payload type = %s, want %s: %s", got, wsEventTypeCompleted, firstPayload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.processed","response_id":"resp-upstream"}`)); errWrite != nil {
		t.Fatalf("write response.processed: %v", errWrite)
	}
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","input":[{"type":"message","id":"msg-2"}]}`)); errWrite != nil {
		t.Fatalf("write second create: %v", errWrite)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, secondPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read second completed: %v", errRead)
	}
	if got := gjson.GetBytes(secondPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s: %s", got, wsEventTypeCompleted, secondPayload)
	}
	if got := executor.streamCalls; got != 2 {
		t.Fatalf("stream calls = %d, want 2", got)
	}
}

func TestResponsesWebsocketRetriesFullTranscriptWhenIncrementalToolOutputIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketRetryFullTranscriptExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "auth-ws",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"websockets": "true",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)); errWrite != nil {
		t.Fatalf("write first create: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, firstPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read first completed: %v", errRead)
	}
	if got := gjson.GetBytes(firstPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("first payload type = %s, want %s: %s", got, wsEventTypeCompleted, firstPayload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call_Rx1FW4RrRF9C1SyH2xxBVtEn","output":"ok"},{"type":"message","id":"msg-2","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)); errWrite != nil {
		t.Fatalf("write second create: %v", errWrite)
	}
	_, secondPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read retry completed: %v", errRead)
	}
	if got := gjson.GetBytes(secondPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s: %s", got, wsEventTypeCompleted, secondPayload)
	}
	if gjson.GetBytes(secondPayload, "error").Exists() {
		t.Fatalf("fallback should not forward upstream tool-output error: %s", secondPayload)
	}

	payloads := executor.payloads
	if len(payloads) != 3 {
		t.Fatalf("executor payload count = %d, want 3", len(payloads))
	}
	if got := gjson.GetBytes(payloads[1], "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("incremental attempt previous_response_id = %q, want resp-1; body=%s", got, payloads[1])
	}
	retryPayload := payloads[2]
	if got := gjson.GetBytes(retryPayload, "previous_response_id"); got.Exists() {
		t.Fatalf("full transcript retry must omit previous_response_id: %s", retryPayload)
	}
	items := gjson.GetBytes(retryPayload, "input").Array()
	if len(items) != 4 {
		t.Fatalf("retry input len = %d, want full transcript with tool call/output/message: %s", len(items), retryPayload)
	}
	if items[1].Get("type").String() != "function_call" ||
		items[1].Get("call_id").String() != "call_Rx1FW4RrRF9C1SyH2xxBVtEn" {
		t.Fatalf("retry should include original tool call before output: %s", retryPayload)
	}
	if items[2].Get("type").String() != "function_call_output" ||
		items[2].Get("call_id").String() != "call_Rx1FW4RrRF9C1SyH2xxBVtEn" {
		t.Fatalf("retry should include function call output after call: %s", retryPayload)
	}
}

func TestResponsesWebsocketRetriesFullTranscriptWhenIncrementalDataErrorIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketRetryFullTranscriptExecutor{
		secondCallPayload: []byte(`{"type":"error","status":400,"error":{"code":"previous_response_not_found","message":"Previous response with id 'resp_0806d41b86f2084b016a1908c1edac819181dc011e6fffd7ce' not found.","param":"previous_response_id","type":"invalid_request_error"}}`),
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "auth-ws-data-error",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"websockets": "true",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)); errWrite != nil {
		t.Fatalf("write first create: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, firstPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read first completed: %v", errRead)
	}
	if got := gjson.GetBytes(firstPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("first payload type = %s, want %s: %s", got, wsEventTypeCompleted, firstPayload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call_Rx1FW4RrRF9C1SyH2xxBVtEn","output":"ok"},{"type":"message","id":"msg-2","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)); errWrite != nil {
		t.Fatalf("write second create: %v", errWrite)
	}
	_, secondPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read retry completed: %v", errRead)
	}
	if got := gjson.GetBytes(secondPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s: %s", got, wsEventTypeCompleted, secondPayload)
	}
	if gjson.GetBytes(secondPayload, "error").Exists() {
		t.Fatalf("fallback should not forward upstream websocket error payload: %s", secondPayload)
	}

	payloads := executor.Payloads()
	if len(payloads) != 3 {
		t.Fatalf("executor payload count = %d, want 3", len(payloads))
	}
	if got := gjson.GetBytes(payloads[1], "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("incremental attempt previous_response_id = %q, want resp-1; body=%s", got, payloads[1])
	}
	retryPayload := payloads[2]
	if got := gjson.GetBytes(retryPayload, "previous_response_id"); got.Exists() {
		t.Fatalf("full transcript retry must omit previous_response_id: %s", retryPayload)
	}
	items := gjson.GetBytes(retryPayload, "input").Array()
	if len(items) != 4 {
		t.Fatalf("retry input len = %d, want full transcript with tool call/output/message: %s", len(items), retryPayload)
	}
	if items[1].Get("type").String() != "function_call" ||
		items[1].Get("call_id").String() != "call_Rx1FW4RrRF9C1SyH2xxBVtEn" {
		t.Fatalf("retry should include original tool call before output: %s", retryPayload)
	}
	if items[2].Get("type").String() != "function_call_output" ||
		items[2].Get("call_id").String() != "call_Rx1FW4RrRF9C1SyH2xxBVtEn" {
		t.Fatalf("retry should include function call output after call: %s", retryPayload)
	}
}

func TestResponsesWebsocketContinuesIncrementalAfterSuccessfulFullTranscriptRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketRetryFullTranscriptExecutor{
		secondCallPayload: []byte(`{"type":"error","status":400,"error":{"code":"previous_response_not_found","message":"Previous response with id 'resp-1' not found.","param":"previous_response_id","type":"invalid_request_error"}}`),
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "auth-ws-disable-incremental",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"websockets": "true",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)); errWrite != nil {
		t.Fatalf("write first create: %v", errWrite)
	}
	_, firstPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read first completed: %v", errRead)
	}
	if got := gjson.GetBytes(firstPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("first payload type = %s, want %s: %s", got, wsEventTypeCompleted, firstPayload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call_Rx1FW4RrRF9C1SyH2xxBVtEn","output":"ok"},{"type":"message","id":"msg-2","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)); errWrite != nil {
		t.Fatalf("write second create: %v", errWrite)
	}
	_, secondPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read second completed: %v", errRead)
	}
	if got := gjson.GetBytes(secondPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s: %s", got, wsEventTypeCompleted, secondPayload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp-2","input":[{"type":"message","id":"msg-3","role":"user","content":[{"type":"input_text","text":"again"}]}]}`)); errWrite != nil {
		t.Fatalf("write third create: %v", errWrite)
	}
	_, thirdPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read third completed: %v", errRead)
	}
	if got := gjson.GetBytes(thirdPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("third payload type = %s, want %s: %s", got, wsEventTypeCompleted, thirdPayload)
	}

	payloads := executor.Payloads()
	if len(payloads) != 4 {
		t.Fatalf("executor payload count = %d, want 4", len(payloads))
	}
	if got := gjson.GetBytes(payloads[1], "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("second request should make one incremental attempt, got previous_response_id=%q; body=%s", got, payloads[1])
	}
	if got := gjson.GetBytes(payloads[2], "previous_response_id"); got.Exists() {
		t.Fatalf("second request retry must be full transcript: %s", payloads[2])
	}
	if got := gjson.GetBytes(payloads[3], "previous_response_id").String(); got != "resp-2" {
		t.Fatalf("third request should resume incremental with latest response id, got previous_response_id=%q; body=%s", got, payloads[3])
	}
}

func TestResponsesWebsocketSkipsIncrementalWhenPreviousResponseIDIsStale(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "auth-ws-stale-prev",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"websockets": "true",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)); errWrite != nil {
		t.Fatalf("write first create: %v", errWrite)
	}
	_, firstPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read first completed: %v", errRead)
	}
	if got := gjson.GetBytes(firstPayload, "response.id").String(); got != "resp-upstream" {
		t.Fatalf("first response id = %q, want resp-upstream; payload=%s", got, firstPayload)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp-stale","input":[{"type":"message","id":"msg-2","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)); errWrite != nil {
		t.Fatalf("write second create: %v", errWrite)
	}
	_, secondPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read second completed: %v", errRead)
	}
	if got := gjson.GetBytes(secondPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s: %s", got, wsEventTypeCompleted, secondPayload)
	}

	payloads := executor.payloads
	if len(payloads) != 2 {
		t.Fatalf("executor payload count = %d, want 2", len(payloads))
	}
	if got := gjson.GetBytes(payloads[1], "previous_response_id"); got.Exists() {
		t.Fatalf("stale previous_response_id should not be forwarded upstream: %s", payloads[1])
	}
	input := gjson.GetBytes(payloads[1], "input").Array()
	if len(input) != 3 {
		t.Fatalf("second payload input len = %d, want full transcript: %s", len(input), payloads[1])
	}
}

func TestWebsocketClientAddressUsesGinClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(recorder)
	if err := engine.SetTrustedProxies([]string{"0.0.0.0/0", "::/0"}); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/responses/ws", nil)
	req.RemoteAddr = "172.18.0.1:34282"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	c.Request = req

	if got := websocketClientAddress(c); got != strings.TrimSpace(c.ClientIP()) {
		t.Fatalf("websocketClientAddress = %q, ClientIP = %q", got, c.ClientIP())
	}
}

func TestWebsocketClientAddressReturnsEmptyForNilContext(t *testing.T) {
	if got := websocketClientAddress(nil); got != "" {
		t.Fatalf("websocketClientAddress(nil) = %q, want empty", got)
	}
}

func TestResponsesWebsocketPinsOnlyWebsocketCapableAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	selector := &orderedWebsocketSelector{order: []string{"auth-sse", "auth-ws"}}
	executor := &websocketAuthCaptureExecutor{}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	authSSE := &coreauth.Auth{ID: "auth-sse", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), authSSE); err != nil {
		t.Fatalf("Register SSE auth: %v", err)
	}
	authWS := &coreauth.Auth{
		ID:         "auth-ws",
		Provider:   executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), authWS); err != nil {
		t.Fatalf("Register websocket auth: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(authSSE.ID, authSSE.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	registry.GetGlobalRegistry().RegisterClient(authWS.ID, authWS.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authSSE.ID)
		registry.GetGlobalRegistry().UnregisterClient(authWS.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	requests := []string{
		`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.create","input":[{"type":"message","id":"msg-2"}]}`,
	}
	for i := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(requests[i])); errWrite != nil {
			t.Fatalf("write websocket message %d: %v", i+1, errWrite)
		}
		_, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			t.Fatalf("read websocket message %d: %v", i+1, errReadMessage)
		}
		if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
			t.Fatalf("message %d payload type = %s, want %s", i+1, got, wsEventTypeCompleted)
		}
	}

	if got := executor.AuthIDs(); len(got) != 2 || got[0] != "auth-sse" || got[1] != "auth-ws" {
		t.Fatalf("selected auth IDs = %v, want [auth-sse auth-ws]", got)
	}
}

func TestResponsesWebsocketUnpinsAuthAfterAuthWideFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	selector := &orderedWebsocketSelector{order: []string{"auth-ws", "auth-fallback"}}
	executor := &websocketPinnedAuthFailureExecutor{}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	authWS := &coreauth.Auth{
		ID:         "auth-ws",
		Provider:   executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	authFallback := &coreauth.Auth{
		ID:         "auth-fallback",
		Provider:   executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), authWS); err != nil {
		t.Fatalf("Register websocket auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), authFallback); err != nil {
		t.Fatalf("Register fallback auth: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(authWS.ID, authWS.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	registry.GetGlobalRegistry().RegisterClient(authFallback.ID, authFallback.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authWS.ID)
		registry.GetGlobalRegistry().UnregisterClient(authFallback.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	header := http.Header{"X-Session-ID": []string{"ws-auth-failure-session"}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	requests := []string{
		`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.create","previous_response_id":"resp-upstream","input":[{"type":"message","id":"msg-2"}]}`,
		`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-3"}]}`,
	}
	wantTypes := []string{wsEventTypeCompleted, wsEventTypeError, wsEventTypeCompleted}
	for i := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(requests[i])); errWrite != nil {
			t.Fatalf("write websocket message %d: %v", i+1, errWrite)
		}
		_, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			t.Fatalf("read websocket message %d: %v", i+1, errReadMessage)
		}
		if got := gjson.GetBytes(payload, "type").String(); got != wantTypes[i] {
			t.Fatalf("message %d payload type = %s, want %s; payload=%s", i+1, got, wantTypes[i], payload)
		}
	}

	if got := executor.AuthIDs(); len(got) != 3 || got[0] != "auth-ws" || got[1] != "auth-ws" || got[2] != "auth-fallback" {
		t.Fatalf("selected auth IDs = %v, want [auth-ws auth-ws auth-fallback]", got)
	}
	resets := executor.ResetIDs()
	if len(resets) == 0 {
		t.Fatal("expected pinned auth failure to reset execution session")
	}
	if got := resets[0]; got != "ws-auth-failure-session" {
		t.Fatalf("reset session id = %q, want ws-auth-failure-session", got)
	}
}

func TestResponsesWebsocketClearsPinnedAuthBeforeNormalizingIncrementalRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	selector := &orderedWebsocketSelector{order: []string{"auth-ws", "auth-fallback"}}
	executor := &websocketAuthCaptureExecutor{}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	authWS := &coreauth.Auth{
		ID:         "auth-ws",
		Provider:   executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	authFallback := &coreauth.Auth{
		ID:         "auth-fallback",
		Provider:   executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), authWS); err != nil {
		t.Fatalf("Register websocket auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), authFallback); err != nil {
		t.Fatalf("Register fallback auth: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(authWS.ID, authWS.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	registry.GetGlobalRegistry().RegisterClient(authFallback.ID, authFallback.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authWS.ID)
		registry.GetGlobalRegistry().UnregisterClient(authFallback.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1"}]}`)); errWrite != nil {
		t.Fatalf("write first websocket message: %v", errWrite)
	}
	if _, payload, errReadMessage := conn.ReadMessage(); errReadMessage != nil {
		t.Fatalf("read first websocket message: %v", errReadMessage)
	} else if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("first payload type = %s, want %s; payload=%s", got, wsEventTypeCompleted, payload)
	}

	updatedWS := authWS.Clone()
	updatedWS.Unavailable = true
	updatedWS.NextRetryAfter = time.Now().Add(time.Hour)
	if _, err := manager.Update(coreauth.WithSkipPersist(context.Background()), updatedWS); err != nil {
		t.Fatalf("mark websocket auth unavailable: %v", err)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp-upstream","input":[{"type":"message","id":"msg-2"}]}`)); errWrite != nil {
		t.Fatalf("write second websocket message: %v", errWrite)
	}
	if _, payload, errReadMessage := conn.ReadMessage(); errReadMessage != nil {
		t.Fatalf("read second websocket message: %v", errReadMessage)
	} else if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s; payload=%s", got, wsEventTypeCompleted, payload)
	}

	if got := executor.AuthIDs(); len(got) != 2 || got[0] != "auth-ws" || got[1] != "auth-fallback" {
		t.Fatalf("selected auth IDs = %v, want [auth-ws auth-fallback]", got)
	}
	payloads := executor.Payloads()
	if len(payloads) != 2 {
		t.Fatalf("captured payload count = %d, want 2", len(payloads))
	}
	if gjson.GetBytes(payloads[1], "previous_response_id").Exists() {
		t.Fatalf("fallback auth request must not preserve previous_response_id: %s", payloads[1])
	}
}

func TestNormalizeResponsesWebsocketRequestTreatsTranscriptReplacementAsReset(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"},{"type":"function_call","id":"fc-1","call_id":"call-1"},{"type":"function_call_output","id":"tool-out-1","call_id":"call-1"},{"type":"message","id":"assistant-1","role":"assistant"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1","role":"assistant"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"function_call","id":"fc-compact","call_id":"call-1","name":"tool"},{"type":"message","id":"msg-2"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must not exist in transcript replacement mode")
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 2 {
		t.Fatalf("replacement input len = %d, want 2: %s", len(items), normalized)
	}
	if items[0].Get("id").String() != "fc-compact" || items[1].Get("id").String() != "msg-2" {
		t.Fatalf("replacement transcript was not preserved as-is: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match replacement request")
	}
}

func TestNormalizeResponsesWebsocketTranscriptReplacementNormalizesCodexInputItems(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"},{"type":"function_call","id":"fc-1","call_id":"call-1"},{"type":"function_call_output","id":"tool-out-1","call_id":"call-1"},{"type":"message","id":"assistant-1","role":"assistant"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1","role":"assistant"}
	]`)
	raw := []byte(`{
		"type":"response.create",
		"input":[
			{"type":"function_call","id":"fc-compact","call_id":"call-1","name":"tool"},
			{"type":"mcp_tool_call_output","call_id":"call-mcp","output":{"structuredContent":{"ok":true},"content":[]}},
			{"type":"compaction_trigger","reason":"token_limit"},
			{"type":"message","id":"msg-2"}
		]
	}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 3 {
		t.Fatalf("replacement input len = %d, want 3 after dropping compaction_trigger: %s", len(items), normalized)
	}
	if got := items[1].Get("type").String(); got != "function_call_output" {
		t.Fatalf("mcp output type = %q, want function_call_output: %s", got, normalized)
	}
	if got := items[1].Get("output").String(); got != "Wall time: 0.0000 seconds\nOutput:\n"+`{"ok":true}` {
		t.Fatalf("mcp output = %q, want structured content JSON: %s", got, normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match replacement request")
	}
}

func TestNormalizeResponsesWebsocketRequestDoesNotTreatDeveloperMessageAsReplacement(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1","role":"assistant"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"message","id":"dev-1","role":"developer"},{"type":"message","id":"msg-2"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 4 {
		t.Fatalf("merged input len = %d, want 4: %s", len(items), normalized)
	}
	if items[0].Get("id").String() != "msg-1" ||
		items[1].Get("id").String() != "assistant-1" ||
		items[2].Get("id").String() != "dev-1" ||
		items[3].Get("id").String() != "msg-2" {
		t.Fatalf("developer follow-up should preserve merge behavior: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match merged request")
	}
}

func TestNormalizeResponsesWebsocketRequestDropsDuplicateFunctionCallsByCallID(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"function_call","id":"fc-1","call_id":"call-1"},{"type":"function_call_output","id":"tool-out-1","call_id":"call-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1","name":"tool"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"message","id":"msg-2"}]}`)

	normalized, _, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}

	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 3 {
		t.Fatalf("merged input len = %d, want 3: %s", len(items), normalized)
	}
	if items[0].Get("id").String() != "fc-1" ||
		items[1].Get("id").String() != "tool-out-1" ||
		items[2].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected merged input order: %s", normalized)
	}
}

func TestNormalizeResponsesWebsocketRequestTreatsCustomToolTranscriptReplacementAsReset(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"},{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"apply_patch"},{"type":"custom_tool_call_output","id":"tool-out-1","call_id":"call-1"},{"type":"message","id":"assistant-1","role":"assistant"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1","role":"assistant"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"custom_tool_call","id":"ctc-compact","call_id":"call-1","name":"apply_patch"},{"type":"custom_tool_call_output","id":"tool-out-compact","call_id":"call-1"},{"type":"message","id":"msg-2"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must not exist in transcript replacement mode")
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 3 {
		t.Fatalf("replacement input len = %d, want 3: %s", len(items), normalized)
	}
	if items[0].Get("id").String() != "ctc-compact" ||
		items[1].Get("id").String() != "tool-out-compact" ||
		items[2].Get("id").String() != "msg-2" {
		t.Fatalf("replacement transcript was not preserved as-is: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match replacement request")
	}
}

func TestNormalizeResponsesWebsocketRequestTreatsLocalShellTranscriptReplacementAsReset(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"},{"type":"local_shell_call","id":"lsc-1","call_id":"call-shell","status":"completed","action":{"type":"exec","command":["pwd"]}},{"type":"function_call_output","id":"tool-out-1","call_id":"call-shell"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1","role":"assistant"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"local_shell_call","id":"lsc-compact","call_id":"call-shell","status":"completed","action":{"type":"exec","command":["ls"]}},{"type":"function_call_output","id":"tool-out-compact","call_id":"call-shell"},{"type":"message","id":"msg-2"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must not exist in transcript replacement mode")
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 3 {
		t.Fatalf("replacement input len = %d, want 3: %s", len(items), normalized)
	}
	if items[0].Get("id").String() != "lsc-compact" ||
		items[1].Get("id").String() != "tool-out-compact" ||
		items[2].Get("id").String() != "msg-2" {
		t.Fatalf("replacement transcript was not preserved as-is: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match replacement request")
	}
}

func TestNormalizeResponsesWebsocketRequestTreatsToolSearchTranscriptReplacementAsReset(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"},{"type":"tool_search_call","call_id":"search-1","execution":"client","arguments":{"query":"calendar"}},{"type":"tool_search_output","call_id":"search-1","status":"completed","execution":"client","tools":[]}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1","role":"assistant"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"tool_search_call","call_id":"search-compact","execution":"client","arguments":{"query":"mail"}},{"type":"tool_search_output","call_id":"search-compact","status":"completed","execution":"client","tools":[]},{"type":"message","id":"msg-2"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must not exist in transcript replacement mode")
	}
	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 3 {
		t.Fatalf("replacement input len = %d, want 3: %s", len(items), normalized)
	}
	if items[0].Get("call_id").String() != "search-compact" ||
		items[1].Get("call_id").String() != "search-compact" ||
		items[2].Get("id").String() != "msg-2" {
		t.Fatalf("replacement transcript was not preserved as-is: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match replacement request")
	}
}

func TestNormalizeResponsesWebsocketRequestDropsDuplicateCustomToolCallsByCallID(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"apply_patch"},{"type":"custom_tool_call_output","id":"tool-out-1","call_id":"call-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"apply_patch"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"message","id":"msg-2"}]}`)

	normalized, _, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}

	items := gjson.GetBytes(normalized, "input").Array()
	if len(items) != 3 {
		t.Fatalf("merged input len = %d, want 3: %s", len(items), normalized)
	}
	if items[0].Get("id").String() != "ctc-1" ||
		items[1].Get("id").String() != "tool-out-1" ||
		items[2].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected merged input order: %s", normalized)
	}
}

func TestResponsesWebsocketCompactionResetsTurnStateOnCustomToolTranscriptReplacement(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketCompactionCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-sse", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	router.POST("/v1/responses/compact", h.Compact)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	requests := []string{
		`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.create","input":[{"type":"custom_tool_call_output","call_id":"call-1","id":"tool-out-1"}]}`,
	}
	for i := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(requests[i])); errWrite != nil {
			t.Fatalf("write websocket message %d: %v", i+1, errWrite)
		}
		_, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			t.Fatalf("read websocket message %d: %v", i+1, errReadMessage)
		}
		if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
			t.Fatalf("message %d payload type = %s, want %s", i+1, got, wsEventTypeCompleted)
		}
	}

	compactResp, errPost := server.Client().Post(
		server.URL+"/v1/responses/compact",
		"application/json",
		strings.NewReader(`{"model":"test-model","input":[{"type":"message","id":"summary-1"}]}`),
	)
	if errPost != nil {
		t.Fatalf("compact request failed: %v", errPost)
	}
	if errClose := compactResp.Body.Close(); errClose != nil {
		t.Fatalf("close compact response body: %v", errClose)
	}
	if compactResp.StatusCode != http.StatusOK {
		t.Fatalf("compact status = %d, want %d", compactResp.StatusCode, http.StatusOK)
	}

	postCompact := `{"type":"response.create","input":[{"type":"custom_tool_call","id":"ctc-compact","call_id":"call-1","name":"apply_patch"},{"type":"custom_tool_call_output","id":"tool-out-compact","call_id":"call-1"},{"type":"message","id":"msg-2"}]}`
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(postCompact)); errWrite != nil {
		t.Fatalf("write post-compact websocket message: %v", errWrite)
	}
	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read post-compact websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("post-compact payload type = %s, want %s", got, wsEventTypeCompleted)
	}

	executor.mu.Lock()
	defer executor.mu.Unlock()

	if executor.compactPayload == nil {
		t.Fatalf("compact payload was not captured")
	}
	if len(executor.streamPayloads) != 3 {
		t.Fatalf("stream payload count = %d, want 3", len(executor.streamPayloads))
	}

	merged := executor.streamPayloads[2]
	items := gjson.GetBytes(merged, "input").Array()
	if len(items) != 3 {
		t.Fatalf("merged input len = %d, want 3: %s", len(items), merged)
	}
	if items[0].Get("id").String() != "ctc-compact" ||
		items[1].Get("id").String() != "tool-out-compact" ||
		items[2].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected post-compact input order: %s", merged)
	}
	if items[0].Get("call_id").String() != "call-1" {
		t.Fatalf("post-compact custom tool call id = %s, want call-1", items[0].Get("call_id").String())
	}
}

func TestResponsesWebsocketCompactionResetsTurnStateOnTranscriptReplacement(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketCompactionCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-sse", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	router.POST("/v1/responses/compact", h.Compact)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	requests := []string{
		`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.create","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`,
	}
	for i := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(requests[i])); errWrite != nil {
			t.Fatalf("write websocket message %d: %v", i+1, errWrite)
		}
		_, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			t.Fatalf("read websocket message %d: %v", i+1, errReadMessage)
		}
		if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
			t.Fatalf("message %d payload type = %s, want %s", i+1, got, wsEventTypeCompleted)
		}
	}

	compactResp, errPost := server.Client().Post(
		server.URL+"/v1/responses/compact",
		"application/json",
		strings.NewReader(`{"model":"test-model","input":[{"type":"message","id":"summary-1"}]}`),
	)
	if errPost != nil {
		t.Fatalf("compact request failed: %v", errPost)
	}
	if errClose := compactResp.Body.Close(); errClose != nil {
		t.Fatalf("close compact response body: %v", errClose)
	}
	if compactResp.StatusCode != http.StatusOK {
		t.Fatalf("compact status = %d, want %d", compactResp.StatusCode, http.StatusOK)
	}

	// Simulate a post-compaction client turn that replaces local history with a compacted transcript.
	// The websocket handler must treat this as a state reset, not append it to stale pre-compaction state.
	postCompact := `{"type":"response.create","input":[{"type":"function_call","id":"fc-compact","call_id":"call-1","name":"tool"},{"type":"message","id":"msg-2"}]}`
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(postCompact)); errWrite != nil {
		t.Fatalf("write post-compact websocket message: %v", errWrite)
	}
	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read post-compact websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("post-compact payload type = %s, want %s", got, wsEventTypeCompleted)
	}

	executor.mu.Lock()
	defer executor.mu.Unlock()

	if executor.compactPayload == nil {
		t.Fatalf("compact payload was not captured")
	}
	if len(executor.streamPayloads) != 3 {
		t.Fatalf("stream payload count = %d, want 3", len(executor.streamPayloads))
	}

	merged := executor.streamPayloads[2]
	items := gjson.GetBytes(merged, "input").Array()
	if len(items) != 3 {
		t.Fatalf("merged input len = %d, want 3: %s", len(items), merged)
	}
	if items[0].Get("type").String() != "function_call" || items[0].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected post-compact call item: %s", merged)
	}
	if items[1].Get("type").String() != "function_call_output" ||
		items[1].Get("call_id").String() != "call-1" ||
		items[1].Get("output").String() != "aborted" {
		t.Fatalf("unexpected post-compact synthesized output: %s", merged)
	}
	if items[2].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected post-compact input order: %s", merged)
	}
}

func TestResponsesWebsocketUsesExplicitExecutionSessionIDFromPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketExecutionSessionCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-ws-session", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","prompt_cache_key":"conv-1","input":[{"type":"message","id":"msg-1"}]}`)); errWrite != nil {
		t.Fatalf("write websocket message: %v", errWrite)
	}
	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("payload type = %s, want %s", got, wsEventTypeCompleted)
	}

	if got := executor.ExecutionSessionIDs(); len(got) != 1 || got[0] != "conv-1" {
		t.Fatalf("execution session IDs = %#v, want [conv-1]", got)
	}
}

func TestResponsesExplicitExecutionSessionIDUsesOfficialHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		body    []byte
		want    string
	}{
		{
			name: "official session header",
			headers: map[string]string{
				"session-id": "official-session",
				"thread-id":  "official-thread",
			},
			want: "official-thread",
		},
		{
			name: "official thread fallback",
			headers: map[string]string{
				"thread-id": "official-thread",
			},
			want: "official-thread",
		},
		{
			name: "turn metadata thread fallback",
			headers: map[string]string{
				"X-Codex-Turn-Metadata": `{"thread_id":"turn-thread"}`,
			},
			want: "turn-thread",
		},
		{
			name: "payload prompt cache fallback",
			body: []byte(`{"prompt_cache_key":"body-cache"}`),
			want: "body-cache",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/responses/ws", nil)
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			if got := responsesExplicitExecutionSessionID(req, tc.body); got != tc.want {
				t.Fatalf("responsesExplicitExecutionSessionID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponsesWebsocketSwitchesExecutionSessionByPayloadAndResetsTranscript(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketExecutionSessionCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-ws-session-switch", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","prompt_cache_key":"conv-1","input":[{"type":"message","id":"msg-1"}]}`)); errWrite != nil {
		t.Fatalf("write first websocket message: %v", errWrite)
	}
	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read first websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("first payload type = %s, want %s", got, wsEventTypeCompleted)
	}

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","prompt_cache_key":"conv-2","input":[{"type":"message","id":"msg-2"}]}`)); errWrite != nil {
		t.Fatalf("write second websocket message: %v", errWrite)
	}
	_, payload, errReadMessage = conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read second websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s", got, wsEventTypeCompleted)
	}

	if got := executor.ExecutionSessionIDs(); len(got) != 2 || got[0] != "conv-1" || got[1] != "conv-2" {
		t.Fatalf("execution session IDs = %#v, want [conv-1 conv-2]", got)
	}
	if got := executor.ResetIDs(); len(got) != 1 || got[0] != "conv-1" {
		t.Fatalf("reset session IDs = %#v, want [conv-1]", got)
	}

	payloads := executor.Payloads()
	if len(payloads) != 2 {
		t.Fatalf("captured payload count = %d, want 2", len(payloads))
	}
	secondInput := gjson.GetBytes(payloads[1], "input").Array()
	if len(secondInput) != 1 {
		t.Fatalf("second upstream input len = %d, want 1: %s", len(secondInput), payloads[1])
	}
	if got := secondInput[0].Get("id").String(); got != "msg-2" {
		t.Fatalf("second upstream input id = %s, want msg-2", got)
	}
}

func TestResponsesWebsocketRecordsToolCallsWithoutHandshakeSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	outputCache := newWebsocketToolOutputCache(time.Minute, 10)
	callCache := newWebsocketToolOutputCache(time.Minute, 10)
	restoreCaches := replaceDefaultWebsocketToolCachesForTest(outputCache, callCache, newWebsocketToolSessionRefCounter())
	t.Cleanup(restoreCaches)

	executor := &websocketCompactionCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-ws-tool-cache", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","input":[{"type":"message","id":"msg-1"}]}`)); errWrite != nil {
		t.Fatalf("write websocket message: %v", errWrite)
	}
	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read websocket message: %v", errReadMessage)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("payload type = %s, want %s", got, wsEventTypeCompleted)
	}

	callCache.mu.Lock()
	defer callCache.mu.Unlock()

	if len(callCache.sessions) != 1 {
		t.Fatalf("recorded tool-call session count = %d, want 1", len(callCache.sessions))
	}
	for _, session := range callCache.sessions {
		if session == nil {
			continue
		}
		if _, ok := session.outputs["call-1"]; ok {
			return
		}
	}
	t.Fatalf("expected cached tool call for call-1")
}
