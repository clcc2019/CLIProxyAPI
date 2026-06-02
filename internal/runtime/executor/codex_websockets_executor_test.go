package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body, "")

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := gjson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

func TestBuildCodexWebsocketRequestBodyRepairsMissingContext(t *testing.T) {
	wsReqBody := buildCodexWebsocketRequestBody([]byte(`{"model":"gpt-5-codex"}`), "")

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create; body=%s", got, wsReqBody)
	}
	input := gjson.GetBytes(wsReqBody, "input")
	if !input.IsArray() || len(input.Array()) != 0 {
		t.Fatalf("missing Responses context should be repaired with input=[]; body=%s", wsReqBody)
	}
}

func TestBuildCodexWebsocketRequestBodyRepairsNullInput(t *testing.T) {
	wsReqBody := buildCodexWebsocketRequestBody([]byte(`{"type":"response.create","model":"gpt-5-codex","input":null}`), "")

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create; body=%s", got, wsReqBody)
	}
	input := gjson.GetBytes(wsReqBody, "input")
	if !input.IsArray() || len(input.Array()) != 0 {
		t.Fatalf("null input should be repaired with input=[]; body=%s", wsReqBody)
	}
}

func TestBuildCodexWebsocketRetryWithoutPreviousResponseDropsExplicitID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRetryWithoutPreviousResponse(body, `{"turn_id":"turn-1"}`, time.UnixMilli(1234))

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id"); got.Exists() {
		t.Fatalf("retry body should omit previous_response_id: %s", wsReqBody)
	}
	if got := gjson.GetBytes(wsReqBody, "input.0.id").String(); got != "msg-1" {
		t.Fatalf("input item id = %q, want msg-1; body=%s", got, wsReqBody)
	}
	if got := gjson.GetBytes(wsReqBody, "client_metadata.x-codex-turn-metadata").String(); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("turn metadata = %q, want turn-1 metadata; body=%s", got, wsReqBody)
	}
	if got := gjson.GetBytes(wsReqBody, "client_metadata.x-codex-ws-stream-request-start-ms").String(); got != "1234" {
		t.Fatalf("stream start ms = %q, want 1234; body=%s", got, wsReqBody)
	}
}

func TestBuildCodexWebsocketSendRetryBodyDropsOnlyInternalPreviousResponseID(t *testing.T) {
	fullBody := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","id":"msg-1"},{"type":"message","id":"msg-2"}]}`)
	incrementalBody := []byte(`{"type":"response.create","model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2"}]}`)

	retryBody := buildCodexWebsocketSendRetryBody(fullBody, incrementalBody, "", time.UnixMilli(1234))

	if got := gjson.GetBytes(retryBody, "previous_response_id"); got.Exists() {
		t.Fatalf("internal retry body should omit previous_response_id: %s", retryBody)
	}
	if got := gjson.GetBytes(retryBody, "input.#").Int(); got != 2 {
		t.Fatalf("internal retry body input len = %d, want full transcript; body=%s", got, retryBody)
	}

	explicitBody := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2"}]}`)
	retryBody = buildCodexWebsocketSendRetryBody(explicitBody, incrementalBody, "", time.UnixMilli(1234))

	if got := gjson.GetBytes(retryBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("explicit retry body previous_response_id = %q, want resp-1; body=%s", got, retryBody)
	}
	if got := gjson.GetBytes(retryBody, "input.#").Int(); got != 1 {
		t.Fatalf("explicit retry body input len = %d, want original delta; body=%s", got, retryBody)
	}
}

func TestCodexShouldRetryWithoutPreviousResponseWhenContextCanBeReplayed(t *testing.T) {
	errorPayload := []byte(`{"type":"error","status":400,"error":{"code":"previous_response_not_found","param":"previous_response_id"}}`)
	noToolCallPayload := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"No tool call found for function call output with call_id call_Rx1FW4RrRF9C1SyH2xxBVtEn."}}`)
	fullBody := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","id":"msg-1"},{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
	internalIncremental := []byte(`{"type":"response.create","model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)

	if !codexShouldRetryWithoutPreviousResponse(fullBody, internalIncremental, errorPayload) {
		t.Fatal("internally compressed request should retry with full transcript")
	}
	if !codexShouldRetryWithoutPreviousResponse(fullBody, internalIncremental, noToolCallPayload) {
		t.Fatal("internally compressed tool output should retry with full transcript when upstream loses the tool call")
	}

	explicitMessageBody := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)
	if !codexShouldRetryWithoutPreviousResponse(explicitMessageBody, explicitMessageBody, errorPayload) {
		t.Fatal("explicit previous_response_id with standalone message context should retry without previous_response_id")
	}

	explicitBody := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
	if codexShouldRetryWithoutPreviousResponse(explicitBody, internalIncremental, errorPayload) {
		t.Fatal("explicit previous_response_id request must not retry by dropping previous_response_id")
	}
	if codexShouldRetryWithoutPreviousResponse(explicitBody, internalIncremental, noToolCallPayload) {
		t.Fatal("explicit previous_response_id tool output must not retry by dropping previous_response_id")
	}
}

func TestBuildCodexWebsocketRequestBodyIncludesClientMetadata(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body, `{"turn_id":"turn-1","sandbox":"none"}`)

	if got := gjson.GetBytes(wsReqBody, "client_metadata.x-codex-turn-metadata").String(); got != `{"turn_id":"turn-1","sandbox":"none"}` {
		t.Fatalf("client_metadata.x-codex-turn-metadata = %q, want %q", got, `{"turn_id":"turn-1","sandbox":"none"}`)
	}
}

func TestPrepareCodexWebsocketRequestBuildsSharedRequestState(t *testing.T) {
	resetCodexWindowStateStore()

	executor := NewCodexWebsocketsExecutor(&config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent: "config-ua",
		},
	})
	executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Label:    "primary",
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"input":[]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: "openai",
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
		},
	}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		auth,
		req,
		opts,
		[]byte(`{"model":"gpt-5-codex","input":[]}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	if got := prepared.wsURL; got != "wss://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("wsURL = %q, want %q", got, "wss://chatgpt.com/backend-api/codex/responses")
	}
	if got := prepared.wsHeaders.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer oauth-token")
	}
	if got := prepared.wsHeaders.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %q, want %q", got, "config-ua")
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %q, want %q", got, "response.create")
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "client_metadata.x-codex-ws-stream-request-start-ms"); got.String() == "" || got.Int() <= 0 {
		t.Fatalf("client_metadata.x-codex-ws-stream-request-start-ms = %q, want positive millisecond timestamp; body=%s", got.String(), prepared.wsReqBody)
	}
	if !bytes.Equal(prepared.wsReqLog.Body, prepared.wsReqBody) {
		t.Fatal("wsReqLog.Body should match wsReqBody")
	}
	if got := prepared.wsReqLog.URL; got != prepared.wsURL {
		t.Fatalf("wsReqLog.URL = %q, want %q", got, prepared.wsURL)
	}
	if got := prepared.authID; got != "auth-1" {
		t.Fatalf("authID = %q, want %q", got, "auth-1")
	}
	if got := prepared.executionSessionID; got != "session-1" {
		t.Fatalf("executionSessionID = %q, want %q", got, "session-1")
	}
	if got := prepared.wsHeaders.Get(codexHeaderSessionID); got != "session-1" {
		t.Fatalf("%s = %q, want %q", codexHeaderSessionID, got, "session-1")
	}
	if got := prepared.wsHeaders.Get(codexHeaderOfficialSessionID); got != "session-1" {
		t.Fatalf("%s = %q, want %q", codexHeaderOfficialSessionID, got, "session-1")
	}
	if got := prepared.wsHeaders.Get(codexHeaderOfficialThreadID); got != "session-1" {
		t.Fatalf("%s = %q, want %q", codexHeaderOfficialThreadID, got, "session-1")
	}
	if got := prepared.wsHeaders.Get(codexHeaderWindowID); got != "session-1:0" {
		t.Fatalf("%s = %q, want %q", codexHeaderWindowID, got, "session-1:0")
	}
	if prepared.sess == nil || prepared.sess.sessionID != "session-1" {
		t.Fatalf("session = %#v, want session-1", prepared.sess)
	}
}

func TestPrepareCodexWebsocketRequestBuildsReusableKeyForPromptCache(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"prompt_cache_key":"cache-1","input":[]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: "openai-response",
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
		},
	}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		auth,
		req,
		opts,
		[]byte(`{"model":"gpt-5-codex","prompt_cache_key":"cache-1","input":[]}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	wantReuseKey := "auth-1|wss://chatgpt.com/backend-api/codex/responses|cache-1|cache-1:0"
	if prepared.reuseKey != wantReuseKey {
		t.Fatalf("reuseKey = %q, want %q", prepared.reuseKey, wantReuseKey)
	}
	if prepared.sess == nil {
		t.Fatal("expected session to be created")
	}
	if prepared.sess.reuseKey != wantReuseKey {
		t.Fatalf("session reuseKey = %q, want %q", prepared.sess.reuseKey, wantReuseKey)
	}
}

func TestCodexWebsocketReusableKeySeparatesWindowGeneration(t *testing.T) {
	bodyA := []byte(`{"prompt_cache_key":"cache-1","client_metadata":{"x-codex-window-id":"cache-1:0"}}`)
	bodyB := []byte(`{"prompt_cache_key":"cache-1","client_metadata":{"x-codex-window-id":"cache-1:1"}}`)

	keyA := codexWebsocketReusableKey("openai-response", "auth-1", "wss://example.test/responses", bodyA)
	keyB := codexWebsocketReusableKey("openai-response", "auth-1", "wss://example.test/responses", bodyB)

	if keyA == "" || keyB == "" {
		t.Fatalf("expected non-empty reuse keys, got %q and %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("different window generations must not share websocket reuse key: %q", keyA)
	}
}

func TestPrepareCodexWebsocketRequestReplaysSessionTurnState(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	sess := executor.getOrCreateSession("session-1", "")
	if sess == nil {
		t.Fatal("expected session to be created")
	}
	sess.setTurnStateScope(`{"turn_id":"turn-1"}`)
	sess.rememberTurnStateHeader(http.Header{codexHeaderTurnState: []string{"turn-state-1"}})

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		&cliproxyauth.Auth{ID: "auth-1", Provider: "codex"},
		cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"input":[]}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			},
		},
		[]byte(`{"model":"gpt-5-codex","input":[],"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-1\"}"}}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	if got := prepared.wsHeaders.Get(codexHeaderTurnState); got != "turn-state-1" {
		t.Fatalf("%s = %q, want turn-state-1", codexHeaderTurnState, got)
	}
}

func TestPrepareCodexWebsocketRequestResetsTurnStateForGeneratedTurnMetadata(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	sess := executor.getOrCreateSession("session-1", "")
	if sess == nil {
		t.Fatal("expected session to be created")
	}
	sess.setTurnStateScope(`{"turn_id":"turn-1"}`)
	sess.rememberTurnStateHeader(http.Header{codexHeaderTurnState: []string{"turn-state-1"}})

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		&cliproxyauth.Auth{ID: "auth-1", Provider: "codex"},
		cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"input":[]}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			},
		},
		[]byte(`{"model":"gpt-5-codex","input":[]}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	if got := prepared.wsHeaders.Get(codexHeaderTurnState); got != "" {
		t.Fatalf("%s = %q, want empty for a newly generated turn", codexHeaderTurnState, got)
	}
	if got := prepared.sess.currentTurnState(); got != "" {
		t.Fatalf("session turn state = %q, want empty", got)
	}
}

func TestPrepareCodexWebsocketRequestResetsTurnStateWhenTurnMetadataChanges(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	sess := executor.getOrCreateSession("session-1", "")
	if sess == nil {
		t.Fatal("expected session to be created")
	}
	sess.setTurnStateScope(`{"turn_id":"turn-1"}`)
	sess.rememberTurnStateHeader(http.Header{codexHeaderTurnState: []string{"turn-state-1"}})

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		&cliproxyauth.Auth{ID: "auth-1", Provider: "codex"},
		cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"input":[]}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
			},
		},
		[]byte(`{"model":"gpt-5-codex","input":[],"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-2\"}"}}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	if got := prepared.wsHeaders.Get(codexHeaderTurnState); got != "" {
		t.Fatalf("%s = %q, want empty after turn metadata changed", codexHeaderTurnState, got)
	}
	if got := prepared.sess.currentTurnState(); got != "" {
		t.Fatalf("session turn state = %q, want empty", got)
	}
}

func TestPrepareCodexWebsocketRequestDerivesPromptCacheForOpenAIChat(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
	}
	payload := []byte(`{"model":"gpt-5-codex","metadata":{"conversation_id":"conv-42"},"messages":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: payload,
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: "openai",
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-1",
		},
	}

	prepared, err := executor.prepareCodexWebsocketRequest(
		ctxWithAPIKey(t, "api-key-1"),
		auth,
		req,
		opts,
		[]byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	promptCacheKey := gjson.GetBytes(prepared.wsReqBody, "prompt_cache_key").String()
	if promptCacheKey == "" {
		t.Fatalf("prompt_cache_key should be derived for OpenAI chat websocket request: %s", string(prepared.wsReqBody))
	}
	if got := prepared.wsHeaders.Get("Session_id"); got != promptCacheKey {
		t.Fatalf("Session_id = %q, want prompt_cache_key %q", got, promptCacheKey)
	}
	if !strings.Contains(prepared.reuseKey, "|"+promptCacheKey+"|") {
		t.Fatalf("reuseKey = %q, want to include prompt_cache_key %q", prepared.reuseKey, promptCacheKey)
	}
}

