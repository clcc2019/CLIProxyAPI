package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func ctxWithAPIKey(t *testing.T, apiKey string) context.Context {
	return ctxWithAPIKeyAndHeaders(t, apiKey, nil)
}

func ctxWithAPIKeyAndHeaders(t *testing.T, apiKey string, headers http.Header) context.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	if apiKey != "" {
		ginCtx.Set("apiKey", apiKey)
	}
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if headers != nil {
		ginCtx.Request.Header = headers.Clone()
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestCodexExecutorCacheHelper_OpenAIChatCompletions_StablePromptCacheKeyFromAPIKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("userApiKey", "test-api-key")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	executor := &CodexExecutor{}
	rawJSON := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex"}`),
	}
	url := "https://example.com/responses"

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	expectedKey := uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:test-api-key")).String()
	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey != expectedKey {
		t.Fatalf("prompt_cache_key = %q, want %q", gotKey, expectedKey)
	}
	if gotConversation := httpReq.Header.Get("Conversation_id"); gotConversation != "" {
		t.Fatalf("Conversation_id = %q, want empty", gotConversation)
	}
	if gotSession := httpReq.Header.Get("Session_id"); gotSession != expectedKey {
		t.Fatalf("Session_id = %q, want %q", gotSession, expectedKey)
	}

	httpReq2, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error (second call): %v", err)
	}
	body2, errRead2 := io.ReadAll(httpReq2.Body)
	if errRead2 != nil {
		t.Fatalf("read request body (second call): %v", errRead2)
	}
	gotKey2 := gjson.GetBytes(body2, "prompt_cache_key").String()
	if gotKey2 != expectedKey {
		t.Fatalf("prompt_cache_key (second call) = %q, want %q", gotKey2, expectedKey)
	}
}

func TestCodexExecutorCacheHelper_ConcurrentSyntheticPromptCacheColdStartUsesOneID(t *testing.T) {
	executor := &CodexExecutor{}
	apiKey := "api-" + uuid.NewString()
	user := "user-" + uuid.NewString()
	payload := []byte(`{"model":"gpt-5","user":"` + user + `","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	type result struct {
		key string
		err error
	}
	const workers = 32
	contexts := make([]context.Context, workers)
	for i := range contexts {
		contexts[i] = ctxWithAPIKey(t, apiKey)
	}
	start := make(chan struct{})
	results := make(chan result, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		ctx := contexts[i]
		go func() {
			defer wg.Done()
			<-start
			httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", req, payload)
			if err != nil {
				results <- result{err: err}
				return
			}
			body, err := io.ReadAll(httpReq.Body)
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{key: gjson.GetBytes(body, "prompt_cache_key").String()}
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	var first string
	for res := range results {
		if res.err != nil {
			t.Fatalf("cacheHelper error: %v", res.err)
		}
		if res.key == "" {
			t.Fatal("prompt_cache_key is empty")
		}
		if first == "" {
			first = res.key
			continue
		}
		if res.key != first {
			t.Fatalf("concurrent cold start produced multiple prompt_cache_key values: first=%q got=%q", first, res.key)
		}
	}
}

func assertPromptCacheKey(t *testing.T, executor *CodexExecutor, ctx context.Context, format string, req cliproxyexecutor.Request, rawJSON []byte) string {
	t.Helper()
	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString(format), "https://example.com/responses", req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return gjson.GetBytes(body, "prompt_cache_key").String()
}

func assertPreparedSessionID(t *testing.T, executor *CodexExecutor, ctx context.Context, format string, url string, req cliproxyexecutor.Request, rawJSON []byte) string {
	t.Helper()
	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString(format), url, req, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	return httpReq.Header.Get(codexHeaderSessionID)
}

func TestHashCodexFinalUpstreamBodyMemoKeyIsDeterministicAndDistinguishing(t *testing.T) {
	payload := append([]byte(`{"model":"gpt-5","input":"`), bytes.Repeat([]byte("secret-prompt-"), 512)...)
	payload = append(payload, []byte(`"}`)...)
	opts := codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	}

	key := hashCodexFinalUpstreamBodyMemoKey("gpt-5", opts, payload)
	if keyAgain := hashCodexFinalUpstreamBodyMemoKey("gpt-5", opts, payload); keyAgain != key {
		t.Fatalf("same payload produced different memo keys")
	}

	otherPayload := bytes.Clone(payload)
	otherPayload[len(otherPayload)-3] = 'x'
	if otherKey := hashCodexFinalUpstreamBodyMemoKey("gpt-5", opts, otherPayload); otherKey == key {
		t.Fatalf("different payloads produced same memo key")
	}

	otherOpts := opts
	otherOpts.streamMode = codexStreamFieldDelete
	if otherKey := hashCodexFinalUpstreamBodyMemoKey("gpt-5", otherOpts, payload); otherKey == key {
		t.Fatalf("different options produced same memo key")
	}

	otherOpts = opts
	otherOpts.preserveGenerate = true
	if otherKey := hashCodexFinalUpstreamBodyMemoKey("gpt-5", otherOpts, payload); otherKey == key {
		t.Fatalf("different preserveGenerate option produced same memo key")
	}

	otherOpts = opts
	otherOpts.omitServiceTier = true
	if otherKey := hashCodexFinalUpstreamBodyMemoKey("gpt-5", otherOpts, payload); otherKey == key {
		t.Fatalf("different omitServiceTier option produced same memo key")
	}

	otherOpts = opts
	otherOpts.suppressDefaultInstructions = true
	if otherKey := hashCodexFinalUpstreamBodyMemoKey("gpt-5", otherOpts, payload); otherKey == key {
		t.Fatalf("different suppressDefaultInstructions option produced same memo key")
	}
}

func BenchmarkHashCodexFinalUpstreamBodyMemoKeyLargePayload(b *testing.B) {
	payload := append([]byte(`{"model":"gpt-5","input":"`), bytes.Repeat([]byte("large-prompt-"), 4096)...)
	payload = append(payload, []byte(`"}`)...)
	opts := codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = hashCodexFinalUpstreamBodyMemoKey("gpt-5", opts, payload)
	}
}

