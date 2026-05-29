package executor

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexDefaultSandboxTag       = "none"
	codexTurnRequestKind         = "turn"
	codexCompactionRequestKind   = "compaction"
	codexPrewarmRequestKind      = "prewarm"
	codexRequestKindMetadataPath = "request_kind"
	codexWindowIDMetadataPath    = "window_id"
)

type codexTurnMetadata struct {
	RequestKind  string `json:"request_kind,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	ThreadID     string `json:"thread_id,omitempty"`
	ThreadSource string `json:"thread_source,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`
	Sandbox      string `json:"sandbox,omitempty"`
	WindowID     string `json:"window_id,omitempty"`
	StartedAtMS  int64  `json:"turn_started_at_unix_ms,omitempty"`
}

type codexTurnMetadataDefaults struct {
	requestKind            string
	sessionID              string
	threadID               string
	threadSource           string
	turnID                 string
	sandbox                string
	windowID               string
	turnStartedAtUnixMilli int64
}

func codexEnsureTurnMetadataHeader(target http.Header, source http.Header, defaults codexTurnMetadataDefaults) {
	if target == nil {
		return
	}
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata); value != "" {
		target.Set(codexHeaderTurnMetadata, codexAugmentTurnMetadataHeader(value, defaults))
		return
	}
	target.Set(codexHeaderTurnMetadata, codexBuildTurnMetadataHeader(
		defaults.requestKind,
		defaults.sessionID,
		defaults.threadID,
		defaults.threadSource,
		defaults.turnID,
		defaults.sandbox,
		defaults.windowID,
		defaults.turnStartedAtUnixMilli,
	))
}

func codexEnsureCompactTurnMetadataHeader(target http.Header, source http.Header, defaults codexTurnMetadataDefaults) {
	if target == nil {
		return
	}
	defaults.requestKind = codexCompactionRequestKind
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata); value != "" {
		updated := codexAugmentTurnMetadataHeader(value, defaults)
		updated = codexSetTurnMetadataString(updated, codexRequestKindMetadataPath, defaults.requestKind, true)
		updated = codexSetTurnMetadataString(updated, codexWindowIDMetadataPath, defaults.windowID, true)
		target.Set(codexHeaderTurnMetadata, updated)
		return
	}
	target.Set(codexHeaderTurnMetadata, codexBuildTurnMetadataHeader(
		defaults.requestKind,
		defaults.sessionID,
		defaults.threadID,
		defaults.threadSource,
		defaults.turnID,
		defaults.sandbox,
		defaults.windowID,
		defaults.turnStartedAtUnixMilli,
	))
}

func codexDefaultTurnMetadataHeader(sessionID string) string {
	return codexBuildTurnMetadataHeader(
		codexTurnRequestKind,
		sessionID,
		sessionID,
		"",
		uuid.NewString(),
		codexDefaultSandboxTag,
		"",
		0,
	)
}

func codexBuildTurnMetadataHeader(requestKind string, sessionID string, threadID string, threadSource string, turnID string, sandbox string, windowID string, turnStartedAtUnixMilli int64) string {
	requestKind = strings.TrimSpace(requestKind)
	sessionID = strings.TrimSpace(sessionID)
	threadID = strings.TrimSpace(threadID)
	threadSource = strings.TrimSpace(threadSource)
	turnID = strings.TrimSpace(turnID)
	sandbox = strings.TrimSpace(sandbox)
	windowID = strings.TrimSpace(windowID)
	if turnStartedAtUnixMilli <= 0 {
		turnStartedAtUnixMilli = time.Now().UnixMilli()
	}

	var builder strings.Builder
	builder.Grow(len(requestKind) + len(sessionID) + len(threadID) + len(threadSource) + len(turnID) + len(sandbox) + len(windowID) + 128)
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

	appendQuotedJSONField("request_kind", requestKind)
	appendQuotedJSONField("session_id", sessionID)
	appendQuotedJSONField("thread_id", threadID)
	appendQuotedJSONField("thread_source", threadSource)
	appendQuotedJSONField("turn_id", turnID)
	appendQuotedJSONField("sandbox", sandbox)
	appendQuotedJSONField("window_id", windowID)
	appendInt64JSONField("turn_started_at_unix_ms", turnStartedAtUnixMilli)
	builder.WriteByte('}')
	return builder.String()
}

func codexAugmentTurnMetadataHeader(raw string, defaults codexTurnMetadataDefaults) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return codexBuildTurnMetadataHeader(
			defaults.requestKind,
			defaults.sessionID,
			defaults.threadID,
			defaults.threadSource,
			defaults.turnID,
			defaults.sandbox,
			defaults.windowID,
			defaults.turnStartedAtUnixMilli,
		)
	}
	if defaults.turnStartedAtUnixMilli <= 0 {
		defaults.turnStartedAtUnixMilli = time.Now().UnixMilli()
	}
	updated := raw
	setStringIfMissing := func(path string, value string) {
		value = strings.TrimSpace(value)
		if value == "" || gjson.Get(updated, path).Exists() {
			return
		}
		if next, err := sjson.Set(updated, path, value); err == nil {
			updated = next
		}
	}
	setInt64IfMissing := func(path string, value int64) {
		if value <= 0 || gjson.Get(updated, path).Exists() {
			return
		}
		if next, err := sjson.Set(updated, path, value); err == nil {
			updated = next
		}
	}

	setStringIfMissing(codexRequestKindMetadataPath, defaults.requestKind)
	setStringIfMissing("session_id", defaults.sessionID)
	setStringIfMissing("thread_id", defaults.threadID)
	setStringIfMissing("thread_source", defaults.threadSource)
	setStringIfMissing("turn_id", defaults.turnID)
	setStringIfMissing("sandbox", defaults.sandbox)
	setStringIfMissing(codexWindowIDMetadataPath, defaults.windowID)
	setInt64IfMissing("turn_started_at_unix_ms", defaults.turnStartedAtUnixMilli)
	return updated
}

func codexSetTurnMetadataString(raw string, path string, value string, overwrite bool) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.TrimSpace(path) == "" || !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return raw
	}
	if !overwrite && gjson.Get(raw, path).Exists() {
		return raw
	}
	if updated, err := sjson.Set(raw, path, value); err == nil {
		return updated
	}
	return raw
}

func codexTurnMetadataSessionID(target http.Header, source http.Header) string {
	raw := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata)
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, "session_id").String())
}
