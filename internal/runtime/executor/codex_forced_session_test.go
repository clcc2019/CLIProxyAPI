package executor

import (
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestForcedUpstreamSessionOverridesCallerOwnedCodexSession(t *testing.T) {
	executor := NewCodexExecutor(nil)
	ctx := contextWithGinHeaders(map[string]string{
		codexHeaderSessionID:        "old-session",
		codexHeaderThreadID:         "old-thread",
		codexHeaderTurnMetadata:     `{"session_id":"old-meta-session","thread_id":"old-meta-thread","turn_id":"old-turn"}`,
		"X-Client-Request-Id":       "old-request",
		"Conversation_id":           "old-conversation",
		codexHeaderOfficialThreadID: "old-official-thread",
	})
	ctx = contextWithCodexForcedUpstreamSessionFromOptions(ctx, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ForcedUpstreamSessionMetadataKey: "new-upstream-session",
		},
	})
	req := cliproxyexecutor.Request{
		Model: "gpt-5-codex",
		Payload: []byte(`{
			"model":"gpt-5-codex",
			"prompt_cache_key":"old-cache",
			"previous_response_id":"old-response",
			"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"old-body-session\",\"thread_id\":\"old-body-thread\"}"}
		}`),
	}

	call, err := executor.prepareCodexHTTPCall(ctx, nil, sdktranslator.FromString("openai"), "downstream-exec-session", "https://chatgpt.com/backend-api/codex/responses", req, req.Payload, "token", true)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall error: %v", err)
	}

	headers := call.prepared.httpReq.Header
	if got := headers.Get(codexHeaderSessionID); got != "new-upstream-session" {
		t.Fatalf("%s = %q, want new-upstream-session", codexHeaderSessionID, got)
	}
	if got := headers.Get(codexHeaderThreadID); got != "new-upstream-session" {
		t.Fatalf("%s = %q, want new-upstream-session", codexHeaderThreadID, got)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "new-upstream-session" {
		t.Fatalf("X-Client-Request-Id = %q, want new-upstream-session", got)
	}
	assertCodexTurnMetadataString(t, headers.Get(codexHeaderTurnMetadata), "session_id", "new-upstream-session")
	assertCodexTurnMetadataString(t, headers.Get(codexHeaderTurnMetadata), "thread_id", "new-upstream-session")

	body := call.prepared.body
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "new-upstream-session" {
		t.Fatalf("prompt_cache_key = %q, want new-upstream-session; body=%s", got, body)
	}
	if gjson.GetBytes(body, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id should be removed during forced upstream session failover: %s", body)
	}
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Exists() {
		t.Fatalf("old body turn metadata should be removed during forced upstream session failover: %s", body)
	}
}