func TestCodexExecutorCacheHelper_CallerProvidedKeyIsPassedThrough(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "some-api-key")

	payload := []byte(`{"model":"gpt-5","prompt_cache_key":"caller-owned-id","input":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	got := assertPromptCacheKey(t, executor, ctx, "openai-response", req, payload)
	if got != "caller-owned-id" {
		t.Fatalf("expected caller-owned id to pass through, got %q", got)
	}
}

func TestCodexExecutorCacheHelper_ConversationHeadersBecomePromptCacheKey(t *testing.T) {
	executor := &CodexExecutor{}

	tests := []struct {
		name   string
		header string
		value  string
	}{
		{name: "conversation", header: "Conversation_id", value: "conv-header"},
		{name: "thread", header: codexHeaderThreadID, value: "thread-header"},
		{name: "official-thread", header: codexHeaderOfficialThreadID, value: "official-thread-header"},
		{name: "session", header: "Session_id", value: "session-header"},
		{name: "official-session", header: codexHeaderOfficialSessionID, value: "official-session-header"},
		{name: "x-session", header: "X-Session-ID", value: "x-session-header"},
	}

	for _, tc := range tests {
		headers := http.Header{}
		headers.Set(tc.header, tc.value)
		ctx := ctxWithAPIKeyAndHeaders(t, "api-key-header", headers)
		payload := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"hello"}]}`)
		req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

		httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", req, payload)
		if err != nil {
			t.Fatalf("%s: cacheHelper error: %v", tc.name, err)
		}
		body, err := io.ReadAll(httpReq.Body)
		if err != nil {
			t.Fatalf("%s: read body: %v", tc.name, err)
		}

		if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != tc.value {
			t.Fatalf("%s: prompt_cache_key = %q, want %q; body=%s", tc.name, got, tc.value, body)
		}
		if got := httpReq.Header.Get(codexHeaderSessionID); got != tc.value {
			t.Fatalf("%s: Session_id = %q, want %q", tc.name, got, tc.value)
		}
	}
}

func TestCodexExecutorCacheHelper_OfficialThreadHeaderBecomesPromptCacheKey(t *testing.T) {
	executor := &CodexExecutor{}
	headers := http.Header{}
	headers.Set(codexHeaderOfficialSessionID, "official-session")
	headers.Set(codexHeaderOfficialThreadID, "official-thread")
	ctx := ctxWithAPIKeyAndHeaders(t, "api-key-official", headers)

	payload := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}
	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", req, payload)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "official-thread" {
		t.Fatalf("prompt_cache_key = %q, want official-thread; body=%s", got, body)
	}
	if got := httpReq.Header.Get(codexHeaderSessionID); got != "official-session" {
		t.Fatalf("%s = %q, want official-session", codexHeaderSessionID, got)
	}
	if got := httpReq.Header.Get(codexHeaderThreadID); got != "official-thread" {
		t.Fatalf("%s = %q, want official-thread", codexHeaderThreadID, got)
	}
}

func TestCodexExecutorCacheHelper_TurnMetadataThreadBecomesPromptCacheKey(t *testing.T) {
	executor := &CodexExecutor{}
	headers := http.Header{}
	headers.Set(codexHeaderTurnMetadata, `{"session_id":"meta-session","thread_id":"meta-thread"}`)
	ctx := ctxWithAPIKeyAndHeaders(t, "api-key-turn-metadata", headers)

	payload := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}
	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", req, payload)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "meta-thread" {
		t.Fatalf("prompt_cache_key = %q, want meta-thread; body=%s", got, body)
	}
	if got := httpReq.Header.Get(codexHeaderSessionID); got != "meta-session" {
		t.Fatalf("%s = %q, want meta-session", codexHeaderSessionID, got)
	}
	if got := httpReq.Header.Get(codexHeaderThreadID); got != "meta-thread" {
		t.Fatalf("%s = %q, want meta-thread", codexHeaderThreadID, got)
	}
}

