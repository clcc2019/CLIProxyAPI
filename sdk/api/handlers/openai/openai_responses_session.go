package openai

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

var responseExecutionThreadHeaderKeys = []string{"Thread_id", "thread_id", "Thread-Id", "thread-id", "X-Thread-ID"}
var responseExecutionConversationHeaderKeys = []string{"Conversation_id", "conversation_id", "Conversation-Id", "conversation-id", "X-Conversation-ID"}
var responseExecutionSessionHeaderKeys = []string{"Session_id", "session_id", "Session-Id", "session-id", "X-Session-ID"}

func responsesExplicitExecutionSessionID(req *http.Request, rawJSON []byte) string {
	if req != nil {
		for _, key := range responseExecutionThreadHeaderKeys {
			if threadID := strings.TrimSpace(req.Header.Get(key)); threadID != "" {
				return threadID
			}
		}
		for _, key := range responseExecutionConversationHeaderKeys {
			if conversationID := strings.TrimSpace(req.Header.Get(key)); conversationID != "" {
				return conversationID
			}
		}
		if raw := strings.TrimSpace(req.Header.Get("X-Codex-Turn-Metadata")); raw != "" {
			if threadID := strings.TrimSpace(gjson.Get(raw, "thread_id").String()); threadID != "" {
				return threadID
			}
			if sessionID := strings.TrimSpace(gjson.Get(raw, "session_id").String()); sessionID != "" {
				return sessionID
			}
		}
		for _, key := range responseExecutionSessionHeaderKeys {
			if sessionID := strings.TrimSpace(req.Header.Get(key)); sessionID != "" {
				return sessionID
			}
		}
	}

	if len(rawJSON) == 0 {
		return ""
	}
	for _, path := range []string{
		"client_metadata.x-codex-turn-metadata.thread_id",
		"metadata.thread_id",
		"metadata.threadId",
		"thread_id",
		"threadId",
		"metadata.conversation_id",
		"metadata.conversationId",
		"conversation_id",
		"conversationId",
		"client_metadata.x-codex-turn-metadata.session_id",
		"metadata.session_id",
		"metadata.sessionId",
		"session_id",
		"sessionId",
		"prompt_cache_key",
		"metadata.prompt_cache_key",
	} {
		if sessionID := strings.TrimSpace(gjson.GetBytes(rawJSON, path).String()); sessionID != "" {
			return sessionID
		}
	}
	if raw := strings.TrimSpace(gjson.GetBytes(rawJSON, "client_metadata.x-codex-turn-metadata").String()); raw != "" {
		if threadID := strings.TrimSpace(gjson.Get(raw, "thread_id").String()); threadID != "" {
			return threadID
		}
		if sessionID := strings.TrimSpace(gjson.Get(raw, "session_id").String()); sessionID != "" {
			return sessionID
		}
	}

	return ""
}