func TestPrepareCodexWebsocketRequestUsesOfficialThreadHeaderForPromptCache(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
	}
	headers := http.Header{}
	headers.Set(codexHeaderOfficialSessionID, "official-session")
	headers.Set(codexHeaderOfficialThreadID, "official-thread")
	payload := []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: payload,
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: "openai-response",
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "execution-session",
		},
	}

	prepared, err := executor.prepareCodexWebsocketRequest(
		ctxWithAPIKeyAndHeaders(t, "api-key-official-ws", headers),
		auth,
		req,
		opts,
		payload,
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	if got := gjson.GetBytes(prepared.wsReqBody, "prompt_cache_key").String(); got != "official-thread" {
		t.Fatalf("prompt_cache_key = %q, want official-thread; body=%s", got, prepared.wsReqBody)
	}
	if got := prepared.wsHeaders.Get(codexHeaderSessionID); got != "official-session" {
		t.Fatalf("%s = %q, want official-session", codexHeaderSessionID, got)
	}
	if got := prepared.wsHeaders.Get(codexHeaderThreadID); got != "official-thread" {
		t.Fatalf("%s = %q, want official-thread", codexHeaderThreadID, got)
	}
	if !strings.HasSuffix(prepared.reuseKey, "|official-thread|official-thread:0") {
		t.Fatalf("reuseKey = %q, want prompt cache key and window official-thread", prepared.reuseKey)
	}
}

func TestPrepareCodexWebsocketRequestClearsIncrementalStateWhenReuseKeyChanges(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
	}
	const (
		oldCacheKey   = "cache-old"
		newCacheKey   = "cache-new"
		executionID   = "session-1"
		oldUserItem   = `{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]}`
		oldReplyItem  = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old reply"}]}`
		newUserItem   = `{"type":"message","role":"user","content":[{"type":"input_text","text":"new"}]}`
		upstreamHTTP  = "https://chatgpt.com/backend-api/codex/responses"
		upstreamWSURL = "wss://chatgpt.com/backend-api/codex/responses"
	)
	oldReuseKey := "auth-1|" + upstreamWSURL + "|" + oldCacheKey
	sess := executor.getOrCreateSession(executionID, oldReuseKey)
	if sess == nil {
		t.Fatal("expected session")
	}
	sess.rememberLogicalRequest([]byte(fmt.Sprintf(`{"model":"gpt-5-codex","prompt_cache_key":%q,"input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, oldCacheKey, oldUserItem)))
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_old","output":[%s]}}`, oldReplyItem)))
	sess.turnState.Store("old-turn-state")
	sess.turnStateScope.Store(`{"turn_id":"old"}`)

	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(fmt.Sprintf(`{"model":"gpt-5-codex","prompt_cache_key":%q,"input":[%s,%s,%s]}`, newCacheKey, oldUserItem, oldReplyItem, newUserItem)),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: "openai-response",
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: executionID,
		},
	}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		auth,
		req,
		opts,
		[]byte(fmt.Sprintf(`{"model":"gpt-5-codex","prompt_cache_key":%q,"input":[%s,%s,%s]}`, newCacheKey, oldUserItem, oldReplyItem, newUserItem)),
		"oauth-token",
		upstreamHTTP,
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	if got := gjson.GetBytes(prepared.wsReqBody, "previous_response_id"); got.Exists() {
		t.Fatalf("changed reuseKey should not reuse old previous_response_id: %s", prepared.wsReqBody)
	}
	if got := prepared.sess.reuseKey; got != "auth-1|"+upstreamWSURL+"|"+newCacheKey+"|"+newCacheKey+":0" {
		t.Fatalf("session reuseKey = %q, want new cache key", got)
	}
	if got := prepared.sess.lastResponseID; got != "" {
		t.Fatalf("lastResponseID = %q, want cleared", got)
	}
	if got := prepared.sess.currentTurnState(); got != "" {
		t.Fatalf("turn state = %q, want cleared", got)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "input.#").Int(); got != 3 {
		t.Fatalf("full request input length = %d, want 3; body=%s", got, prepared.wsReqBody)
	}
}

func TestPrepareCodexWebsocketRequestNormalizesFinalUpstreamBody(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"input":"hello","store":true}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: "openai-response",
	}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		auth,
		req,
		opts,
		[]byte(`{
			"model":"wrong-model",
			"input":"hello",
			"store":true,
			"stream":false,
			"generate":false,
			"background":true,
			"prompt_cache_retention":"24h",
			"stream_options":{"include_usage":true},
			"temperature":0.2,
			"context_management":{"compaction":"auto"},
			"previous_response_id":"resp_1"
		}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	body := prepared.body
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.4" {
		t.Fatalf("model = %q, want %q", got, "gpt-5.4")
	}
	if got := gjson.GetBytes(body, "store").Bool(); got {
		t.Fatalf("store = true, want false; body=%s", body)
	}
	if got := gjson.GetBytes(body, "stream").Bool(); !got {
		t.Fatalf("stream = false, want true for websocket body: %s", body)
	}
	if got := gjson.GetBytes(body, "generate"); !got.Exists() || got.Bool() {
		t.Fatalf("generate should be preserved as false for websocket prewarm: %s", body)
	}
	if gjson.GetBytes(body, "background").Exists() {
		t.Fatalf("background should be removed from websocket body: %s", body)
	}
	if got := gjson.GetBytes(body, "prompt_cache_retention"); got.Exists() {
		t.Fatalf("prompt_cache_retention should be removed from final websocket body: %s", body)
	}
	if got := gjson.GetBytes(body, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("previous_response_id = %q, want %q; body=%s", got, "resp_1", body)
	}
	for _, field := range []string{"stream_options", "temperature", "context_management"} {
		if gjson.GetBytes(body, field).Exists() {
			t.Fatalf("%s should be removed from final websocket body: %s", field, body)
		}
	}
	if got := gjson.GetBytes(body, "instructions").String(); got != "You are a helpful assistant." {
		t.Fatalf("instructions = %q, want default instructions; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tools").IsArray(); !got {
		t.Fatalf("tools should default to an empty array: %s", body)
	}
	if got := gjson.GetBytes(body, "tools.#").Int(); got != 0 {
		t.Fatalf("tools length = %d, want 0; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want %q; body=%s", got, "auto", body)
	}
	if got := gjson.GetBytes(body, "parallel_tool_calls").Bool(); !got {
		t.Fatalf("parallel_tool_calls = false, want true; body=%s", body)
	}
	if got := gjson.GetBytes(body, "include").IsArray(); !got {
		t.Fatalf("include should default to an empty array: %s", body)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "store").Bool(); got {
		t.Fatalf("websocket request body store = true, want false; wsReqBody=%s", prepared.wsReqBody)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "stream").Bool(); !got {
		t.Fatalf("websocket request body stream = false, want true: %s", prepared.wsReqBody)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "generate"); !got.Exists() || got.Bool() {
		t.Fatalf("websocket request body generate should be preserved as false: %s", prepared.wsReqBody)
	}
	if gjson.GetBytes(prepared.wsReqBody, "background").Exists() {
		t.Fatalf("websocket request body background should be absent: %s", prepared.wsReqBody)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "prompt_cache_retention"); got.Exists() {
		t.Fatalf("websocket request body prompt_cache_retention should be absent: %s", prepared.wsReqBody)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("wsReqBody previous_response_id = %q, want %q", got, "resp_1")
	}
}

func TestPrepareCodexWebsocketRequestDropsServiceTierByDefault(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex"}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		auth,
		req,
		cliproxyexecutor.Options{SourceFormat: "openai-response"},
		[]byte(`{"model":"gpt-5-codex","input":"hello","service_tier":"priority"}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()
	if got := gjson.GetBytes(prepared.body, "service_tier"); got.Exists() {
		t.Fatalf("service_tier should be omitted by default: %s", prepared.body)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "service_tier"); got.Exists() {
		t.Fatalf("websocket request service_tier should be omitted by default: %s", prepared.wsReqBody)
	}
}

func TestPrepareCodexWebsocketRequestMarksGenerateFalseAsPrewarm(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex"}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		auth,
		req,
		cliproxyexecutor.Options{SourceFormat: "openai-response"},
		[]byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"generate":false}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	turnMetadata := prepared.wsHeaders.Get(codexHeaderTurnMetadata)
	assertCodexTurnMetadataString(t, turnMetadata, "request_kind", codexPrewarmRequestKind)
	clientMetadata := gjson.GetBytes(prepared.wsReqBody, "client_metadata."+codexClientMetadataTurnMetadata).String()
	assertCodexTurnMetadataString(t, clientMetadata, "request_kind", codexPrewarmRequestKind)
	if got := gjson.GetBytes(prepared.wsReqBody, "generate"); !got.Exists() || got.Bool() {
		t.Fatalf("generate should remain false on websocket prewarm body: %s", prepared.wsReqBody)
	}
	if got := gjson.Get(turnMetadata, "window_id").String(); got == "" || got != gjson.GetBytes(prepared.wsReqBody, "client_metadata."+codexClientMetadataWindowID).String() {
		t.Fatalf("turn metadata window_id = %q, want client_metadata window id; body=%s", got, prepared.wsReqBody)
	}
}