func TestCodexExecutorCacheHelper_BodyTurnMetadataThreadBecomesPromptCacheKey(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-body-turn-metadata")

	payload := []byte(`{"model":"gpt-5","client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"body-session\",\"thread_id\":\"body-thread\"}"},"input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}
	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", req, payload)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "body-thread" {
		t.Fatalf("prompt_cache_key = %q, want body-thread; body=%s", got, body)
	}
	if got := httpReq.Header.Get(codexHeaderSessionID); got != "body-session" {
		t.Fatalf("%s = %q, want body-session", codexHeaderSessionID, got)
	}
	if got := httpReq.Header.Get(codexHeaderThreadID); got != "body-thread" {
		t.Fatalf("%s = %q, want body-thread", codexHeaderThreadID, got)
	}
}

func TestCodexExecutorCacheHelper_BodyPromptCacheKeyBeatsConversationHeader(t *testing.T) {
	executor := &CodexExecutor{}
	headers := http.Header{}
	headers.Set("Conversation_id", "conv-header")
	ctx := ctxWithAPIKeyAndHeaders(t, "api-key-header", headers)

	payload := []byte(`{"model":"gpt-5","prompt_cache_key":"body-cache","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", req, payload)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "body-cache" {
		t.Fatalf("prompt_cache_key = %q, want body-cache; body=%s", got, body)
	}
	if got := httpReq.Header.Get(codexHeaderSessionID); got != "body-cache" {
		t.Fatalf("Session_id = %q, want body-cache", got)
	}
	if got := httpReq.Header.Get(codexHeaderThreadID); got != "body-cache" {
		t.Fatalf("Thread_id = %q, want body-cache", got)
	}
}

func TestPrepareCodexHTTPCallPreservesOfficialCLIIdentityHeaders(t *testing.T) {
	resetCodexWindowStateStore()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex"}
	headers := http.Header{}
	headers.Set(codexHeaderSessionID, "cli-session")
	headers.Set(codexHeaderThreadID, "cli-thread")
	ctx := ctxWithAPIKeyAndHeaders(t, "api-key-cli", headers)

	payload := []byte(`{"model":"gpt-5","prompt_cache_key":"cli-thread","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	call, err := executor.prepareCodexHTTPCall(
		ctx,
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.com/responses",
		req,
		payload,
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall error: %v", err)
	}

	if got := gjson.GetBytes(call.prepared.body, "prompt_cache_key").String(); got != "cli-thread" {
		t.Fatalf("prompt_cache_key = %q, want cli-thread; body=%s", got, call.prepared.body)
	}
	if got := call.prepared.httpReq.Header.Get(codexHeaderSessionID); got != "cli-session" {
		t.Fatalf("Session_id = %q, want cli-session", got)
	}
	if got := call.prepared.httpReq.Header.Get(codexHeaderThreadID); got != "cli-thread" {
		t.Fatalf("Thread_id = %q, want cli-thread", got)
	}
	if got := call.prepared.httpReq.Header.Get("X-Client-Request-Id"); got != "cli-thread" {
		t.Fatalf("X-Client-Request-Id = %q, want cli-thread", got)
	}
	if got := call.prepared.httpReq.Header.Get(codexHeaderWindowID); got != "cli-thread:0" {
		t.Fatalf("%s = %q, want cli-thread:0", codexHeaderWindowID, got)
	}
}

func TestPrepareCodexHTTPCallUsesOfficialThreadHeaderForPromptCache(t *testing.T) {
	resetCodexWindowStateStore()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex"}
	headers := http.Header{}
	headers.Set(codexHeaderOfficialSessionID, "official-session")
	headers.Set(codexHeaderOfficialThreadID, "official-thread")
	ctx := ctxWithAPIKeyAndHeaders(t, "api-key-official", headers)

	payload := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}
	call, err := executor.prepareCodexHTTPCall(
		ctx,
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.com/responses",
		req,
		payload,
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall error: %v", err)
	}

	if got := gjson.GetBytes(call.prepared.body, "prompt_cache_key").String(); got != "official-thread" {
		t.Fatalf("prompt_cache_key = %q, want official-thread; body=%s", got, call.prepared.body)
	}
	if got := call.prepared.httpReq.Header.Get(codexHeaderSessionID); got != "official-session" {
		t.Fatalf("%s = %q, want official-session", codexHeaderSessionID, got)
	}
	if got := call.prepared.httpReq.Header.Get(codexHeaderThreadID); got != "official-thread" {
		t.Fatalf("%s = %q, want official-thread", codexHeaderThreadID, got)
	}
	if got := call.prepared.httpReq.Header.Get(codexHeaderOfficialSessionID); got != "official-session" {
		t.Fatalf("%s = %q, want official-session", codexHeaderOfficialSessionID, got)
	}
	if got := call.prepared.httpReq.Header.Get(codexHeaderOfficialThreadID); got != "official-thread" {
		t.Fatalf("%s = %q, want official-thread", codexHeaderOfficialThreadID, got)
	}
	if got := call.prepared.httpReq.Header.Get("X-Client-Request-Id"); got != "official-thread" {
		t.Fatalf("X-Client-Request-Id = %q, want official-thread", got)
	}
}

func TestCodexExecutorCacheHelper_DifferentConversationsGetDifferentKeys(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-shared")

	convA := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"first question about python"}]}`)
	convB := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"completely different topic about cooking"}]}`)

	keyA := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: convA}, convA)
	keyB := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: convB}, convB)

	if keyA == "" || keyB == "" {
		t.Fatalf("expected non-empty keys, got %q and %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("two different conversations must not share a prompt_cache_key, got %q for both", keyA)
	}
}

func TestCodexExecutorCacheHelper_OpenAIUserDoesNotCollapseDifferentConversations(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-openai-user")

	convA := []byte(`{"model":"gpt-5","user":"end-user-1","messages":[{"role":"user","content":"first question about python"}]}`)
	convB := []byte(`{"model":"gpt-5","user":"end-user-1","messages":[{"role":"user","content":"completely different topic about cooking"}]}`)
	convC := []byte(`{"model":"gpt-5","user":"end-user-2","messages":[{"role":"user","content":"first question about python"}]}`)

	keyA := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: convA}, convA)
	keyB := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: convB}, convB)
	keyC := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: convC}, convC)

	if keyA == "" || keyB == "" || keyC == "" {
		t.Fatalf("expected non-empty keys, got %q, %q, %q", keyA, keyB, keyC)
	}
	if keyA == keyB {
		t.Fatalf("same OpenAI user with different conversations must not share prompt_cache_key: %q", keyA)
	}
	if keyA == keyC {
		t.Fatalf("different OpenAI users with same first message must not share prompt_cache_key: %q", keyA)
	}
}

func TestCodexExecutorCacheHelper_SameConversationReusesKeyAcrossTurns(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-convos")

	turn1 := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"explain closures"}]}`)
	turn2 := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"explain closures"},{"role":"assistant","content":"..."},{"role":"user","content":"show an example"}]}`)

	keyTurn1 := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: turn1}, turn1)
	keyTurn2 := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: turn2}, turn2)

	if keyTurn1 == "" || keyTurn2 == "" {
		t.Fatalf("expected non-empty keys, got %q and %q", keyTurn1, keyTurn2)
	}
	if keyTurn1 != keyTurn2 {
		t.Fatalf("same conversation must reuse prompt_cache_key across turns: turn1=%q turn2=%q", keyTurn1, keyTurn2)
	}
}

func TestCodexExecutorCacheHelper_ConversationIDFieldPreferredOverContent(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-field")

	p1 := []byte(`{"model":"gpt-5","metadata":{"conversation_id":"conv-42"},"messages":[{"role":"user","content":"first"}]}`)
	p2 := []byte(`{"model":"gpt-5","metadata":{"conversation_id":"conv-42"},"messages":[{"role":"user","content":"second"}]}`)

	k1 := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: p1}, p1)
	k2 := assertPromptCacheKey(t, executor, ctx, "openai", cliproxyexecutor.Request{Model: "gpt-5", Payload: p2}, p2)
	if k1 != "conv-42" || k2 != "conv-42" {
		t.Fatalf("explicit conversation_id must pass through as prompt_cache_key: got %q vs %q", k1, k2)
	}
}

func TestCodexExecutorCacheHelper_CompactUsesCallerProvidedPromptCacheKeyAsSessionID(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-compact")

	payload := []byte(`{"model":"gpt-5","prompt_cache_key":"caller-owned-id","input":"hello"}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses/compact", req, payload)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "caller-owned-id" {
		t.Fatalf("prompt_cache_key = %q, want caller-owned-id; body=%s", got, body)
	}
	if got := httpReq.Header.Get(codexHeaderSessionID); got != "caller-owned-id" {
		t.Fatalf("Session_id = %q, want %q", got, "caller-owned-id")
	}
	if got := httpReq.Header.Get(codexHeaderThreadID); got != "caller-owned-id" {
		t.Fatalf("Thread_id = %q, want %q", got, "caller-owned-id")
	}
}

