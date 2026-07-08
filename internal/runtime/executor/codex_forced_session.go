package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/sjson"
)

type codexForcedUpstreamSessionContextKey struct{}

func contextWithCodexForcedUpstreamSessionFromOptions(ctx context.Context, opts cliproxyexecutor.Options) context.Context {
	sessionID := forcedUpstreamSessionIDFromOptions(opts)
	if sessionID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexForcedUpstreamSessionContextKey{}, sessionID)
}

func forcedUpstreamSessionIDFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ForcedUpstreamSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func codexForcedUpstreamSessionID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw, _ := ctx.Value(codexForcedUpstreamSessionContextKey{}).(string)
	return strings.TrimSpace(raw)
}

func codexSanitizeForcedUpstreamSessionBody(ctx context.Context, body []byte) []byte {
	if codexForcedUpstreamSessionID(ctx) == "" || len(body) == 0 {
		return body
	}
	for _, path := range []string{
		"previous_response_id",
		"prompt_cache_key",
		"metadata.prompt_cache_key",
		"conversation_id",
		"conversationId",
		"metadata.conversation_id",
		"metadata.conversationId",
		"session_id",
		"sessionId",
		"metadata.session_id",
		"metadata.sessionId",
		"thread_id",
		"threadId",
		"metadata.thread_id",
		"metadata.threadId",
		"client_metadata.x-codex-turn-metadata",
	} {
		if updated, err := sjson.DeleteBytes(body, path); err == nil {
			body = updated
		}
	}
	return body
}

func codexApplyForcedUpstreamSessionHeaders(ctx context.Context, headers http.Header) {
	sessionID := codexForcedUpstreamSessionID(ctx)
	if sessionID == "" || headers == nil {
		return
	}
	headers.Set(codexHeaderSessionID, sessionID)
	headers.Set(codexHeaderOfficialSessionID, sessionID)
	headers.Set(codexHeaderThreadID, sessionID)
	headers.Set(codexHeaderOfficialThreadID, sessionID)
	headers.Set("X-Client-Request-Id", sessionID)
	headers.Set(codexHeaderTurnMetadata, codexBuildTurnMetadataHeader(
		"",
		sessionID,
		sessionID,
		"",
		"",
		"",
		"",
		uuid.NewString(),
		codexDefaultSandboxTag,
		"",
		0,
	))
	headers.Del("Conversation_id")
}