func TestCodexWebsocketTurnMetadataRequestKindOnlyPrewarmsExplicitGenerateFalse(t *testing.T) {
	if got := codexWebsocketTurnMetadataRequestKind([]byte(`{"generate":false}`)); got != codexPrewarmRequestKind {
		t.Fatalf("generate=false request kind = %q, want %q", got, codexPrewarmRequestKind)
	}
	for _, body := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"generate":true}`),
		[]byte(`{"generate":null}`),
		[]byte(`{"generate":"false"}`),
	} {
		if got := codexWebsocketTurnMetadataRequestKind(body); got != codexTurnRequestKind {
			t.Fatalf("request kind for %s = %q, want %q", body, got, codexTurnRequestKind)
		}
	}
}

func TestPrepareCodexWebsocketRequestPreservesServiceTierWhenAuthOptedIn(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Metadata: map[string]any{cliproxyauth.AuthFileServiceTierPassthroughKey: true},
	}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex"}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		auth,
		req,
		cliproxyexecutor.Options{SourceFormat: "openai-response"},
		[]byte(`{"model":"gpt-5-codex","input":"hello","service_tier":"flex"}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()
	if got := gjson.GetBytes(prepared.body, "service_tier").String(); got != "flex" {
		t.Fatalf("service_tier = %q, want flex; body=%s", got, prepared.body)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "service_tier").String(); got != "flex" {
		t.Fatalf("websocket request service_tier = %q, want flex; body=%s", got, prepared.wsReqBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodyIgnoresPrewarmGenerate(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	prewarmBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[],"generate":false}`, userItem1))
	sess.rememberLogicalRequest(prewarmBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"warm-1","output":[%s]}}`, assistantItem)))

	currentBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1, assistantItem, userItem2))
	wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, currentBody, "")
	if !ok {
		t.Fatalf("expected first real turn to reuse prewarm response")
	}
	if got := gjson.GetBytes(wsBody, "previous_response_id").String(); got != "warm-1" {
		t.Fatalf("previous_response_id = %q, want warm-1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.#").Int(); got != 1 {
		t.Fatalf("delta input length = %d, want 1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.0.content.0.text").String(); got != "next" {
		t.Fatalf("delta text = %q, want next; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "generate"); got.Exists() {
		t.Fatalf("generate should not be added to real incremental request: %s", wsBody)
	}
	if got := gjson.GetBytes(prewarmBody, "generate"); !got.Exists() || got.Bool() {
		t.Fatalf("prewarm body should keep generate=false: %s", prewarmBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodyIgnoresComparableKeyOrder(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{
		"model":"gpt-5.4",
		"input":[%s],
		"tools":[{"name":"lookup","type":"function","strict":false,"parameters":{"properties":{"query":{"type":"string"}},"type":"object"}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"reasoning":null,
		"store":false,
		"stream":true,
		"include":[]
	}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, assistantItem)))

	secondBody := []byte(fmt.Sprintf(`{
		"include":[],
		"stream":true,
		"store":false,
		"reasoning":null,
		"parallel_tool_calls":true,
		"tool_choice":"auto",
		"tools":[{"parameters":{"type":"object","properties":{"query":{"type":"string"}}},"strict":false,"type":"function","name":"lookup"}],
		"input":[%s,%s,%s],
		"model":"gpt-5.4"
	}`, userItem1, assistantItem, userItem2))

	wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, secondBody, "")
	if !ok {
		t.Fatalf("expected semantically identical non-input fields with different key order to reuse previous response")
	}
	if got := gjson.GetBytes(wsBody, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("previous_response_id = %q, want resp_1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.#").Int(); got != 1 {
		t.Fatalf("delta input length = %d, want 1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.0.content.0.text").String(); got != "next" {
		t.Fatalf("delta text = %q, want next; body=%s", got, wsBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodyIgnoresWebsocketTraceMetadata(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[],"client_metadata":{"x-codex-installation-id":"install-1","ws_request_header_traceparent":"00-first","ws_request_header_tracestate":"state-first"}}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, assistantItem)))

	secondBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[],"client_metadata":{"x-codex-installation-id":"install-1","ws_request_header_traceparent":"00-second","ws_request_header_tracestate":"state-second"}}`, userItem1, assistantItem, userItem2))
	wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, secondBody, "")
	if !ok {
		t.Fatalf("expected websocket trace metadata changes to be ignored for incremental reuse")
	}
	if got := gjson.GetBytes(wsBody, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("previous_response_id = %q, want resp_1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.#").Int(); got != 1 {
		t.Fatalf("delta input length = %d, want 1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "client_metadata.ws_request_header_traceparent").String(); got != "00-second" {
		t.Fatalf("traceparent should still be forwarded on websocket payload, got %q; body=%s", got, wsBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodyRejectsServiceTierChangeAfterPrewarm(t *testing.T) {
	const userItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`

	sess := &codexWebsocketSession{}
	prewarmBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[],"service_tier":"flex","generate":false}`, userItem))
	sess.rememberLogicalRequest(prewarmBody)
	sess.rememberCompletedResponse([]byte(`{"response":{"id":"warm-1","output":[]}}`))

	currentBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem))
	if wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, currentBody, ""); ok {
		t.Fatalf("service_tier change should not reuse prewarm response: %s", wsBody)
	}
}

func TestRememberLogicalRequestClearsPreviousResponseForFailedTurn(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, assistantItem)))

	secondBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1, assistantItem, userItem2))
	if _, ok := buildCodexIncrementalWebsocketRequestBody(sess, secondBody, ""); !ok {
		t.Fatal("expected second request to be incremental from first response")
	}

	sess.rememberLogicalRequest(secondBody)
	if wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, secondBody, ""); ok {
		t.Fatalf("request after an unfinished turn should not reuse stale previous_response_id: %s", wsBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodySkipsUnanchoredToolOutput(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		toolOutput    = `{"type":"function_call_output","call_id":"call-missing","output":"ok"}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, assistantItem)))

	currentBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1, assistantItem, toolOutput, userItem2))
	if wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, currentBody, ""); ok {
		t.Fatalf("unanchored function_call_output should use full transcript instead of failing incremental request: %s", wsBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodyAllowsToolOutputAnchoredInPreviousResponse(t *testing.T) {
	const (
		userItem1  = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		toolCall   = `{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}`
		toolOutput = `{"type":"function_call_output","call_id":"call-1","output":"ok"}`
		userItem2  = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, toolCall)))

	currentBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1, toolCall, toolOutput, userItem2))
	wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, currentBody, "")
	if !ok {
		t.Fatal("expected function_call_output anchored to previous response call to remain incremental")
	}
	if got := gjson.GetBytes(wsBody, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("previous_response_id = %q, want resp_1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.#").Int(); got != 2 {
		t.Fatalf("delta input length = %d, want 2; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.0.call_id").String(); got != "call-1" {
		t.Fatalf("delta tool output call_id = %q, want call-1; body=%s", got, wsBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodyAllowsToolCallAndOutputInDelta(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		toolCall      = `{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"{}"}`
		toolOutput    = `{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, assistantItem)))

	currentBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1, assistantItem, toolCall, toolOutput, userItem2))
	wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, currentBody, "")
	if !ok {
		t.Fatal("expected tool call and matching output in delta to remain incremental")
	}
	if got := gjson.GetBytes(wsBody, "input.#").Int(); got != 3 {
		t.Fatalf("delta input length = %d, want 3; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.1.type").String(); got != "custom_tool_call_output" {
		t.Fatalf("delta output type = %q, want custom_tool_call_output; body=%s", got, wsBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodyAllowsCallIDLessToolSearchOutput(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		searchOutput  = `{"type":"tool_search_output","execution":"client","status":"completed","tools":[]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, assistantItem)))

	currentBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1, assistantItem, searchOutput, userItem2))
	wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, currentBody, "")
	if !ok {
		t.Fatal("expected call_id-less tool_search_output to remain incremental")
	}
	if got := gjson.GetBytes(wsBody, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("previous_response_id = %q, want resp_1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.#").Int(); got != 2 {
		t.Fatalf("delta input length = %d, want 2; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.0.type").String(); got != "tool_search_output" {
		t.Fatalf("delta output type = %q, want tool_search_output; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.0.call_id"); got.Exists() {
		t.Fatalf("call_id-less tool_search_output gained call_id: %s", wsBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodyAllowsServerToolSearchOutputWithCallID(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		searchOutput  = `{"type":"tool_search_output","call_id":"server-search","execution":"server","status":"completed","tools":[]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, assistantItem)))

	currentBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1, assistantItem, searchOutput, userItem2))
	wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, currentBody, "")
	if !ok {
		t.Fatal("expected server tool_search_output to remain incremental without a matching call")
	}
	if got := gjson.GetBytes(wsBody, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("previous_response_id = %q, want resp_1; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.#").Int(); got != 2 {
		t.Fatalf("delta input length = %d, want 2; body=%s", got, wsBody)
	}
	if got := gjson.GetBytes(wsBody, "input.0.call_id").String(); got != "server-search" {
		t.Fatalf("server tool search call_id = %q, want server-search; body=%s", got, wsBody)
	}
}

func TestBuildCodexIncrementalWebsocketRequestBodySkipsMismatchedToolOutputType(t *testing.T) {
	const (
		userItem1  = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		toolCall   = `{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"{}"}`
		toolOutput = `{"type":"function_call_output","call_id":"call-1","output":"wrong-type"}`
		userItem2  = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	sess := &codexWebsocketSession{}
	firstBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1))
	sess.rememberLogicalRequest(firstBody)
	sess.rememberCompletedResponse([]byte(fmt.Sprintf(`{"response":{"id":"resp_1","output":[%s]}}`, toolCall)))

	currentBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s,%s],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":null,"store":false,"stream":true,"include":[]}`, userItem1, toolCall, toolOutput, userItem2))
	if wsBody, ok := buildCodexIncrementalWebsocketRequestBody(sess, currentBody, ""); ok {
		t.Fatalf("mismatched tool output type should use full transcript instead of failing incremental request: %s", wsBody)
	}
}

func TestPrepareCodexWebsocketRequestIncludesEncryptedReasoningContentWhenReasoningRequested(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"input":"hello","reasoning":{"effort":"high"}}`),
	}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		auth,
		req,
		cliproxyexecutor.Options{SourceFormat: "openai-response"},
		[]byte(`{"model":"gpt-5.4","input":"hello","reasoning":{"effort":"high"}}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	for _, payload := range [][]byte{prepared.body, prepared.wsReqBody} {
		if got := gjson.GetBytes(payload, `include.#(=="reasoning.encrypted_content")`).String(); got != "reasoning.encrypted_content" {
			t.Fatalf("include missing reasoning.encrypted_content; body=%s", payload)
		}
	}
}

func TestCodexWebsocketSessionShouldRotateBeforeConnectionLimit(t *testing.T) {
	sess := &codexWebsocketSession{}
	now := time.Now()
	sess.markOpened(now.Add(-codexResponsesWebsocketMaxLifetime + time.Second))
	if sess.shouldRotate(now) {
		t.Fatal("session rotated before max lifetime")
	}
	sess.markOpened(now.Add(-codexResponsesWebsocketMaxLifetime - time.Second))
	if !sess.shouldRotate(now) {
		t.Fatal("session should rotate after max lifetime")
	}
}

func TestPrepareCodexWebsocketRequestStripsUnsupportedFinalUpstreamFields(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","messages":[{"role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: "openai",
	}

	prepared, err := executor.prepareCodexWebsocketRequest(
		contextWithGinHeaders(map[string]string{
			"Traceparent": "00-test",
			"Tracestate":  "state-test",
		}),
		auth,
		req,
		opts,
		[]byte(`{
			"model":"gpt-5-codex",
			"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
			"messages":[{"role":"user","content":"hello"}],
			"metadata":{"conversation_id":"conv-1"},
			"response_format":{"type":"json_schema"},
			"functions":[{"name":"legacy_func"}],
			"trace":{"traceparent":"00-test"}
		}`),
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	for _, payload := range [][]byte{prepared.body, prepared.wsReqBody} {
		for _, field := range []string{"messages", "metadata", "response_format", "functions", "trace"} {
			if gjson.GetBytes(payload, field).Exists() {
				t.Fatalf("%s should not reach websocket Codex upstream payload: %s", field, payload)
			}
		}
		if got := gjson.GetBytes(payload, "input.0.content.0.text").String(); got != "hello" {
			t.Fatalf("input.0.content.0.text = %q, want %q; payload=%s", got, "hello", payload)
		}
	}
	if got := prepared.wsHeaders.Get("Traceparent"); got != "" {
		t.Fatalf("Traceparent handshake header = %q, want empty", got)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "client_metadata.ws_request_header_traceparent").String(); got != "00-test" {
		t.Fatalf("client_metadata.ws_request_header_traceparent = %q, want 00-test; body=%s", got, prepared.wsReqBody)
	}
	if got := gjson.GetBytes(prepared.wsReqBody, "client_metadata.ws_request_header_tracestate").String(); got != "state-test" {
		t.Fatalf("client_metadata.ws_request_header_tracestate = %q, want state-test; body=%s", got, prepared.wsReqBody)
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanIgnoresRecoverableInvalidate(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "sess-1"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	if disconnectCh == nil {
		t.Fatal("expected disconnect channel")
	}

	sess := exec.getOrCreateSession(sessionID, "")
	if sess == nil {
		t.Fatal("expected session")
	}
	sess.connMu.Lock()
	sess.conn = conn
	sess.authID = "auth-1"
	sess.wsURL = "ws://example.test/responses"
	sess.readerConn = conn
	sess.connMu.Unlock()

	upstreamErr := errors.New("upstream gone")
	exec.invalidateUpstreamConn(sess, conn, "test_invalidate", upstreamErr)

	select {
	case errRead, ok := <-disconnectCh:
		t.Fatalf("recoverable invalidate should not signal disconnect, got ok=%v err=%v", ok, errRead)
	default:
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanSignalsAndResets(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "sess-1"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	if disconnectCh == nil {
		t.Fatal("expected disconnect channel")
	}

	sess := exec.getOrCreateSession(sessionID, "")
	if sess == nil {
		t.Fatal("expected session")
	}

	upstreamErr := errors.New("upstream gone")
	sess.notifyUpstreamDisconnect(upstreamErr)

	select {
	case errRead, ok := <-disconnectCh:
		if !ok {
			t.Fatal("expected disconnect channel to deliver error before closing")
		}
		if errRead == nil || errRead.Error() != upstreamErr.Error() {
			t.Fatalf("disconnect error = %v, want %v", errRead, upstreamErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disconnect signal")
	}

	resetCh := exec.UpstreamDisconnectChan(sessionID)
	if resetCh == nil {
		t.Fatal("expected reset disconnect channel")
	}
	if resetCh == disconnectCh {
		t.Fatal("expected a fresh disconnect channel after signal")
	}
	select {
	case errRead, ok := <-resetCh:
		t.Fatalf("fresh disconnect channel should not replay stale signal, got ok=%v err=%v", ok, errRead)
	default:
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanIfExistsDoesNotCreateSession(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{})

	if ch := exec.UpstreamDisconnectChanIfExists("missing-session"); ch != nil {
		t.Fatalf("missing session channel = %v, want nil", ch)
	}
	if sess := exec.existingSession("missing-session"); sess != nil {
		t.Fatalf("missing session was created: %#v", sess)
	}

	if sess := exec.getOrCreateSession("existing-session", ""); sess == nil {
		t.Fatal("expected session to be created")
	}
	if ch := exec.UpstreamDisconnectChanIfExists("existing-session"); ch == nil {
		t.Fatal("expected channel for existing session")
	}
}

func TestApplyCodexWebsocketHeadersDefaultsToCurrentResponsesBeta(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("User-Agent"); got != misc.CodexCLIUserAgent {
		t.Fatalf("User-Agent = %s, want %s", got, misc.CodexCLIUserAgent)
	}
	if got := headers.Get("Version"); got != misc.CodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, misc.CodexCLIVersion)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
	assertGeneratedCodexTurnMetadata(t, headers.Get("X-Codex-Turn-Metadata"))
	assertCodexTurnMetadataString(t, headers.Get("X-Codex-Turn-Metadata"), "window_id", headers.Get(codexHeaderWindowID))
	if got := headers.Get("Session_id"); got == "" {
		t.Fatal("Session_id should be generated by default")
	}
	if got := headers.Get(codexHeaderOfficialSessionID); got != headers.Get("Session_id") {
		t.Fatalf("%s = %q, want Session_id %q", codexHeaderOfficialSessionID, got, headers.Get("Session_id"))
	}
	if got := headers.Get(codexHeaderOfficialThreadID); got != headers.Get(codexHeaderThreadID) {
		t.Fatalf("%s = %q, want %s %q", codexHeaderOfficialThreadID, got, codexHeaderThreadID, headers.Get(codexHeaderThreadID))
	}
	if got := headers.Get("X-Client-Request-Id"); got != headers.Get("Session_id") {
		t.Fatalf("X-Client-Request-Id = %q, want Session_id %q", got, headers.Get("Session_id"))
	}
}

func TestCodexWebsocketResponseProcessedFeatureMatching(t *testing.T) {
	headers := http.Header{}
	headers.Add("X-Codex-Beta-Features", " other-feature , RESPONSES_WEBSOCKET_RESPONSE_PROCESSED ")

	if !codexWebsocketResponseProcessedEnabled(headers) {
		t.Fatal("expected response.processed feature to match case-insensitively")
	}
}

func TestCodexWebsocketShouldSendResponseProcessedSkipsGenerateFalse(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Beta-Features", codexBetaFeatureResponseProcessed)

	if codexWebsocketShouldSendResponseProcessed(headers, []byte(`{"type":"response.create","generate":false}`)) {
		t.Fatal("response.processed should not be sent for generate=false prewarm requests")
	}
	if !codexWebsocketShouldSendResponseProcessed(headers, []byte(`{"type":"response.create"}`)) {
		t.Fatal("response.processed should be sent when feature is enabled and generate is absent")
	}
	if !codexWebsocketShouldSendResponseProcessed(headers, []byte(`{"type":"response.create","generate":null}`)) {
		t.Fatal("response.processed should be sent unless generate is explicitly false")
	}
}

func TestCodexEnsureVersionHeaderNormalizesMinimum(t *testing.T) {
	tests := []struct {
		name          string
		sourceVersion string
		targetVersion string
		want          string
	}{
		{
			name: "missing version uses default",
			want: misc.CodexCLIVersion,
		},
		{
			name:          "local dev sentinel is upgraded",
			sourceVersion: "0.0.0",
			want:          misc.CodexCLIVersion,
		},
		{
			name:          "older official client is upgraded",
			sourceVersion: "0.115.0-alpha.27",
			want:          misc.CodexCLIVersion,
		},
		{
			name:          "older prerelease is upgraded",
			sourceVersion: "0.134.0-alpha.1",
			want:          misc.CodexCLIVersion,
		},
		{
			name:          "newer prerelease is preserved",
			sourceVersion: "0.134.0-alpha.4",
			want:          "0.134.0-alpha.4",
		},
		{
			name:          "newer stable client is preserved",
			sourceVersion: "0.135.0",
			want:          "0.135.0",
		},
		{
			name:          "invalid client version is upgraded",
			sourceVersion: "dev-build",
			want:          misc.CodexCLIVersion,
		},
		{
			name:          "target version is used when source is empty",
			targetVersion: "0.135.0",
			want:          "0.135.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := http.Header{}
			source := http.Header{}
			if tt.targetVersion != "" {
				target.Set("Version", tt.targetVersion)
			}
			if tt.sourceVersion != "" {
				source.Set("Version", tt.sourceVersion)
			}

			codexEnsureVersionHeader(target, source)

			if got := target.Get("Version"); got != tt.want {
				t.Fatalf("Version = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyCodexWebsocketHeadersOverridesClientResponsesBeta(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"OpenAI-Beta": "responses_websockets=2025-01-01",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, nil, "", nil)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
}

func TestApplyCodexWebsocketHeadersPreservesOfficialSessionHeaders(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderOfficialSessionID: "official-session",
		codexHeaderOfficialThreadID:  "official-thread",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, nil, "", nil)

	if got := headers.Get(codexHeaderSessionID); got != "official-session" {
		t.Fatalf("%s = %q, want official-session", codexHeaderSessionID, got)
	}
	if got := headers.Get(codexHeaderThreadID); got != "official-thread" {
		t.Fatalf("%s = %q, want official-thread", codexHeaderThreadID, got)
	}
	if got := headers.Get(codexHeaderOfficialSessionID); got != "official-session" {
		t.Fatalf("%s = %q, want official-session", codexHeaderOfficialSessionID, got)
	}
	if got := headers.Get(codexHeaderOfficialThreadID); got != "official-thread" {
		t.Fatalf("%s = %q, want official-thread", codexHeaderOfficialThreadID, got)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "official-thread" {
		t.Fatalf("X-Client-Request-Id = %q, want official-thread", got)
	}
}

func TestApplyCodexWebsocketHeadersNormalizesVersionAndPassesThroughClientIdentityHeaders(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"Originator":              "Codex Desktop",
		"Version":                 "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata":   `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":     "019d2233-e240-7162-992d-38df0a2a0e0d",
		codexHeaderOAIAttestation: "v1.attestation",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := headers.Get("Version"); got != misc.CodexCLIVersion {
		t.Fatalf("Version = %s, want %s", got, misc.CodexCLIVersion)
	}
	assertCodexTurnMetadataString(t, headers.Get("X-Codex-Turn-Metadata"), "turn_id", "turn-1")
	assertCodexTurnMetadataString(t, headers.Get("X-Codex-Turn-Metadata"), "request_kind", codexTurnRequestKind)
	assertCodexTurnMetadataString(t, headers.Get("X-Codex-Turn-Metadata"), "window_id", headers.Get(codexHeaderWindowID))
	if got := headers.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
	if got := headers.Get(codexHeaderOAIAttestation); got != "v1.attestation" {
		t.Fatalf("%s = %s, want v1.attestation", codexHeaderOAIAttestation, got)
	}
}

func TestApplyCodexWebsocketHeadersSetsFedrampForOAuthAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "oauth-token",
			"account_id":   "account-1",
			"fedramp":      true,
		},
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "oauth-token", nil)

	if got := headers.Get(codexHeaderOpenAIFedramp); got != "true" {
		t.Fatalf("%s = %q, want true", codexHeaderOpenAIFedramp, got)
	}
}

func TestApplyCodexWebsocketHeadersUsesDerivedSessionHeadersWithoutForwardingConversationID(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"header:Originator": "codex_vscode",
		},
	}
	headers := http.Header{
		"Conversation_id": []string{"conv-1"},
	}

	got := applyCodexWebsocketHeaders(context.Background(), headers, auth, "", nil)

	if gotConversation := got.Get("Conversation_id"); gotConversation != "" {
		t.Fatalf("Conversation_id = %q, want empty", gotConversation)
	}
	if gotSession := got.Get("Session_id"); gotSession != "conv-1" {
		t.Fatalf("Session_id = %q, want %q", gotSession, "conv-1")
	}
	if gotRequestID := got.Get("X-Client-Request-Id"); gotRequestID != "conv-1" {
		t.Fatalf("X-Client-Request-Id = %q, want %q", gotRequestID, "conv-1")
	}
	if gotOriginator := got.Get("Originator"); gotOriginator != "codex_vscode" {
		t.Fatalf("Originator = %q, want %q", gotOriginator, "codex_vscode")
	}
	if gotUA := got.Get("User-Agent"); !strings.HasPrefix(gotUA, "codex_vscode/") {
		t.Fatalf("User-Agent = %q, want codex_vscode/ prefix", gotUA)
	}
}

func TestApplyCodexWebsocketHeadersUsesConfigDefaultsForOAuth(t *testing.T) {
	includeTimingMetrics := true
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:            "my-codex-client/1.0",
			BetaFeatures:         "feature-a,feature-b",
			IncludeTimingMetrics: &includeTimingMetrics,
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "my-codex-client/1.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "my-codex-client/1.0")
	}
	if got := headers.Get("x-codex-beta-features"); got != "feature-a,feature-b" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "feature-a,feature-b")
	}
	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("x-responsesapi-include-timing-metrics"); got != "true" {
		t.Fatalf("x-responsesapi-include-timing-metrics = %q, want true", got)
	}
}

func TestApplyCodexWebsocketHeadersConfigUserAgentOverridesExistingHeader(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})
	headers := http.Header{}
	headers.Set("User-Agent", "existing-ua")
	headers.Set("X-Codex-Beta-Features", "existing-beta")

	got := applyCodexWebsocketHeaders(ctx, headers, auth, "", cfg)

	if gotVal := got.Get("User-Agent"); gotVal != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", gotVal, "config-ua")
	}
	if gotVal := got.Get("x-codex-beta-features"); gotVal != "existing-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", gotVal, "existing-beta")
	}
}

func TestApplyCodexWebsocketHeadersConfigUserAgentOverridesClientHeader(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := headers.Get("x-codex-beta-features"); got != "client-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "client-beta")
	}
}

func TestApplyCodexWebsocketHeadersConfigUserAgentOverridesAuthFileAndClient(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":      "user@example.com",
			"user_agent": "auth-file-ua",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	})
	headers := http.Header{}
	headers.Set("User-Agent", "existing-ua")

	got := applyCodexWebsocketHeaders(ctx, headers, auth, "", cfg)

	if gotVal := got.Get("User-Agent"); gotVal != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", gotVal, "config-ua")
	}
}

func TestApplyCodexWebsocketHeadersUsesConfigUserAgentForAPIKeyAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "sk-test", cfg)

	if got := headers.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := headers.Get("x-codex-beta-features"); got != "config-beta" {
		t.Fatalf("x-codex-beta-features = %q, want config-beta", got)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
}

func TestApplyCodexHeadersUsesConfigUserAgentForOAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := req.Header.Get("x-codex-beta-features"); got != "config-beta" {
		t.Fatalf("x-codex-beta-features = %q, want config-beta", got)
	}
}

func TestApplyCodexHeadersAddsAccountIDForMirroredOAuthAccessToken(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "access-token",
		},
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "acct_123",
		},
	}

	applyCodexHeaders(req, auth, "access-token", true, nil)

	if got := req.Header[codexHeaderChatGPTAccountID]; len(got) != 1 || got[0] != "acct_123" {
		t.Fatalf("%s values = %#v, want [acct_123]", codexHeaderChatGPTAccountID, got)
	}
	if _, ok := req.Header[codexHeaderChatGPTAccountID]; !ok {
		t.Fatalf("expected exact %s header key, got %#v", codexHeaderChatGPTAccountID, req.Header)
	}
}

func TestApplyCodexWebsocketHeadersAddsAccountIDForMirroredOAuthAccessToken(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "access-token",
		},
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "acct_123",
		},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "access-token", nil)

	if got := headers[codexHeaderChatGPTAccountID]; len(got) != 1 || got[0] != "acct_123" {
		t.Fatalf("%s values = %#v, want [acct_123]", codexHeaderChatGPTAccountID, got)
	}
	if _, ok := headers[codexHeaderChatGPTAccountID]; !ok {
		t.Fatalf("expected exact %s header key, got %#v", codexHeaderChatGPTAccountID, headers)
	}
}

func TestApplyCodexHeadersNormalizesVersionAndPassesThroughClientIdentityHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"Originator":              "Codex Desktop",
		"Version":                 "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata":   `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":     "019d2233-e240-7162-992d-38df0a2a0e0d",
		codexHeaderOAIAttestation: "v1.attestation",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := req.Header.Get("Version"); got != misc.CodexCLIVersion {
		t.Fatalf("Version = %s, want %s", got, misc.CodexCLIVersion)
	}
	assertCodexTurnMetadataString(t, req.Header.Get("X-Codex-Turn-Metadata"), "turn_id", "turn-1")
	assertCodexTurnMetadataString(t, req.Header.Get("X-Codex-Turn-Metadata"), "request_kind", codexTurnRequestKind)
	assertCodexTurnMetadataString(t, req.Header.Get("X-Codex-Turn-Metadata"), "window_id", req.Header.Get(codexHeaderWindowID))
	if got := req.Header.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
	if got := req.Header.Get(codexHeaderOAIAttestation); got != "v1.attestation" {
		t.Fatalf("%s = %s, want v1.attestation", codexHeaderOAIAttestation, got)
	}
}

func TestApplyCodexHeadersUsesMetadataOriginatorFallback(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"originator": "codex_vscode",
		},
	}

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("Originator"); got != "codex_vscode" {
		t.Fatalf("Originator = %q, want %q", got, "codex_vscode")
	}
	if gotUA := req.Header.Get("User-Agent"); !strings.HasPrefix(gotUA, "codex_vscode/") {
		t.Fatalf("User-Agent = %q, want codex_vscode/ prefix", gotUA)
	}
}

func TestApplyCodexHeadersUsesDerivedSessionHeadersWithoutForwardingConversationID(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"header:Originator": "codex_vscode",
		},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"Conversation_id": "conv-1",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if gotConversation := req.Header.Get("Conversation_id"); gotConversation != "" {
		t.Fatalf("Conversation_id = %q, want empty", gotConversation)
	}
	if gotSession := req.Header.Get("Session_id"); gotSession != "conv-1" {
		t.Fatalf("Session_id = %q, want %q", gotSession, "conv-1")
	}
	if gotRequestID := req.Header.Get("X-Client-Request-Id"); gotRequestID != "conv-1" {
		t.Fatalf("X-Client-Request-Id = %q, want conv-1", gotRequestID)
	}
	if gotOriginator := req.Header.Get("Originator"); gotOriginator != "codex_vscode" {
		t.Fatalf("Originator = %q, want %q", gotOriginator, "codex_vscode")
	}
	if gotUA := req.Header.Get("User-Agent"); !strings.HasPrefix(gotUA, "codex_vscode/") {
		t.Fatalf("User-Agent = %q, want codex_vscode/ prefix", gotUA)
	}
}

func TestApplyCodexHeadersConfigUserAgentOverridesAuthFileAndClient(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("User-Agent", "existing-ua")

	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
		Attributes: map[string]string{
			"header:User-Agent": "auth-file-ua",
		},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := req.Header.Get("x-codex-beta-features"); got != "config-beta" {
		t.Fatalf("x-codex-beta-features = %q, want config-beta", got)
	}
}

func TestApplyCodexHeadersUsesConfigUserAgentForAPIKeyAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "sk-test",
		},
	}

	applyCodexHeaders(req, auth, "sk-test", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := req.Header.Get("x-codex-beta-features"); got != "config-beta" {
		t.Fatalf("x-codex-beta-features = %q, want config-beta", got)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
	if got := req.Header.Get("Version"); got != misc.CodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, misc.CodexCLIVersion)
	}
	if got := req.Header.Get("Connection"); got != "" {
		t.Fatalf("Connection = %q, want empty", got)
	}
}

func TestApplyCodexHeadersDoesNotInjectClientOnlyHeadersByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	applyCodexHeaders(req, nil, "oauth-token", true, nil)

	if got := req.Header.Get("User-Agent"); got != misc.CodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, misc.CodexCLIUserAgent)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
	if got := req.Header.Get("Version"); got != misc.CodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, misc.CodexCLIVersion)
	}
	assertGeneratedCodexTurnMetadata(t, req.Header.Get("X-Codex-Turn-Metadata"))
	assertCodexTurnMetadataString(t, req.Header.Get("X-Codex-Turn-Metadata"), "window_id", req.Header.Get(codexHeaderWindowID))
	if got := req.Header.Get("Session_id"); got == "" {
		t.Fatal("Session_id should be generated by default")
	}
	if got := req.Header.Get(codexHeaderOfficialSessionID); got != req.Header.Get("Session_id") {
		t.Fatalf("%s = %q, want Session_id %q", codexHeaderOfficialSessionID, got, req.Header.Get("Session_id"))
	}
	if got := req.Header.Get(codexHeaderOfficialThreadID); got != req.Header.Get(codexHeaderThreadID) {
		t.Fatalf("%s = %q, want %s %q", codexHeaderOfficialThreadID, got, codexHeaderThreadID, req.Header.Get(codexHeaderThreadID))
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != req.Header.Get(codexHeaderThreadID) {
		t.Fatalf("X-Client-Request-Id = %q, want %s %q", got, codexHeaderThreadID, req.Header.Get(codexHeaderThreadID))
	}
}

func TestApplyCodexHeadersPreservesOfficialSessionHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		codexHeaderOfficialSessionID: "official-session",
		codexHeaderOfficialThreadID:  "official-thread",
	}))

	applyCodexHeaders(req, nil, "oauth-token", true, nil)

	if got := req.Header.Get(codexHeaderSessionID); got != "official-session" {
		t.Fatalf("%s = %q, want official-session", codexHeaderSessionID, got)
	}
	if got := req.Header.Get(codexHeaderThreadID); got != "official-thread" {
		t.Fatalf("%s = %q, want official-thread", codexHeaderThreadID, got)
	}
	if got := req.Header.Get(codexHeaderOfficialSessionID); got != "official-session" {
		t.Fatalf("%s = %q, want official-session", codexHeaderOfficialSessionID, got)
	}
	if got := req.Header.Get(codexHeaderOfficialThreadID); got != "official-thread" {
		t.Fatalf("%s = %q, want official-thread", codexHeaderOfficialThreadID, got)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "official-thread" {
		t.Fatalf("X-Client-Request-Id = %q, want official-thread", got)
	}
}