func TestCodexExecutorCacheHelper_CompactUsesExplicitConversationHintSessionID(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-compact")

	payload := []byte(`{"model":"gpt-5","metadata":{"conversation_id":"conv-42"},"input":"hello"}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	expected := assertPromptCacheKey(t, executor, ctx, "openai-response", req, payload)
	got := assertPreparedSessionID(t, executor, ctx, "openai-response", "https://example.com/responses/compact", req, payload)
	if got != expected {
		t.Fatalf("Session_id = %q, want prompt-cache-derived id %q", got, expected)
	}
}

func TestCodexExecutorCacheHelper_ExecutionSessionMetadataShortCircuitsPayloadFingerprinting(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-exec-session")

	payload := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	memoEntriesBefore := globalCodexPromptResolutionMemo.orderLen()
	resolutionA := executor.resolvePromptCacheResolution(ctx, "openai", "exec-session-1", req)
	resolutionB := executor.resolvePromptCacheResolution(ctx, "openai", "exec-session-1", req)
	resolutionC := executor.resolvePromptCacheResolution(ctx, "openai", "exec-session-2", req)
	if resolutionA.cache.ID == "" {
		t.Fatal("expected execution session resolution to produce a prompt cache id")
	}
	if resolutionA.cache.ID != "exec-session-1" {
		t.Fatalf("execution session prompt_cache_key = %q, want exec-session-1", resolutionA.cache.ID)
	}
	if resolutionA.headerEligibleID != "exec-session-1" {
		t.Fatalf("execution session headerEligibleID = %q, want exec-session-1", resolutionA.headerEligibleID)
	}
	if resolutionA.cache.ID != resolutionB.cache.ID {
		t.Fatalf("same execution session should reuse cache id: %q vs %q", resolutionA.cache.ID, resolutionB.cache.ID)
	}
	if resolutionA.cache.ID == resolutionC.cache.ID {
		t.Fatalf("different execution sessions must not share cache id: %q", resolutionA.cache.ID)
	}
	if got := globalCodexPromptResolutionMemo.orderLen(); got != memoEntriesBefore {
		t.Fatalf("execution session prompt cache resolution should bypass payload memo: entries before=%d after=%d", memoEntriesBefore, got)
	}
}

func TestCodexExecutorCacheHelper_LongExecutionSessionPromptCacheKeyIsStableAndBounded(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-long-exec-session")
	payload := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}
	sessionID := strings.Repeat("session-", 20)

	resolutionA := executor.resolvePromptCacheResolution(ctx, "openai", sessionID, req)
	resolutionB := executor.resolvePromptCacheResolution(ctx, "openai", sessionID, req)
	if resolutionA.cache.ID == "" {
		t.Fatal("expected long execution session to produce a prompt cache id")
	}
	if len(resolutionA.cache.ID) > codexPromptCacheKeyMaxLen {
		t.Fatalf("prompt_cache_key length = %d, want <= %d: %q", len(resolutionA.cache.ID), codexPromptCacheKeyMaxLen, resolutionA.cache.ID)
	}
	if resolutionA.cache.ID != resolutionB.cache.ID {
		t.Fatalf("long execution session prompt_cache_key should be stable: %q vs %q", resolutionA.cache.ID, resolutionB.cache.ID)
	}
	if resolutionA.cache.ID == sessionID {
		t.Fatalf("long execution session should be compacted before upstream use")
	}
}

func TestCodexExecutorCacheHelper_ExecutionSessionBecomesPromptCacheKey(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "api-key-exec-session-body")
	payload := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	httpReq, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", req, payload)
	if err != nil {
		t.Fatalf("cacheHelper without execution session error: %v", err)
	}
	bodyWithoutSession, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body without execution session: %v", err)
	}

	prepared, err := executor.prepareCodexRequestWithKind(ctx, sdktranslator.FromString("openai-response"), "exec-session-1", "https://example.com/responses", codexFinalUpstreamResponses, req, payload)
	if err != nil {
		t.Fatalf("prepareCodexRequestWithKind error: %v", err)
	}
	bodyWithSession, err := io.ReadAll(prepared.httpReq.Body)
	if err != nil {
		t.Fatalf("read body with execution session: %v", err)
	}

	if got := gjson.GetBytes(bodyWithSession, "prompt_cache_key").String(); got != "exec-session-1" {
		t.Fatalf("prompt_cache_key = %q, want exec-session-1; body=%s", got, bodyWithSession)
	}
	if got := prepared.httpReq.Header.Get(codexHeaderSessionID); got != "exec-session-1" {
		t.Fatalf("%s = %q, want exec-session-1", codexHeaderSessionID, got)
	}
	if got := prepared.httpReq.Header.Get(codexHeaderThreadID); got != "exec-session-1" {
		t.Fatalf("%s = %q, want exec-session-1", codexHeaderThreadID, got)
	}
	if without := gjson.GetBytes(bodyWithoutSession, "prompt_cache_key").String(); without == "exec-session-1" {
		t.Fatalf("request without execution session unexpectedly used execution prompt_cache_key: %s", bodyWithoutSession)
	}
}

func TestCodexExecutorCacheHelper_DifferentTenantsDoNotCollide(t *testing.T) {
	executor := &CodexExecutor{}

	payload := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}

	k1 := assertPromptCacheKey(t, executor, ctxWithAPIKey(t, "tenant-a"), "openai", req, payload)
	k2 := assertPromptCacheKey(t, executor, ctxWithAPIKey(t, "tenant-b"), "openai", req, payload)
	if k1 == "" || k1 == k2 {
		t.Fatalf("different tenants must not share prompt_cache_key: got %q for both", k1)
	}
}

func TestCodexExecutorCacheHelper_ClaudeUserIDBackwardsCompatible(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := ctxWithAPIKey(t, "")

	payload := []byte(`{"model":"claude-sonnet","metadata":{"user_id":"u-7"},"messages":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "claude-sonnet", Payload: payload}

	k1 := assertPromptCacheKey(t, executor, ctx, "claude", req, payload)
	k2 := assertPromptCacheKey(t, executor, ctx, "claude", req, payload)
	if k1 == "" || k1 != k2 {
		t.Fatalf("Claude path must keep stable id from metadata.user_id: got %q and %q", k1, k2)
	}
	if _, ok := helps.GetCodexCache("claude-sonnet-u-7"); !ok {
		t.Fatalf("expected legacy claude cache key to be populated")
	}
}

func TestHashCodexDedupeHeaders_IgnoresTraceAndTimingHeaders(t *testing.T) {
	left := http.Header{
		"Content-Type":                          []string{"application/json"},
		"X-Codex-Turn-Metadata":                 []string{`{"turn_id":"turn-left","sandbox":"none"}`},
		"Traceparent":                           []string{"00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"},
		"Tracestate":                            []string{"vendor-a=value-a"},
		"X-Responsesapi-Include-Timing-Metrics": []string{"1"},
		"X-Client-Request-Id":                   []string{"req-left"},
	}
	right := http.Header{
		"Content-Type":                          []string{"application/json"},
		"X-Codex-Turn-Metadata":                 []string{`{"turn_id":"turn-right","sandbox":"none"}`},
		"Traceparent":                           []string{"00-cccccccccccccccccccccccccccccccc-dddddddddddddddd-01"},
		"Tracestate":                            []string{"vendor-b=value-b"},
		"X-Responsesapi-Include-Timing-Metrics": []string{"0"},
		"X-Client-Request-Id":                   []string{"req-right"},
	}

	leftHash := hashCodexDedupeHeaders(left)
	rightHash := hashCodexDedupeHeaders(right)
	if leftHash != rightHash {
		t.Fatalf("hashCodexDedupeHeaders() mismatch: left=%q right=%q", leftHash, rightHash)
	}
}

func TestHashCodexDedupeHeaders_DistinguishesRelevantHeaders(t *testing.T) {
	left := http.Header{
		"Session_id":              []string{"session-a"},
		"OpenAI-Beta":             []string{"responses=v1"},
		"X-Codex-Beta-Features":   []string{"beta-a"},
		"X-Codex-Installation-Id": []string{"installation-1"},
		misc.CodexResidencyHeader: []string{"us"},
	}
	right := http.Header{
		"Session_id":              []string{"session-b"},
		"OpenAI-Beta":             []string{"responses=v1"},
		"X-Codex-Beta-Features":   []string{"beta-a"},
		"X-Codex-Installation-Id": []string{"installation-1"},
		misc.CodexResidencyHeader: []string{"us"},
	}

	if leftHash, rightHash := hashCodexDedupeHeaders(left), hashCodexDedupeHeaders(right); leftHash == rightHash {
		t.Fatalf("hashCodexDedupeHeaders() should differ for relevant headers: left=%q right=%q", leftHash, rightHash)
	}
}

func TestHashCodexDedupeHeadersReadsCanonicalizedHeaders(t *testing.T) {
	left := http.Header{}
	left.Set("OpenAI-Beta", "responses=v1")
	left.Set(misc.CodexResidencyHeader, "us")

	right := http.Header{}
	right.Set("OpenAI-Beta", "responses=v1")
	right.Set(misc.CodexResidencyHeader, "eu")

	leftHash := hashCodexDedupeHeaders(left)
	rightHash := hashCodexDedupeHeaders(right)
	if leftHash == "none" {
		t.Fatal("hashCodexDedupeHeaders() ignored canonicalized relevant headers")
	}
	if leftHash == rightHash {
		t.Fatalf("hashCodexDedupeHeaders() should distinguish canonicalized residency header: left=%q right=%q", leftHash, rightHash)
	}
}

func BenchmarkHashCodexDedupeHeaders(b *testing.B) {
	headers := http.Header{
		"Accept":                  []string{"text/event-stream"},
		"Content-Type":            []string{"application/json"},
		"Session_id":              []string{"session-123"},
		"OpenAI-Beta":             []string{"responses=v1"},
		"X-Codex-Beta-Features":   []string{"beta-a"},
		"X-Codex-Installation-Id": []string{"installation-123"},
		misc.CodexResidencyHeader: []string{"us"},
		"Traceparent":             []string{"00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"},
		"Tracestate":              []string{"vendor-a=value-a"},
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = hashCodexDedupeHeaders(headers)
	}
}

func TestPrepareCodexHTTPCallAppliesHeadersAndPreservesLogBody(t *testing.T) {
	t.Setenv(codexCompressionEnv, "1")

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct_123"},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}
	rawJSON := []byte(`{"model":"gpt-5.4","input":"hello"}`)

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.com/responses",
		req,
		rawJSON,
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}

	if got := call.prepared.httpReq.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer oauth-token")
	}
	if got := call.prepared.httpReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want %q", got, "text/event-stream")
	}
	if got := call.prepared.httpReq.Header.Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("Content-Encoding = %q, want %q", got, "zstd")
	}
	if !bytes.Equal(call.requestLog.Body, call.prepared.body) {
		t.Fatalf("requestLog.Body = %q, want prepared body %q", string(call.requestLog.Body), string(call.prepared.body))
	}
	if id := gjson.GetBytes(call.prepared.body, "client_metadata.x-codex-installation-id").String(); id == "" {
		t.Fatalf("prepared body should include client_metadata.x-codex-installation-id, got %s", call.prepared.body)
	}
	if got := call.requestLog.URL; got != "https://example.com/responses" {
		t.Fatalf("requestLog.URL = %q, want %q", got, "https://example.com/responses")
	}
	if got := call.requestLog.Headers.Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("requestLog.Headers[Content-Encoding] = %q, want %q", got, "zstd")
	}
}

