package openai

import (
	"net/http"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

var responseExecutionThreadHeaderKeys = []string{"Thread_id", "thread_id", "Thread-Id", "thread-id", "X-Thread-ID"}
var responseExecutionConversationHeaderKeys = []string{"Conversation_id", "conversation_id", "Conversation-Id", "conversation-id", "X-Conversation-ID"}
var responseExecutionSessionHeaderKeys = []string{"Session_id", "session_id", "Session-Id", "session-id", "X-Session-ID"}
var responseExecutionBodyPaths = []string{
	"client_metadata.x-codex-turn-metadata.thread_id", "metadata.thread_id", "metadata.threadId",
	"thread_id", "threadId", "metadata.conversation_id", "metadata.conversationId",
	"conversation_id", "conversationId", "client_metadata.x-codex-turn-metadata.session_id",
	"metadata.session_id", "metadata.sessionId", "session_id", "sessionId",
	"prompt_cache_key", "metadata.prompt_cache_key",
}

const responseExecutionBodyPathCount = 16

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
	return responsesBodyExecutionSessionID(rawJSON)

}

// responsesBodyExecutionSessionID scans the request object once. Nested
// metadata is parsed only after the top-level scan identifies its container,
// avoiding a full-body scan for each candidate path.
func responsesBodyExecutionSessionID(rawJSON []byte) string {
	if len(rawJSON) == 0 {
		return ""
	}
	var fields [responseExecutionBodyPathCount]gjson.Result
	var found [responseExecutionBodyPathCount]bool
	var clientMetadata, metadata gjson.Result
	jsonText := unsafe.String(unsafe.SliceData(rawJSON), len(rawJSON))
	gjson.Parse(jsonText).ForEach(func(key, value gjson.Result) bool {
		if key.Type != gjson.String {
			return true
		}
		name := key.String()
		if name == "client_metadata" && !clientMetadata.Exists() {
			clientMetadata = value
		}
		if name == "metadata" && !metadata.Exists() {
			metadata = value
		}
		for index, path := range responseExecutionBodyPaths {
			if path == name && !found[index] {
				fields[index], found[index] = value, true
			}
		}
		return true
	})

	turn := clientMetadata.Get("x-codex-turn-metadata")
	if turn.Type == gjson.JSON {
		if value := turnMetadataField(turn, "thread_id"); value != "" {
			return value
		}
	}
	for _, path := range []string{"thread_id", "threadId"} {
		if value := ownedSessionValue(metadata.Get(path).String()); value != "" {
			return value
		}
	}
	for _, index := range []int{3, 4} {
		if value := ownedSessionValue(fields[index].String()); value != "" {
			return value
		}
	}
	for _, path := range []string{"conversation_id", "conversationId"} {
		if value := ownedSessionValue(metadata.Get(path).String()); value != "" {
			return value
		}
	}
	for _, index := range []int{7, 8} {
		if value := ownedSessionValue(fields[index].String()); value != "" {
			return value
		}
	}
	for _, path := range []string{"session_id", "sessionId"} {
		if value := ownedSessionValue(metadata.Get(path).String()); value != "" {
			return value
		}
	}
	for _, index := range []int{12, 13, 14} {
		if value := ownedSessionValue(fields[index].String()); value != "" {
			return value
		}
	}
	if value := ownedSessionValue(metadata.Get("prompt_cache_key").String()); value != "" {
		return value
	}
	if turn.Exists() {
		if turn.Type != gjson.JSON {
			if value := turnMetadataField(turn, "thread_id"); value != "" {
				return value
			}
		}
		if value := turnMetadataField(turn, "session_id"); value != "" {
			return value
		}
	}
	return ""
}

func turnMetadataField(value gjson.Result, field string) string {
	if value.Type == gjson.JSON {
		return ownedSessionValue(value.Get(field).String())
	}
	if raw := strings.TrimSpace(value.String()); raw != "" {
		return ownedSessionValue(gjson.Get(raw, field).String())
	}
	return ""
}

func ownedSessionValue(value string) string {
	return strings.Clone(strings.TrimSpace(value))
}