func TestApplyCodexHeadersCompactKeepsHeadersLeanByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses/compact", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	applyCodexHeaders(req, nil, "oauth-token", false, nil)

	if got := req.Header.Get("User-Agent"); got != misc.CodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, misc.CodexCLIUserAgent)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
	if got := req.Header.Get("Version"); got != misc.CodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, misc.CodexCLIVersion)
	}
	if got := req.Header.Get(codexHeaderSessionID); got == "" {
		t.Fatal("Session_id should be generated for compact requests")
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "request_kind", codexCompactionRequestKind)
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "window_id", req.Header.Get(codexHeaderWindowID))
	if got := req.Header.Get(codexHeaderTurnState); got != "" {
		t.Fatalf("%s = %q, want empty", codexHeaderTurnState, got)
	}
}

func TestApplyCodexHeadersCompactPreservesExplicitTurnHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses/compact", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Codex-Turn-State":    "turn-state-1",
		"X-Client-Request-Id":   "request-1",
	}))

	applyCodexHeaders(req, nil, "oauth-token", false, nil)

	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "turn_id", "turn-1")
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "request_kind", codexCompactionRequestKind)
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "window_id", req.Header.Get(codexHeaderWindowID))
	if got := req.Header.Get(codexHeaderTurnState); got != "turn-state-1" {
		t.Fatalf("%s = %q, want explicit value", codexHeaderTurnState, got)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "request-1" {
		t.Fatalf("X-Client-Request-Id = %q, want explicit value", got)
	}
}