func TestPrepareCodexHTTPCallNormalizesFinalUpstreamBody(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","store":true}`),
	}
	rawJSON := []byte(`{
		"model":"wrong-model",
		"input":"hello",
		"tools":"bad",
		"parallel_tool_calls":"false",
		"service_tier":123,
		"prompt_cache_key":"",
		"store":true,
		"stream":false,
		"generate":false,
		"prompt_cache_retention":"24h",
		"stream_options":{"include_usage":true},
		"temperature":0.2,
		"context_management":{"compaction":"auto"},
		"previous_response_id":"resp_1"
	}`)

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.com/responses",
		req,
		rawJSON,
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}

	body := call.prepared.body
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.4" {
		t.Fatalf("model = %q, want %q", got, "gpt-5.4")
	}
	if got := gjson.GetBytes(body, "store").Bool(); got {
		t.Fatalf("store = true, want false; body=%s", body)
	}
	if got := gjson.GetBytes(body, "stream").Bool(); !got {
		t.Fatalf("stream = false, want true; body=%s", body)
	}
	if got := gjson.GetBytes(body, "prompt_cache_retention"); got.Exists() {
		t.Fatalf("prompt_cache_retention should be removed from final upstream body: %s", body)
	}
	for _, field := range []string{"stream_options", "temperature", "context_management"} {
		if gjson.GetBytes(body, field).Exists() {
			t.Fatalf("%s should be removed from final upstream body: %s", field, body)
		}
	}
	if got := gjson.GetBytes(body, "previous_response_id"); got.Exists() {
		t.Fatalf("previous_response_id should be removed from HTTP Responses body: %s", body)
	}
	if got := gjson.GetBytes(body, "generate"); got.Exists() {
		t.Fatalf("generate should be removed from HTTP Responses body: %s", body)
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
	if got := gjson.GetBytes(body, "parallel_tool_calls").Bool(); got {
		t.Fatalf("parallel_tool_calls = true, want false from string compatibility input; body=%s", body)
	}
	if got := gjson.GetBytes(body, "parallel_tool_calls"); got.Type != gjson.False {
		t.Fatalf("parallel_tool_calls type = %v, want JSON false; body=%s", got.Type, body)
	}
	if got := gjson.GetBytes(body, "service_tier"); got.Exists() {
		t.Fatalf("invalid service_tier should be removed from final upstream body: %s", body)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key"); got.Exists() {
		t.Fatalf("empty prompt_cache_key should be removed from final upstream body: %s", body)
	}
	if got := gjson.GetBytes(body, "include").IsArray(); !got {
		t.Fatalf("include should default to an empty array: %s", body)
	}
}

