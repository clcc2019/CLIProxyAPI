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
	codexMemoryRequestKind       = "memory"
	codexRequestKindMetadataPath = "request_kind"
	codexWindowIDMetadataPath    = "window_id"
	codexCompactionMetadataPath  = "compaction"

	codexDefaultCompactionImplementation = "responses_compact"
	codexDefaultCompactionStrategy       = "memento"
)

type codexTurnMetadata struct {
	RequestKind        string `json:"request_kind,omitempty"`
	SessionID          string `json:"session_id,omitempty"`
	ThreadID           string `json:"thread_id,omitempty"`
	ForkedFromThreadID string `json:"forked_from_thread_id,omitempty"`
	ParentThreadID     string `json:"parent_thread_id,omitempty"`
	SubagentKind       string `json:"subagent_kind,omitempty"`
	ThreadSource       string `json:"thread_source,omitempty"`
	TurnID             string `json:"turn_id,omitempty"`
	Sandbox            string `json:"sandbox,omitempty"`
	WindowID           string `json:"window_id,omitempty"`
	StartedAtMS        int64  `json:"turn_started_at_unix_ms,omitempty"`
}

type codexTurnMetadataDefaults struct {
	requestKind            string
	sessionID              string
	threadID               string
	forkedFromThreadID     string
	parentThreadID         string
	subagentKind           string
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
	codexFillTurnMetadataLineageDefaults(target, source, &defaults)
	codexFillTurnMetadataRequestKindDefault(target, source, &defaults)
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata); value != "" {
		target.Set(codexHeaderTurnMetadata, codexAugmentTurnMetadataHeader(value, defaults))
		return
	}
	target.Set(codexHeaderTurnMetadata, codexBuildTurnMetadataHeader(
		defaults.requestKind,
		defaults.sessionID,
		defaults.threadID,
		defaults.forkedFromThreadID,
		defaults.parentThreadID,
		defaults.subagentKind,
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
	codexFillTurnMetadataLineageDefaults(target, source, &defaults)
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata); value != "" {
		updated := codexAugmentTurnMetadataHeader(value, defaults)
		updated = codexSetTurnMetadataString(updated, codexRequestKindMetadataPath, defaults.requestKind, true)
		updated = codexSetTurnMetadataString(updated, codexWindowIDMetadataPath, defaults.windowID, true)
		updated = codexAugmentCompactionMetadata(updated, target, source)
		target.Set(codexHeaderTurnMetadata, updated)
		return
	}
	updated := codexBuildTurnMetadataHeader(
		defaults.requestKind,
		defaults.sessionID,
		defaults.threadID,
		defaults.forkedFromThreadID,
		defaults.parentThreadID,
		defaults.subagentKind,
		defaults.threadSource,
		defaults.turnID,
		defaults.sandbox,
		defaults.windowID,
		defaults.turnStartedAtUnixMilli,
	)
	updated = codexAugmentCompactionMetadata(updated, target, source)
	target.Set(codexHeaderTurnMetadata, updated)
}

func codexFillTurnMetadataLineageDefaults(target http.Header, source http.Header, defaults *codexTurnMetadataDefaults) {
	if defaults == nil {
		return
	}
	if defaults.parentThreadID == "" {
		defaults.parentThreadID = firstNonEmptyHeaderValue(target, source, codexHeaderParentThreadID)
	}
	if defaults.subagentKind == "" {
		defaults.subagentKind = firstNonEmptyHeaderValue(target, source, "X-OpenAI-Subagent")
	}
}

func codexFillTurnMetadataRequestKindDefault(target http.Header, source http.Header, defaults *codexTurnMetadataDefaults) {
	if defaults == nil || strings.TrimSpace(defaults.requestKind) != codexTurnRequestKind {
		return
	}
	if codexHeaderBoolValue(target, source, codexHeaderMemgenRequest) {
		defaults.requestKind = codexMemoryRequestKind
	}
}

func codexHeaderBoolValue(target http.Header, source http.Header, key string) bool {
	value := firstNonEmptyHeaderValue(target, source, key)
	if parsed, ok := codexParseBoolLike(value); ok {
		return parsed
	}
	return false
}

func codexDefaultTurnMetadataHeader(sessionID string) string {
	return codexBuildTurnMetadataHeader(
		codexTurnRequestKind,
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
	)
}

func codexBuildTurnMetadataHeader(requestKind string, sessionID string, threadID string, forkedFromThreadID string, parentThreadID string, subagentKind string, threadSource string, turnID string, sandbox string, windowID string, turnStartedAtUnixMilli int64) string {
	requestKind = strings.TrimSpace(requestKind)
	sessionID = strings.TrimSpace(sessionID)
	threadID = strings.TrimSpace(threadID)
	forkedFromThreadID = strings.TrimSpace(forkedFromThreadID)
	parentThreadID = strings.TrimSpace(parentThreadID)
	subagentKind = strings.TrimSpace(subagentKind)
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
	appendQuotedJSONField("forked_from_thread_id", forkedFromThreadID)
	appendQuotedJSONField("parent_thread_id", parentThreadID)
	appendQuotedJSONField("subagent_kind", subagentKind)
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
			defaults.forkedFromThreadID,
			defaults.parentThreadID,
			defaults.subagentKind,
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
	setStringIfMissing("forked_from_thread_id", defaults.forkedFromThreadID)
	setStringIfMissing("parent_thread_id", defaults.parentThreadID)
	setStringIfMissing("subagent_kind", defaults.subagentKind)
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

func codexAugmentCompactionMetadata(raw string, target http.Header, source http.Header) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return raw
	}
	updated := raw
	compaction := gjson.Get(updated, codexCompactionMetadataPath)
	if !compaction.Exists() || !compaction.IsObject() {
		next, err := sjson.SetRaw(updated, codexCompactionMetadataPath, `{}`)
		if err != nil {
			return raw
		}
		updated = next
	}
	setCompactionFieldIfMissing := func(field string, value string) {
		value = codexNormalizeCompactionMetadataValue(value)
		if value == "" || gjson.Get(updated, codexCompactionMetadataPath+"."+field).Exists() {
			return
		}
		if next, err := sjson.Set(updated, codexCompactionMetadataPath+"."+field, value); err == nil {
			updated = next
		}
	}

	setCompactionFieldIfMissing("trigger", firstNonEmptyHeaderValue(target, source, codexHeaderCompactionTrigger))
	setCompactionFieldIfMissing("reason", firstNonEmptyHeaderValue(target, source, codexHeaderCompactionReason))
	setCompactionFieldIfMissing("implementation", firstNonEmptyHeaderValue(target, source, codexHeaderCompactionImpl))
	setCompactionFieldIfMissing("implementation", codexDefaultCompactionImplementation)
	setCompactionFieldIfMissing("phase", firstNonEmptyHeaderValue(target, source, codexHeaderCompactionPhase))
	setCompactionFieldIfMissing("strategy", firstNonEmptyHeaderValue(target, source, codexHeaderCompactionStrategy))
	setCompactionFieldIfMissing("strategy", codexDefaultCompactionStrategy)
	return updated
}

func codexNormalizeCompactionMetadataValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "-", "_")
	value = strings.ReplaceAll(value, " ", "_")
	for strings.Contains(value, "__") {
		value = strings.ReplaceAll(value, "__", "_")
	}
	return strings.Trim(value, "_")
}

func codexTurnMetadataSessionID(target http.Header, source http.Header) string {
	raw := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata)
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, "session_id").String())
}