func TestApplyCodexHeadersUsesTurnMetadataSessionIDWhenMissing(t *testing.T) {
	resetCodexWindowStateStore()

	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		codexHeaderTurnMetadata: `{"session_id":"turn-session-1","turn_id":"turn-1","sandbox":"none"}`,
	}))

	applyCodexHeaders(req, nil, "oauth-token", true, nil)

	if got := req.Header.Get(codexHeaderSessionID); got != "turn-session-1" {
		t.Fatalf("%s = %q, want %q", codexHeaderSessionID, got, "turn-session-1")
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "turn-session-1" {
		t.Fatalf("X-Client-Request-Id = %q, want turn-session-1", got)
	}
	if got := req.Header.Get(codexHeaderWindowID); got != "turn-session-1:0" {
		t.Fatalf("%s = %q, want %q", codexHeaderWindowID, got, "turn-session-1:0")
	}
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "session_id", "turn-session-1")
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "turn_id", "turn-1")
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "sandbox", "none")
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "request_kind", codexTurnRequestKind)
	assertCodexTurnMetadataString(t, req.Header.Get(codexHeaderTurnMetadata), "window_id", req.Header.Get(codexHeaderWindowID))
}

func TestEnsureUpstreamConnRedialsRecentlyActiveBrokenConnection(t *testing.T) {
	var (
		upgrader    = websocket.Upgrader{}
		accepted    atomic.Int32
		serverMu    sync.Mutex
		serverConns []*websocket.Conn
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		accepted.Add(1)
		serverMu.Lock()
		serverConns = append(serverConns, conn)
		serverMu.Unlock()
	}))
	defer server.Close()
	defer func() {
		serverMu.Lock()
		defer serverMu.Unlock()
		for _, conn := range serverConns {
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	staleConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	if err := staleConn.Close(); err != nil {
		t.Fatalf("Close() stale conn error = %v", err)
	}

	executor := NewCodexWebsocketsExecutor(nil)
	sess := &codexWebsocketSession{sessionID: "session-redial"}
	sess.conn = staleConn
	sess.readerConn = staleConn
	sess.lastActivityUnixNano.Store(time.Now().UnixNano())
	defer closeCodexWebsocketSession(sess, "test_cleanup")

	conn, _, err := executor.ensureUpstreamConn(context.Background(), nil, sess, "auth-1", wsURL, http.Header{})
	if err != nil {
		t.Fatalf("ensureUpstreamConn() error = %v", err)
	}
	if conn == nil {
		t.Fatal("ensureUpstreamConn() returned nil conn")
	}
	if conn == staleConn {
		t.Fatal("ensureUpstreamConn() should redial instead of reusing a broken recent conn")
	}
	if got := accepted.Load(); got != 2 {
		t.Fatalf("accepted connections = %d, want 2", got)
	}
	if sess.conn != conn {
		t.Fatal("session should store the redialed conn")
	}
}

func TestCloseExecutionSessionParksReusableSessionAndReattaches(t *testing.T) {
	oldTTL := codexResponsesWebsocketParkTTL
	codexResponsesWebsocketParkTTL = 5 * time.Second
	defer func() {
		codexResponsesWebsocketParkTTL = oldTTL
	}()

	var (
		upgrader    = websocket.Upgrader{}
		accepted    atomic.Int32
		serverMu    sync.Mutex
		serverConns []*websocket.Conn
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		accepted.Add(1)
		serverMu.Lock()
		serverConns = append(serverConns, conn)
		serverMu.Unlock()
	}))
	defer server.Close()
	defer func() {
		serverMu.Lock()
		defer serverMu.Unlock()
		for _, conn := range serverConns {
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}

	store := &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = store

	reuseKey := "auth-1|" + wsURL + "|cache-1"
	sess1 := executor.getOrCreateSession("exec-1", reuseKey)
	if sess1 == nil {
		t.Fatal("expected session to be created")
	}
	sess1.conn = conn
	sess1.readerConn = conn
	sess1.wsURL = wsURL
	sess1.authID = "auth-1"
	sess1.touchActivity()

	executor.CloseExecutionSession("exec-1")

	store.parkedMu.Lock()
	parked := store.parked[reuseKey]
	store.parkedMu.Unlock()
	if parked != sess1 {
		t.Fatal("expected session to be parked for reuse")
	}

	sess2 := executor.getOrCreateSession("exec-2", reuseKey)
	if sess2 != sess1 {
		t.Fatal("expected parked session to be reattached")
	}
	if sess2.sessionID != "exec-2" {
		t.Fatalf("sessionID = %q, want exec-2", sess2.sessionID)
	}

	reusedConn, _, err := executor.ensureUpstreamConn(context.Background(), nil, sess2, "auth-1", wsURL, http.Header{})
	if err != nil {
		t.Fatalf("ensureUpstreamConn() error = %v", err)
	}
	if reusedConn != conn {
		t.Fatal("expected parked session to reuse original upstream conn")
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("accepted connections = %d, want 1", got)
	}

	executor.closeAllExecutionSessions("test_cleanup")
}

func TestResetExecutionSessionClosesReusableSessionWithoutParking(t *testing.T) {
	oldTTL := codexResponsesWebsocketParkTTL
	codexResponsesWebsocketParkTTL = 5 * time.Second
	defer func() {
		codexResponsesWebsocketParkTTL = oldTTL
	}()

	var (
		upgrader    = websocket.Upgrader{}
		accepted    atomic.Int32
		serverMu    sync.Mutex
		serverConns []*websocket.Conn
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		accepted.Add(1)
		serverMu.Lock()
		serverConns = append(serverConns, conn)
		serverMu.Unlock()
	}))
	defer server.Close()
	defer func() {
		serverMu.Lock()
		defer serverMu.Unlock()
		for _, conn := range serverConns {
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}

	store := &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = store

	reuseKey := "auth-1|" + wsURL + "|cache-1"
	sess1 := executor.getOrCreateSession("exec-1", reuseKey)
	if sess1 == nil {
		t.Fatal("expected session to be created")
	}
	sess1.conn = conn
	sess1.readerConn = conn
	sess1.wsURL = wsURL
	sess1.authID = "auth-1"
	sess1.touchActivity()

	executor.ResetExecutionSession("exec-1")

	store.sessionsMu.Lock()
	_, active := store.sessions["exec-1"]
	store.sessionsMu.Unlock()
	store.parkedMu.Lock()
	parked := store.parked[reuseKey]
	store.parkedMu.Unlock()
	if active {
		t.Fatal("expected active session to be removed after reset")
	}
	if parked != nil {
		t.Fatal("expected reset session not to be parked")
	}

	sess2 := executor.getOrCreateSession("exec-2", reuseKey)
	if sess2 == sess1 {
		t.Fatal("expected reset to force a fresh session")
	}

	reconnected, _, err := executor.ensureUpstreamConn(context.Background(), nil, sess2, "auth-1", wsURL, http.Header{})
	if err != nil {
		t.Fatalf("ensureUpstreamConn() error = %v", err)
	}
	if reconnected == conn {
		t.Fatal("expected reset session to dial a fresh upstream conn")
	}
	if got := accepted.Load(); got != 2 {
		t.Fatalf("accepted connections = %d, want 2", got)
	}

	executor.closeAllExecutionSessions("test_cleanup")
}

func TestCodexWebsocketsExecuteStreamTranslatesAndNormalizesOpenAIResponsesRequest(t *testing.T) {
	var (
		upgrader = websocket.Upgrader{}
		received = make(chan []byte, 1)
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}
		received <- append([]byte(nil), payload...)

		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_1",
				"object": "response",
				"status": "completed",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":  1,
					"output_tokens": 0,
					"total_tokens":  1,
				},
			},
		}); err != nil {
			t.Errorf("WriteJSON() error = %v", err)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","store":true,"stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: req.Payload,
	}

	result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	requestBody := <-received
	if got := gjson.GetBytes(requestBody, "type").String(); got != "response.create" {
		t.Fatalf("websocket type = %q, want response.create; body=%s", got, requestBody)
	}
	if got := gjson.GetBytes(requestBody, "store").Bool(); got {
		t.Fatalf("websocket store = true, want false; body=%s", requestBody)
	}
	if got := gjson.GetBytes(requestBody, "stream").Bool(); !got {
		t.Fatalf("websocket stream = false, want true; body=%s", requestBody)
	}
	if got := gjson.GetBytes(requestBody, "input").IsArray(); !got {
		t.Fatalf("input should be translated to an array; body=%s", requestBody)
	}
	if got := gjson.GetBytes(requestBody, "input.0.type").String(); got != "message" {
		t.Fatalf("input.0.type = %q, want %q; body=%s", got, "message", requestBody)
	}
	if got := gjson.GetBytes(requestBody, "input.0.content.0.text").String(); got != "hello" {
		t.Fatalf("input.0.content.0.text = %q, want %q; body=%s", got, "hello", requestBody)
	}
	if gjson.GetBytes(requestBody, "messages").Exists() {
		t.Fatalf("messages should not be forwarded to websocket upstream: %s", requestBody)
	}

	for range result.Chunks {
	}
}