func TestNormalizeCodexFinalUpstreamBodyDefaultsJsonSchemaTextFormatName(t *testing.T) {
	body := []byte(`{
		"model":"wrong-model",
		"input":"hello",
		"text":{
			"format":{
				"type":"json_schema",
				"name":" ",
				"strict":false,
				"schema":{"type":"object","properties":{"answer":{"type":"string"}}}
			},
			"verbosity":"low"
		}
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", &cliproxyauth.Auth{Provider: "codex"}, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})

	if got := gjson.GetBytes(gotBody, "text.format.name").String(); got != codexDefaultOutputSchemaTextFormatName {
		t.Fatalf("text.format.name = %q, want %q; body=%s", got, codexDefaultOutputSchemaTextFormatName, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text.format.strict"); got.Type != gjson.False {
		t.Fatalf("text.format.strict should remain JSON false; got %s body=%s", got.Raw, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text.format.schema.properties.answer.type").String(); got != "string" {
		t.Fatalf("text.format.schema not preserved; body=%s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text.verbosity").String(); got != "low" {
		t.Fatalf("text.verbosity = %q, want low; body=%s", got, gotBody)
	}
}

func TestPrepareCodexHTTPCallDropsServiceTierByDefault(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex"}
	req := cliproxyexecutor.Request{Model: "gpt-5.4"}

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.com/responses",
		req,
		[]byte(`{"model":"gpt-5.4","input":"hello","service_tier":"priority"}`),
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}
	if got := gjson.GetBytes(call.prepared.body, "service_tier"); got.Exists() {
		t.Fatalf("service_tier should be omitted by default: %s", call.prepared.body)
	}
}

func TestPrepareCodexHTTPCallPreservesServiceTierWhenAuthOptedIn(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{cliproxyauth.AuthFileServiceTierPassthroughKey: true},
	}
	req := cliproxyexecutor.Request{Model: "gpt-5.4"}

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.com/responses",
		req,
		[]byte(`{"model":"gpt-5.4","input":"hello","service_tier":"flex"}`),
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}
	if got := gjson.GetBytes(call.prepared.body, "service_tier").String(); got != "flex" {
		t.Fatalf("service_tier = %q, want flex; body=%s", got, call.prepared.body)
	}
}

func TestPrepareCodexHTTPCallUsesStoreForAzureResponsesEndpoint(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex"}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"hello"}]}],"store":false}`),
	}
	rawJSON := []byte(`{"model":"gpt-5.4","input":[{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"hello"}]}],"store":false}`)

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.openai.azure.com/openai/responses",
		req,
		rawJSON,
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}
	if got := gjson.GetBytes(call.prepared.body, "store").Bool(); !got {
		t.Fatalf("store = false, want true for Azure Responses endpoint; body=%s", call.prepared.body)
	}
	if got := gjson.GetBytes(call.prepared.body, "input.0.id").String(); got != "msg_1" {
		t.Fatalf("input.0.id = %q, want msg_1; body=%s", got, call.prepared.body)
	}
}

