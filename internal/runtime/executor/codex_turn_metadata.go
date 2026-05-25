package executor

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	codexDefaultSandboxTag = "none"
)

type codexTurnMetadata struct {
	SessionID    string `json:"session_id,omitempty"`
	ThreadID     string `json:"thread_id,omitempty"`
	ThreadSource string `json:"thread_source,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`
	Sandbox      string `json:"sandbox,omitempty"`
	StartedAtMS  int64  `json:"turn_started_at_unix_ms,omitempty"`
}

type codexTurnMetadataDefaults struct {
	sessionID              string
	threadID               string
	threadSource           string
	turnID                 string
	sandbox                string
	turnStartedAtUnixMilli int64
}

func codexEnsureTurnMetadataHeader(target http.Header, source http.Header, defaults codexTurnMetadataDefaults) {
	if target == nil {
		return
	}
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata); value != "" {
		target.Set(codexHeaderTurnMetadata, value)
		return
	}
	target.Set(codexHeaderTurnMetadata, codexBuildTurnMetadataHeader(
		defaults.sessionID,
		defaults.threadID,
		defaults.threadSource,
		defaults.turnID,
		defaults.sandbox,
		defaults.turnStartedAtUnixMilli,
	))
}

func codexDefaultTurnMetadataHeader(sessionID string) string {
	return codexBuildTurnMetadataHeader(
		sessionID,
		sessionID,
		"",
		uuid.NewString(),
		codexDefaultSandboxTag,
		0,
	)
}

func codexBuildTurnMetadataHeader(sessionID string, threadID string, threadSource string, turnID string, sandbox string, turnStartedAtUnixMilli int64) string {
	sessionID = strings.TrimSpace(sessionID)
	threadID = strings.TrimSpace(threadID)
	threadSource = strings.TrimSpace(threadSource)
	turnID = strings.TrimSpace(turnID)
	sandbox = strings.TrimSpace(sandbox)
	if turnStartedAtUnixMilli <= 0 {
		turnStartedAtUnixMilli = time.Now().UnixMilli()
	}

	var builder strings.Builder
	builder.Grow(len(sessionID) + len(threadID) + len(threadSource) + len(turnID) + len(sandbox) + 96)
	builder.WriteByte('{')
	first := true
	appendQuotedJSONField := func(name string, value string) {
		if value == "" {
			return
		}
		if !first {
			builder.WriteByte(',')
		}
		first = false
		builder.WriteByte('"')
		builder.WriteString(name)
		builder.WriteString(`":`)
		builder.WriteString(strconv.Quote(value))
	}
	appendInt64JSONField := func(name string, value int64) {
		if value <= 0 {
			return
		}
		if !first {
			builder.WriteByte(',')
		}
		first = false
		builder.WriteByte('"')
		builder.WriteString(name)
		builder.WriteString(`":`)
		builder.WriteString(strconv.FormatInt(value, 10))
	}

	appendQuotedJSONField("session_id", sessionID)
	appendQuotedJSONField("thread_id", threadID)
	appendQuotedJSONField("thread_source", threadSource)
	appendQuotedJSONField("turn_id", turnID)
	appendQuotedJSONField("sandbox", sandbox)
	appendInt64JSONField("turn_started_at_unix_ms", turnStartedAtUnixMilli)
	builder.WriteByte('}')
	return builder.String()
}

func codexTurnMetadataSessionID(target http.Header, source http.Header) string {
	raw := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata)
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, "session_id").String())
}