func TestCodexWebsocketsExecuteStreamRetriesConnectionLimitError(t *testing.T) {
	var (
		upgrader = websocket.Upgrader{}
		attempts atomic.Int32
		received = make(chan []byte, 2)
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}
		received <- append([]byte(nil), payload...)

		if attempts.Add(1) == 1 {
			if errWrite := conn.WriteJSON(map[string]any{
				"type":   "error",
				"status": http.StatusBadRequest,
				"error": map[string]any{
					"type":    "invalid_request_error",
					"code":    codexWebsocketConnectionLimitReachedCode,
					"message": "Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue.",
				},
			}); errWrite != nil {
				t.Errorf("WriteJSON(connection_limit) error = %v", errWrite)
			}
			return
		}

		if errWrite := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_1",
				"object": "response",
				"status": "completed",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":  1,
					"output_tokens": 0,
					"total_tokens":  1,
				},
			},
		}); errWrite != nil {
			t.Errorf("WriteJSON(response.completed) error = %v", errWrite)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	t.Cleanup(func() { executor.closeAllExecutionSessions("test_cleanup") })
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: req.Payload,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-connection-limit-retry",
		},
	}

	result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
		}
	}

	if got := attempts.Load(); got != 2 {
		t.Fatalf("websocket attempts = %d, want 2", got)
	}
	firstRequest := <-received
	secondRequest := <-received
	if got := gjson.GetBytes(firstRequest, "type").String(); got != "response.create" {
		t.Fatalf("first request type = %q, want response.create; body=%s", got, firstRequest)
	}
	if got := gjson.GetBytes(secondRequest, "type").String(); got != "response.create" {
		t.Fatalf("second request type = %q, want response.create; body=%s", got, secondRequest)
	}
}

func TestCodexWebsocketsExecuteStreamInjectsImageGenerationForOAuth(t *testing.T) {
	var (
		upgrader = websocket.Upgrader{}
		received = make(chan []byte, 1)
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}
		received <- append([]byte(nil), payload...)

		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_1",
				"object": "response",
				"status": "completed",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":  1,
					"output_tokens": 0,
					"total_tokens":  1,
				},
			},
		}); err != nil {
			t.Errorf("WriteJSON() error = %v", err)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "oauth-token",
			"account_id":   "acct_123",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","tools":[]}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: req.Payload,
	}

	result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	requestBody := <-received
	if got := gjson.GetBytes(requestBody, "tools.#").Int(); got != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", got, requestBody)
	}
	if got := gjson.GetBytes(requestBody, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation; body=%s", got, requestBody)
	}

	for range result.Chunks {
	}
}

func TestCodexWebsocketsExecuteStreamTranslatesAndNormalizesOpenAIChatRequest(t *testing.T) {
	var (
		upgrader = websocket.Upgrader{}
		received = make(chan []byte, 1)
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}
		received <- append([]byte(nil), payload...)

		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_1",
				"object": "response",
				"status": "completed",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":  1,
					"output_tokens": 0,
					"total_tokens":  1,
				},
			},
		}); err != nil {
			t.Errorf("WriteJSON() error = %v", err)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"messages":[{"role":"user","content":"hello"}],
			"metadata":{"conversation_id":"conv-1"},
			"store":true
		}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: req.Payload,
	}

	result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	requestBody := <-received
	if got := gjson.GetBytes(requestBody, "type").String(); got != "response.create" {
		t.Fatalf("websocket type = %q, want response.create; body=%s", got, requestBody)
	}
	if got := gjson.GetBytes(requestBody, "store").Bool(); got {
		t.Fatalf("websocket store = true, want false; body=%s", requestBody)
	}
	if got := gjson.GetBytes(requestBody, "stream").Bool(); !got {
		t.Fatalf("websocket stream = false, want true; body=%s", requestBody)
	}
	if gjson.GetBytes(requestBody, "messages").Exists() {
		t.Fatalf("messages should not be forwarded to websocket upstream: %s", requestBody)
	}
	if gjson.GetBytes(requestBody, "metadata").Exists() {
		t.Fatalf("metadata should not be forwarded to websocket upstream: %s", requestBody)
	}
	if got := gjson.GetBytes(requestBody, "input").IsArray(); !got {
		t.Fatalf("input should be translated to an array; body=%s", requestBody)
	}
	if got := gjson.GetBytes(requestBody, "input.0.type").String(); got != "message" {
		t.Fatalf("input.0.type = %q, want %q; body=%s", got, "message", requestBody)
	}
	if got := gjson.GetBytes(requestBody, "input.0.content.0.text").String(); got != "hello" {
		t.Fatalf("input.0.content.0.text = %q, want %q; body=%s", got, "hello", requestBody)
	}

	for range result.Chunks {
	}
}

func TestCodexWebsocketsExecuteStreamSendsIncrementalFollowUp(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	var (
		upgrader  = websocket.Upgrader{}
		received  = make(chan []byte, 2)
		serverErr = make(chan error, 2)
		upgrades  atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		upgrades.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		for i := 0; i < 2; i++ {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				serverErr <- fmt.Errorf("ReadMessage(%d) error: %w", i+1, errRead)
				return
			}
			received <- append([]byte(nil), payload...)

			if i == 0 {
				if errWrite := conn.WriteJSON(map[string]any{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item": map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{map[string]any{
							"type": "output_text",
							"text": "hello",
						}},
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(output_item.done) error: %w", errWrite)
					return
				}
			}

			responseID := fmt.Sprintf("resp_%d", i+1)
			if errWrite := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":     responseID,
					"object": "response",
					"status": "completed",
					"output": []any{},
					"usage": map[string]any{
						"input_tokens":  1,
						"output_tokens": 0,
						"total_tokens":  1,
					},
				},
			}); errWrite != nil {
				serverErr <- fmt.Errorf("WriteJSON(response.completed) error: %w", errWrite)
				return
			}
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-incremental",
		},
	}

	drain := func(result *cliproxyexecutor.StreamResult) {
		t.Helper()
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
			}
		}
	}
	waitPayload := func() []byte {
		t.Helper()
		select {
		case payload := <-received:
			return payload
		case err := <-serverErr:
			t.Fatalf("websocket server error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for websocket request")
		}
		return nil
	}

	firstPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s]}`, userItem1))
	firstResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: firstPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = firstPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() first error = %v", err)
	}
	firstRequest := waitPayload()
	drain(firstResult)

	if got := gjson.GetBytes(firstRequest, "previous_response_id"); got.Exists() {
		t.Fatalf("first request should not include previous_response_id: %s", firstRequest)
	}
	if got := gjson.GetBytes(firstRequest, "input.#").Int(); got != 1 {
		t.Fatalf("first request input length = %d, want 1; body=%s", got, firstRequest)
	}

	secondPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s]}`, userItem1, assistantItem, userItem2))
	secondResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: secondPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = secondPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() second error = %v", err)
	}
	secondRequest := waitPayload()
	drain(secondResult)

	if got := upgrades.Load(); got != 1 {
		t.Fatalf("websocket upgrades = %d, want 1", got)
	}
	if got := gjson.GetBytes(secondRequest, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("second previous_response_id = %q, want resp_1; body=%s", got, secondRequest)
	}
	if got := gjson.GetBytes(secondRequest, "input.#").Int(); got != 1 {
		t.Fatalf("second request input length = %d, want only delta item; body=%s", got, secondRequest)
	}
	if got := gjson.GetBytes(secondRequest, "input.0.content.0.text").String(); got != "next" {
		t.Fatalf("second delta text = %q, want next; body=%s", got, secondRequest)
	}
}

func TestCodexWebsocketsExecuteStreamReusesPreviousResponseAfterReconnect(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	var (
		upgrader  = websocket.Upgrader{}
		received  = make(chan []byte, 2)
		serverErr = make(chan error, 2)
		upgrades  atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		connIndex := int(upgrades.Add(1))
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			serverErr <- fmt.Errorf("ReadMessage(%d) error: %w", connIndex, errRead)
			return
		}
		received <- append([]byte(nil), payload...)

		output := []any{}
		if connIndex == 1 {
			output = []any{map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": "hello",
				}},
			}}
		}
		if errWrite := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     fmt.Sprintf("resp_%d", connIndex),
				"object": "response",
				"status": "completed",
				"output": output,
			},
		}); errWrite != nil {
			serverErr <- fmt.Errorf("WriteJSON(response.completed %d) error: %w", connIndex, errWrite)
			return
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	t.Cleanup(func() { executor.closeAllExecutionSessions("test_cleanup") })

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-reconnect-reset",
		},
	}

	drain := func(result *cliproxyexecutor.StreamResult) {
		t.Helper()
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
			}
		}
	}
	waitPayload := func() []byte {
		t.Helper()
		select {
		case payload := <-received:
			return payload
		case err := <-serverErr:
			t.Fatalf("websocket server error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for websocket request")
		}
		return nil
	}

	firstPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s]}`, userItem1))
	firstResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: firstPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = firstPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() first error = %v", err)
	}
	_ = waitPayload()
	drain(firstResult)

	secondPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s]}`, userItem1, assistantItem, userItem2))
	secondResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: secondPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = secondPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() second error = %v", err)
	}
	secondRequest := waitPayload()
	drain(secondResult)

	if got := upgrades.Load(); got != 2 {
		t.Fatalf("websocket upgrades = %d, want 2", got)
	}
	if got := gjson.GetBytes(secondRequest, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("second request after reconnect previous_response_id = %q, want resp_1; body=%s", got, secondRequest)
	}
	if got := gjson.GetBytes(secondRequest, "input.#").Int(); got != 1 {
		t.Fatalf("second request after reconnect should stay incremental, input length = %d; body=%s", got, secondRequest)
	}
}

func TestCodexWebsocketsExecuteStreamRetriesReadDisconnectBeforePayload(t *testing.T) {
	const userItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`

	var (
		upgrader  = websocket.Upgrader{}
		received  = make(chan []byte, 2)
		serverErr = make(chan error, 2)
		upgrades  atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		connIndex := int(upgrades.Add(1))
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			serverErr <- fmt.Errorf("ReadMessage(%d) error: %w", connIndex, errRead)
			return
		}
		received <- append([]byte(nil), payload...)

		if connIndex == 1 {
			return
		}
		if errWrite := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_retry",
				"object": "response",
				"status": "completed",
				"output": []any{map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "output_text",
						"text": "ok",
					}},
				}},
			},
		}); errWrite != nil {
			serverErr <- fmt.Errorf("WriteJSON(response.completed retry) error: %w", errWrite)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	payload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s]}`, userItem))
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-read-disconnect-stream",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	waitPayload := func() []byte {
		t.Helper()
		select {
		case payload := <-received:
			return payload
		case err := <-serverErr:
			t.Fatalf("websocket server error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for websocket request")
		}
		return nil
	}
	firstRequest := waitPayload()
	retryRequest := waitPayload()

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
		}
	}
	if got := upgrades.Load(); got != 2 {
		t.Fatalf("websocket upgrades = %d, want 2", got)
	}
	if !bytes.Equal(bytes.TrimSpace(firstRequest), bytes.TrimSpace(retryRequest)) {
		t.Fatalf("retry request should replay the same body\nfirst=%s\nretry=%s", firstRequest, retryRequest)
	}
}

func TestCodexWebsocketsExecuteRetriesReadDisconnectBeforeCompletion(t *testing.T) {
	const userItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`

	var (
		upgrader  = websocket.Upgrader{}
		received  = make(chan []byte, 2)
		serverErr = make(chan error, 2)
		upgrades  atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		connIndex := int(upgrades.Add(1))
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			serverErr <- fmt.Errorf("ReadMessage(%d) error: %w", connIndex, errRead)
			return
		}
		received <- append([]byte(nil), payload...)

		if connIndex == 1 {
			return
		}
		if errWrite := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_retry",
				"object": "response",
				"status": "completed",
				"output": []any{},
			},
		}); errWrite != nil {
			serverErr <- fmt.Errorf("WriteJSON(response.completed retry) error: %w", errWrite)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	payload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s]}`, userItem))
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-read-disconnect-nonstream",
		},
	}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	waitPayload := func() []byte {
		t.Helper()
		select {
		case payload := <-received:
			return payload
		case err := <-serverErr:
			t.Fatalf("websocket server error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for websocket request")
		}
		return nil
	}
	firstRequest := waitPayload()
	retryRequest := waitPayload()

	if got := upgrades.Load(); got != 2 {
		t.Fatalf("websocket upgrades = %d, want 2", got)
	}
	if !bytes.Equal(bytes.TrimSpace(firstRequest), bytes.TrimSpace(retryRequest)) {
		t.Fatalf("retry request should replay the same body\nfirst=%s\nretry=%s", firstRequest, retryRequest)
	}
}