func TestPrepareCodexHTTPCallDoesNotCompressCompactRequests(t *testing.T) {
	t.Setenv(codexCompressionEnv, "1")

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct_123"},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.com/responses/compact",
		req,
		req.Payload,
		"oauth-token",
		false,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}
	if got := call.prepared.httpReq.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty for responses/compact", got)
	}
}

func TestPrepareCodexHTTPCallIncludesEncryptedReasoningContentWhenReasoningRequested(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex"}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","reasoning":{"effort":"high"},"include":[123,"file_search_call.results",{"bad":true},"file_search_call.results",""]}`),
	}

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://example.com/responses",
		req,
		req.Payload,
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}

	if got := gjson.GetBytes(call.prepared.body, "include.#").Int(); got != 2 {
		t.Fatalf("include length = %d, want 2; body=%s", got, call.prepared.body)
	}
	if got := gjson.GetBytes(call.prepared.body, `include.#(=="reasoning.encrypted_content")`).String(); got != "reasoning.encrypted_content" {
		t.Fatalf("include missing reasoning.encrypted_content; body=%s", call.prepared.body)
	}
	if got := gjson.GetBytes(call.prepared.body, `include.#(=="file_search_call.results")`).String(); got != "file_search_call.results" {
		t.Fatalf("include missing original value; body=%s", call.prepared.body)
	}
	for _, item := range gjson.GetBytes(call.prepared.body, "include").Array() {
		if item.Type != gjson.String || strings.TrimSpace(item.String()) == "" {
			t.Fatalf("include should contain only non-empty strings; body=%s", call.prepared.body)
		}
	}
}

func TestPrepareCodexHTTPCallStripsUnsupportedFinalUpstreamFields(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex"}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`),
	}
	rawJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
		"messages":[{"role":"user","content":"hello"}],
		"metadata":{"conversation_id":"conv-1"},
		"response_format":{"type":"json_schema"},
		"functions":[{"name":"legacy_func"}],
		"trace":{"traceparent":"00-test"}
	}`)

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai"),
		"",
		"https://example.com/responses",
		req,
		rawJSON,
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}

	body := call.prepared.body
	for _, field := range []string{"messages", "metadata", "response_format", "functions", "trace"} {
		if gjson.GetBytes(body, field).Exists() {
			t.Fatalf("%s should not reach final Codex upstream body: %s", field, body)
		}
	}
	if got := gjson.GetBytes(body, "input.0.content.0.text").String(); got != "hello" {
		t.Fatalf("input.0.content.0.text = %q, want %q; body=%s", got, "hello", body)
	}
}