func TestCodexWebsocketsExecuteStreamRetriesFullRequestWhenPreviousResponseMissing(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	var (
		upgrader  = websocket.Upgrader{}
		received  = make(chan []byte, 3)
		serverErr = make(chan error, 3)
		upgrades  atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		upgrades.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		for i := 0; i < 3; {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				serverErr <- fmt.Errorf("ReadMessage(%d) error: %w", i+1, errRead)
				return
			}
			if gjson.GetBytes(payload, "type").String() == "response.processed" {
				continue
			}
			received <- append([]byte(nil), payload...)

			switch i {
			case 0:
				if errWrite := conn.WriteJSON(map[string]any{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item": map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{map[string]any{
							"type": "output_text",
							"text": "hello",
						}},
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(output_item.done) error: %w", errWrite)
					return
				}
				if errWrite := conn.WriteJSON(map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id":     "resp_1",
						"object": "response",
						"status": "completed",
						"output": []any{map[string]any{
							"type": "message",
							"role": "assistant",
							"content": []any{map[string]any{
								"type": "output_text",
								"text": "hello",
							}},
						}},
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(response.completed first) error: %w", errWrite)
					return
				}
			case 1:
				if errWrite := conn.WriteJSON(map[string]any{
					"type":   "error",
					"status": http.StatusBadRequest,
					"error": map[string]any{
						"code":    codexPreviousResponseNotFoundCode,
						"message": "Previous response with id 'resp_1' not found.",
						"param":   "previous_response_id",
						"type":    "invalid_request_error",
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(previous_response_not_found) error: %w", errWrite)
					return
				}
			case 2:
				if errWrite := conn.WriteJSON(map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id":     "resp_2",
						"object": "response",
						"status": "completed",
						"output": []any{},
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(response.completed retry) error: %w", errWrite)
					return
				}
			}
			i++
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-previous-missing",
		},
	}

	drain := func(result *cliproxyexecutor.StreamResult) {
		t.Helper()
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
			}
		}
	}
	waitPayload := func() []byte {
		t.Helper()
		select {
		case payload := <-received:
			return payload
		case err := <-serverErr:
			t.Fatalf("websocket server error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for websocket request")
		}
		return nil
	}

	firstPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s]}`, userItem1))
	firstResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: firstPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = firstPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() first error = %v", err)
	}
	_ = waitPayload()
	drain(firstResult)

	secondPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s]}`, userItem1, assistantItem, userItem2))
	secondResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: secondPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = secondPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() second error = %v", err)
	}
	incrementalRequest := waitPayload()
	retryRequest := waitPayload()
	drain(secondResult)

	if got := upgrades.Load(); got != 1 {
		t.Fatalf("websocket upgrades = %d, want 1", got)
	}
	if got := gjson.GetBytes(incrementalRequest, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("incremental previous_response_id = %q, want resp_1; body=%s", got, incrementalRequest)
	}
	if got := gjson.GetBytes(retryRequest, "previous_response_id"); got.Exists() {
		t.Fatalf("retry request should omit stale previous_response_id: %s", retryRequest)
	}
	if got := gjson.GetBytes(retryRequest, "input.#").Int(); got != 3 {
		t.Fatalf("retry request input length = %d, want full input; body=%s", got, retryRequest)
	}
	if got := gjson.GetBytes(retryRequest, "input.2.content.0.text").String(); got != "next" {
		t.Fatalf("retry final input text = %q, want next; body=%s", got, retryRequest)
	}
}

func TestCodexWebsocketsExecuteStreamDoesNotDropExplicitPreviousResponseOnMissingError(t *testing.T) {
	var (
		upgrader  = websocket.Upgrader{}
		received  = make(chan []byte, 2)
		serverErr = make(chan error, 2)
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			serverErr <- fmt.Errorf("ReadMessage() error: %w", errRead)
			return
		}
		received <- append([]byte(nil), payload...)
		if errWrite := conn.WriteJSON(map[string]any{
			"type":   "error",
			"status": http.StatusBadRequest,
			"error": map[string]any{
				"code":    codexPreviousResponseNotFoundCode,
				"message": "Previous response with id 'resp-explicit' not found.",
				"param":   "previous_response_id",
				"type":    "invalid_request_error",
			},
		}); errWrite != nil {
			serverErr <- fmt.Errorf("WriteJSON(previous_response_not_found) error: %w", errWrite)
			return
		}

		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, payload, errRead = conn.ReadMessage(); errRead == nil {
			received <- append([]byte(nil), payload...)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-explicit","input":[{"type":"function_call_output","call_id":"call_Rx1FW4RrRF9C1SyH2xxBVtEn","output":"ok"}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-explicit-previous-missing",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	firstRequest := func() []byte {
		t.Helper()
		select {
		case payload := <-received:
			return payload
		case err := <-serverErr:
			t.Fatalf("websocket server error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for websocket request")
		}
		return nil
	}()
	if got := gjson.GetBytes(firstRequest, "previous_response_id").String(); got != "resp-explicit" {
		t.Fatalf("first previous_response_id = %q, want resp-explicit; body=%s", got, firstRequest)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected upstream previous_response_not_found error")
	}
	if !strings.Contains(streamErr.Error(), codexPreviousResponseNotFoundCode) &&
		!strings.Contains(streamErr.Error(), "Previous response with id") {
		t.Fatalf("stream error = %v, want previous_response_not_found", streamErr)
	}

	select {
	case retryRequest := <-received:
		t.Fatalf("explicit previous_response_id must not be retried without context; retry=%s", retryRequest)
	default:
	}
}

func TestCodexWebsocketsExecuteStreamClearsIncrementalStateAfterUpstreamError(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	var (
		upgrader  = websocket.Upgrader{}
		received  = make(chan []byte, 3)
		serverErr = make(chan error, 3)
		requests  atomic.Int32
		upgrades  atomic.Int32
	)

	writeCompleted := func(conn *websocket.Conn, responseID string, output []any) {
		t.Helper()
		if errWrite := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     responseID,
				"object": "response",
				"status": "completed",
				"output": output,
			},
		}); errWrite != nil {
			serverErr <- fmt.Errorf("WriteJSON(response.completed %s) error: %w", responseID, errWrite)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		upgrades.Add(1)
		defer func() { _ = conn.Close() }()

		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			if gjson.GetBytes(payload, "type").String() == "response.processed" {
				continue
			}
			received <- append([]byte(nil), payload...)
			switch requests.Add(1) {
			case 1:
				writeCompleted(conn, "resp_1", []any{map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "output_text",
						"text": "hello",
					}},
				}})
			case 2:
				if errWrite := conn.WriteJSON(map[string]any{
					"type": "response.failed",
					"response": map[string]any{
						"error": map[string]any{
							"code":    "invalid_prompt",
							"message": "synthetic websocket failure",
						},
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(response.failed) error: %w", errWrite)
				}
			case 3:
				writeCompleted(conn, "resp_2", []any{})
				return
			}
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-error-keeps-incremental-state",
		},
	}

	waitPayload := func() []byte {
		t.Helper()
		select {
		case payload := <-received:
			return payload
		case err := <-serverErr:
			t.Fatalf("websocket server error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for websocket request")
		}
		return nil
	}
	drainOK := func(result *cliproxyexecutor.StreamResult) {
		t.Helper()
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("unexpected stream error = %v", chunk.Err)
			}
		}
	}
	drainErr := func(result *cliproxyexecutor.StreamResult) {
		t.Helper()
		var streamErr error
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				streamErr = chunk.Err
			}
		}
		if streamErr == nil {
			t.Fatal("expected stream error")
		}
	}

	firstPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s]}`, userItem1))
	firstResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: firstPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = firstPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() first error = %v", err)
	}
	_ = waitPayload()
	drainOK(firstResult)

	secondPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s]}`, userItem1, assistantItem, userItem2))
	secondResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: secondPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = secondPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() second error = %v", err)
	}
	secondRequest := waitPayload()
	drainErr(secondResult)

	thirdResult, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: secondPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = secondPayload
		return turnOpts
	}())
	if err != nil {
		t.Fatalf("ExecuteStream() third error = %v", err)
	}
	thirdRequest := waitPayload()
	drainOK(thirdResult)

	if got := gjson.GetBytes(secondRequest, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("second request previous_response_id = %q, want resp_1; body=%s", got, secondRequest)
	}
	if got := upgrades.Load(); got != 2 {
		t.Fatalf("websocket upgrades = %d, want 2 after response.failed invalidates connection", got)
	}
	if gjson.GetBytes(thirdRequest, "previous_response_id").Exists() {
		t.Fatalf("failed turn must clear previous_response_id; body=%s", thirdRequest)
	}
	if got := gjson.GetBytes(thirdRequest, "input.#").Int(); got != 3 {
		t.Fatalf("failed turn must resend full input, got %d body=%s", got, thirdRequest)
	}
}

func TestCodexWebsocketsExecuteRetriesFullRequestWhenPreviousResponseMissing(t *testing.T) {
	const (
		userItem1     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
		assistantItem = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
		userItem2     = `{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`
	)

	var (
		upgrader  = websocket.Upgrader{}
		received  = make(chan []byte, 3)
		serverErr = make(chan error, 3)
		upgrades  atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		upgrades.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		for i := 0; i < 3; i++ {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				serverErr <- fmt.Errorf("ReadMessage(%d) error: %w", i+1, errRead)
				return
			}
			received <- append([]byte(nil), payload...)

			switch i {
			case 0:
				if errWrite := conn.WriteJSON(map[string]any{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item": map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{map[string]any{
							"type": "output_text",
							"text": "hello",
						}},
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(output_item.done) error: %w", errWrite)
					return
				}
				if errWrite := conn.WriteJSON(map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id":     "resp_1",
						"object": "response",
						"status": "completed",
						"output": []any{map[string]any{
							"type": "message",
							"role": "assistant",
							"content": []any{map[string]any{
								"type": "output_text",
								"text": "hello",
							}},
						}},
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(response.completed first) error: %w", errWrite)
					return
				}
			case 1:
				if errWrite := conn.WriteJSON(map[string]any{
					"type":   "error",
					"status": http.StatusBadRequest,
					"error": map[string]any{
						"code":    codexPreviousResponseNotFoundCode,
						"message": "Previous response with id 'resp_1' not found.",
						"param":   "previous_response_id",
						"type":    "invalid_request_error",
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(previous_response_not_found) error: %w", errWrite)
					return
				}
			case 2:
				if errWrite := conn.WriteJSON(map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id":     "resp_2",
						"object": "response",
						"status": "completed",
						"output": []any{},
					},
				}); errWrite != nil {
					serverErr <- fmt.Errorf("WriteJSON(response.completed retry) error: %w", errWrite)
					return
				}
			}
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-nonstream-previous-missing",
		},
	}

	waitPayload := func() []byte {
		t.Helper()
		select {
		case payload := <-received:
			return payload
		case err := <-serverErr:
			t.Fatalf("websocket server error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for websocket request")
		}
		return nil
	}

	firstPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s]}`, userItem1))
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: firstPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = firstPayload
		return turnOpts
	}()); err != nil {
		t.Fatalf("Execute() first error = %v", err)
	}
	_ = waitPayload()

	secondPayload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[%s,%s,%s]}`, userItem1, assistantItem, userItem2))
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: secondPayload}, func() cliproxyexecutor.Options {
		turnOpts := opts
		turnOpts.OriginalRequest = secondPayload
		return turnOpts
	}()); err != nil {
		t.Fatalf("Execute() second error = %v", err)
	}
	incrementalRequest := waitPayload()
	retryRequest := waitPayload()
	if got := upgrades.Load(); got != 1 {
		t.Fatalf("websocket upgrades = %d, want 1", got)
	}
	if got := gjson.GetBytes(incrementalRequest, "previous_response_id").String(); got != "resp_1" {
		t.Fatalf("incremental previous_response_id = %q, want resp_1; body=%s", got, incrementalRequest)
	}
	if got := gjson.GetBytes(retryRequest, "previous_response_id"); got.Exists() {
		t.Fatalf("retry request should omit stale previous_response_id: %s", retryRequest)
	}
	if got := gjson.GetBytes(retryRequest, "input.#").Int(); got != 3 {
		t.Fatalf("retry request input length = %d, want full input; body=%s", got, retryRequest)
	}
}

func TestCodexWebsocketsRetryRebindsActiveReadChannel(t *testing.T) {
	var (
		upgrader    = websocket.Upgrader{}
		accepted    = make(chan *websocket.Conn, 2)
		serverErr   = make(chan error, 4)
		serverMu    sync.Mutex
		serverConns []*websocket.Conn
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("Upgrade() error: %w", err)
			return
		}
		serverMu.Lock()
		serverConns = append(serverConns, conn)
		serverMu.Unlock()
		accepted <- conn
	}))
	defer server.Close()
	defer func() {
		serverMu.Lock()
		defer serverMu.Unlock()
		for _, conn := range serverConns {
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()

	waitConn := func(label string) *websocket.Conn {
		t.Helper()
		select {
		case conn := <-accepted:
			return conn
		case err := <-serverErr:
			t.Fatalf("websocket server %s error: %v", label, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for %s websocket connection", label)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	t.Cleanup(func() { executor.closeAllExecutionSessions("test_cleanup") })

	sess := executor.getOrCreateSession("exec-retry-readch", "")
	conn, _, err := executor.ensureUpstreamConn(ctx, nil, sess, "auth-1", wsURL, http.Header{})
	if err != nil {
		t.Fatalf("ensureUpstreamConn() error = %v", err)
	}
	firstServerConn := waitConn("initial")

	oldReadCh := make(chan codexWebsocketRead, codexResponsesWebsocketReadBuffer)
	readCh := oldReadCh
	sess.setActive(readCh, conn)

	requestBody := []byte(`{"type":"response.create","input":[]}`)
	connRetry, wsReqBodyRetry, err := executor.retrySessionWebsocketRequest(
		ctx,
		nil,
		sess,
		conn,
		&readCh,
		"auth-1",
		wsURL,
		http.Header{},
		helps.UpstreamRequestLog{URL: wsURL, Method: "WEBSOCKET"},
		requestBody,
		errors.New("forced send error"),
	)
	if err != nil {
		t.Fatalf("retrySessionWebsocketRequest() error = %v", err)
	}
	if connRetry == nil {
		t.Fatal("retrySessionWebsocketRequest() returned nil conn")
	}
	if len(wsReqBodyRetry) == 0 || !bytes.Equal(wsReqBodyRetry, requestBody) {
		t.Fatalf("retry request body = %s, want %s", wsReqBodyRetry, requestBody)
	}
	if readCh == nil {
		t.Fatal("retrySessionWebsocketRequest() left readCh nil")
	}
	if readCh == oldReadCh {
		t.Fatal("retrySessionWebsocketRequest() must bind a fresh read channel after reconnect")
	}

	secondServerConn := waitConn("retry")
	_ = firstServerConn.Close()

	_ = secondServerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, err := secondServerConn.ReadMessage()
	if err != nil {
		t.Fatalf("retry server ReadMessage() error = %v", err)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
		t.Fatalf("retry payload type = %q, want response.create; payload=%s", got, payload)
	}

	if err := secondServerConn.WriteJSON(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp_retry",
			"object": "response",
			"status": "completed",
			"output": []any{},
		},
	}); err != nil {
		t.Fatalf("retry server WriteJSON() error = %v", err)
	}

	msgType, responsePayload, err := readCodexWebsocketMessage(ctx, sess, connRetry, readCh)
	if err != nil {
		t.Fatalf("readCodexWebsocketMessage() after retry error = %v", err)
	}
	if msgType != websocket.TextMessage {
		t.Fatalf("message type = %d, want text", msgType)
	}
	if got := gjson.GetBytes(responsePayload, "type").String(); got != "response.completed" {
		t.Fatalf("response type = %q, want response.completed; payload=%s", got, responsePayload)
	}
}

func TestCodexWebsocketsExecuteStreamKeepsSessionOnHTTPFallbackAfterUpgradeRequired(t *testing.T) {
	var wsAttempts atomic.Int32
	var httpAttempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			wsAttempts.Add(1)
			http.Error(w, "websockets disabled", http.StatusUpgradeRequired)
			return
		}

		httpAttempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}` + "\n\n"))
	}))
	defer server.Close()

	store := &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = store
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-426",
		},
	}

	for i := 0; i < 2; i++ {
		result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatalf("ExecuteStream() attempt %d error = %v", i+1, err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("ExecuteStream() attempt %d chunk error = %v", i+1, chunk.Err)
			}
		}
	}

	if got := wsAttempts.Load(); got != 1 {
		t.Fatalf("websocket upgrade attempts = %d, want 1", got)
	}
	if got := httpAttempts.Load(); got != 2 {
		t.Fatalf("HTTP fallback attempts = %d, want 2", got)
	}

	store.sessionsMu.RLock()
	sess := store.sessions["exec-426"]
	store.sessionsMu.RUnlock()
	if sess == nil || !sess.httpFallbackActive() {
		t.Fatal("expected execution session to stay on HTTP fallback")
	}
}

func TestCodexWebsocketsExecuteStreamRefreshesAfterUnauthorizedHandshake(t *testing.T) {
	var (
		upgrader = websocket.Upgrader{}
		mu       sync.Mutex
		headers  []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Errorf("unexpected non-websocket request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}

		mu.Lock()
		headers = append(headers, r.Header.Get("Authorization"))
		attempt := len(headers)
		mu.Unlock()

		if attempt == 1 {
			http.Error(w, "expired token", http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("ReadMessage() error = %v", errRead)
			return
		}
		if errWrite := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_1",
				"object": "response",
				"status": "completed",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":  1,
					"output_tokens": 0,
					"total_tokens":  1,
				},
			},
		}); errWrite != nil {
			t.Errorf("WriteJSON() error = %v", errWrite)
		}
	}))
	defer server.Close()

	ctx := cliproxyauth.WithRefreshCoordinator(context.Background(), func(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
		refreshed := auth.Clone()
		if refreshed.Metadata == nil {
			refreshed.Metadata = map[string]any{}
		}
		refreshed.Metadata["access_token"] = "new-access-token"
		refreshed.Metadata["refresh_token"] = "new-refresh-token"
		return refreshed, nil
	})

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "refresh-token",
		},
	}

	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer old-access-token", "Bearer new-access-token"}
	if len(headers) != len(want) {
		t.Fatalf("Authorization headers = %#v, want %#v", headers, want)
	}
	for i := range want {
		if headers[i] != want[i] {
			t.Fatalf("Authorization headers = %#v, want %#v", headers, want)
		}
	}
}

func TestCodexWebsocketsExecuteStreamAllowsNilAuthForEarlyCompactReject(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)

	_, err := executor.ExecuteStream(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{
		Alt:          "responses/compact",
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err == nil {
		t.Fatal("ExecuteStream() error = nil, want compact streaming rejection")
	}
	if !strings.Contains(err.Error(), "streaming not supported") {
		t.Fatalf("ExecuteStream() error = %v, want streaming not supported", err)
	}
}

func TestCodexWebsocketsExecuteRefreshesAfterUnauthorizedHandshake(t *testing.T) {
	var (
		upgrader = websocket.Upgrader{}
		mu       sync.Mutex
		headers  []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Errorf("unexpected non-websocket request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}

		mu.Lock()
		headers = append(headers, r.Header.Get("Authorization"))
		attempt := len(headers)
		mu.Unlock()

		if attempt == 1 {
			http.Error(w, "expired token", http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("ReadMessage() error = %v", errRead)
			return
		}
		if errWrite := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_1",
				"object": "response",
				"status": "completed",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":  1,
					"output_tokens": 0,
					"total_tokens":  1,
				},
			},
		}); errWrite != nil {
			t.Errorf("WriteJSON() error = %v", errWrite)
		}
	}))
	defer server.Close()

	ctx := cliproxyauth.WithRefreshCoordinator(context.Background(), func(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
		refreshed := auth.Clone()
		if refreshed.Metadata == nil {
			refreshed.Metadata = map[string]any{}
		}
		refreshed.Metadata["access_token"] = "new-access-token"
		refreshed.Metadata["refresh_token"] = "new-refresh-token"
		return refreshed, nil
	})

	executor := NewCodexWebsocketsExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "refresh-token",
		},
	}

	if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer old-access-token", "Bearer new-access-token"}
	if len(headers) != len(want) {
		t.Fatalf("Authorization headers = %#v, want %#v", headers, want)
	}
	for i := range want {
		if headers[i] != want[i] {
			t.Fatalf("Authorization headers = %#v, want %#v", headers, want)
		}
	}
}

func TestCodexWebsocketsExecuteStreamSendsResponseProcessedWhenFeatureEnabled(t *testing.T) {
	var (
		upgrader  = websocket.Upgrader{}
		processed = make(chan []byte, 1)
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("ReadMessage() request error = %v", errRead)
			return
		}
		if errWrite := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_1",
				"object": "response",
				"status": "completed",
				"output": []any{},
			},
		}); errWrite != nil {
			t.Errorf("WriteJSON() error = %v", errWrite)
			return
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("ReadMessage() response.processed error = %v", errRead)
			return
		}
		processed <- append([]byte(nil), payload...)
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(&config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			BetaFeatures: codexBetaFeatureResponseProcessed,
		},
	})
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	}

	result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
		}
	}

	select {
	case payload := <-processed:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.processed" {
			t.Fatalf("processed type = %q, want response.processed; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "response_id").String(); got != "resp_1" {
			t.Fatalf("response_id = %q, want resp_1; payload=%s", got, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response.processed")
	}
}

func TestCodexWebsocketParkExecutionSessionCapsParkedSessions(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	t.Cleanup(func() {
		executor.closeAllExecutionSessions("test_cleanup")
	})

	for i := 0; i < codexResponsesWebsocketMaxParked+2; i++ {
		sess := &codexWebsocketSession{
			sessionID: fmt.Sprintf("session-%d", i),
			reuseKey:  fmt.Sprintf("reuse-%d", i),
		}
		sess.lastActivityUnixNano.Store(int64(i + 1))
		if !executor.parkExecutionSession(sess) {
			t.Fatalf("parkExecutionSession(%d) = false, want true", i)
		}
	}

	executor.store.parkedMu.Lock()
	defer executor.store.parkedMu.Unlock()
	if got := len(executor.store.parked); got != codexResponsesWebsocketMaxParked {
		t.Fatalf("parked sessions = %d, want %d", got, codexResponsesWebsocketMaxParked)
	}
	if _, exists := executor.store.parked["reuse-0"]; exists {
		t.Fatal("oldest parked session should be evicted")
	}
	if _, exists := executor.store.parked[fmt.Sprintf("reuse-%d", codexResponsesWebsocketMaxParked+1)]; !exists {
		t.Fatal("newest parked session should remain")
	}
}

func TestCodexWebsocketParkExecutionSessionSkipsExpiringSession(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	t.Cleanup(func() {
		executor.closeAllExecutionSessions("test_cleanup")
	})

	sess := &codexWebsocketSession{
		sessionID: "session-expiring",
		reuseKey:  "reuse-expiring",
	}
	sess.markOpened(time.Now().Add(-codexResponsesWebsocketMaxLifetime + codexResponsesWebsocketParkTTL/2))

	if executor.parkExecutionSession(sess) {
		t.Fatal("expiring websocket session should not be parked")
	}

	executor.store.parkedMu.Lock()
	defer executor.store.parkedMu.Unlock()
	if got := len(executor.store.parked); got != 0 {
		t.Fatalf("parked sessions = %d, want 0", got)
	}
}

func contextWithGinHeaders(headers map[string]string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ginCtx.Request.Header = make(http.Header, len(headers))
	for key, value := range headers {
		ginCtx.Request.Header.Set(key, value)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func assertGeneratedCodexTurnMetadata(t *testing.T, raw string) {
	t.Helper()

	if strings.TrimSpace(raw) == "" {
		t.Fatal("X-Codex-Turn-Metadata should be generated by default")
	}

	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		t.Fatalf("X-Codex-Turn-Metadata should be valid JSON: %v", err)
	}

	turnID, _ := metadata["turn_id"].(string)
	if strings.TrimSpace(turnID) == "" {
		t.Fatalf("turn_id = %q, want non-empty", turnID)
	}
	if requestKind, _ := metadata["request_kind"].(string); requestKind != codexTurnRequestKind {
		t.Fatalf("request_kind = %q, want %q", requestKind, codexTurnRequestKind)
	}
	sessionID, _ := metadata["session_id"].(string)
	if strings.TrimSpace(sessionID) == "" {
		t.Fatalf("session_id = %q, want non-empty", sessionID)
	}
	threadID, _ := metadata["thread_id"].(string)
	if strings.TrimSpace(threadID) == "" {
		t.Fatalf("thread_id = %q, want non-empty", threadID)
	}
	if threadSource := metadata["thread_source"]; threadSource != nil {
		t.Fatalf("thread_source = %#v, want nil", threadSource)
	}
	if sandbox, _ := metadata["sandbox"].(string); sandbox != codexDefaultSandboxTag {
		t.Fatalf("sandbox = %q, want %q", sandbox, codexDefaultSandboxTag)
	}
	if windowID, _ := metadata["window_id"].(string); strings.TrimSpace(windowID) == "" {
		t.Fatalf("window_id = %q, want non-empty", windowID)
	}
	if startedAt, _ := metadata["turn_started_at_unix_ms"].(float64); startedAt <= 0 {
		t.Fatalf("turn_started_at_unix_ms = %.0f, want positive", startedAt)
	}
}

func assertCodexTurnMetadataString(t *testing.T, raw string, path string, want string) {
	t.Helper()

	if strings.TrimSpace(raw) == "" {
		t.Fatalf("%s should be non-empty", codexHeaderTurnMetadata)
	}
	if !gjson.Valid(raw) {
		t.Fatalf("%s should be valid JSON: %q", codexHeaderTurnMetadata, raw)
	}
	if got := strings.TrimSpace(gjson.Get(raw, path).String()); got != want {
		t.Fatalf("%s.%s = %q, want %q in %s", codexHeaderTurnMetadata, path, got, want, raw)
	}
}

func TestNewProxyAwareWebsocketDialerDirectDisablesProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy != nil {
		t.Fatal("expected websocket proxy function to be nil for direct mode")
	}
}
