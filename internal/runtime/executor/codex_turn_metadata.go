package executor

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

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

var codexResponsesAPIClientMetadataReservedKeys = map[string]struct{}{
	"session_id":                 {},
	"thread_id":                  {},
	"turn_id":                    {},
	"turn_started_at_unix_ms":    {},
	"forked_from_thread_id":      {},
	"parent_thread_id":           {},
	"subagent_kind":              {},
	codexRequestKindMetadataPath: {},
	codexCompactionMetadataPath:  {},
	codexWindowIDMetadataPath:    {},
}

var codexResponsesAPIClientMetadataTransportKeys = map[string]struct{}{
	codexClientMetadataInstallationID:         {},
	codexClientMetadataWindowID:               {},
	codexClientMetadataParentThreadID:         {},
	codexClientMetadataSubagent:               {},
	codexClientMetadataTurnMetadata:           {},
	codexWSClientMetadataTraceparent:          {},
	codexWSClientMetadataTracestate:           {},
	codexClientMetadataWSStreamRequestStartMS: {},
}

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
		defaults.subagentKind = firstNonEmptyHeaderValue(target, source, codexWireHeaderOpenAISubagent)
	}
}

func codexFillTurnMetadataRequestKindDefault(target http.Header, source http.Header, defaults *codexTurnMetadataDefaults) {
	if defaults == nil || strings.TrimSpace(defaults.requestKind) != codexTurnRequestKind {
		return
	}
	if codexHeaderBoolValue(target, source, codexWireHeaderMemgenRequest) {
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
		codexWriteJSONString(&builder, value)
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
		var intBuf [20]byte
		_, _ = builder.Write(strconv.AppendInt(intBuf[:0], value, 10))
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

func codexWriteJSONString(builder *strings.Builder, value string) {
	if builder == nil {
		return
	}
	const hex = "0123456789abcdef"
	builder.WriteByte('"')
	start := 0
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch c {
		case '\\', '"':
			builder.WriteString(value[start:i])
			builder.WriteByte('\\')
			builder.WriteByte(c)
			start = i + 1
		case '\b':
			builder.WriteString(value[start:i])
			builder.WriteString(`\b`)
			start = i + 1
		case '\f':
			builder.WriteString(value[start:i])
			builder.WriteString(`\f`)
			start = i + 1
		case '\n':
			builder.WriteString(value[start:i])
			builder.WriteString(`\n`)
			start = i + 1
		case '\r':
			builder.WriteString(value[start:i])
			builder.WriteString(`\r`)
			start = i + 1
		case '\t':
			builder.WriteString(value[start:i])
			builder.WriteString(`\t`)
			start = i + 1
		default:
			if c < 0x20 {
				builder.WriteString(value[start:i])
				builder.WriteString(`\u00`)
				builder.WriteByte(hex[c>>4])
				builder.WriteByte(hex[c&0x0f])
				start = i + 1
			}
		}
	}
	builder.WriteString(value[start:])
	builder.WriteByte('"')
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

func codexResponsesAPIClientMetadataFromBody(body []byte) map[string]string {
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return nil
	}
	var entries map[string]string
	metadata.ForEach(func(key, value gjson.Result) bool {
		keyString := strings.TrimSpace(key.String())
		if keyString == "" || value.Type != gjson.String || codexShouldSkipResponsesAPIClientMetadataForTurnMetadata(keyString) {
			return true
		}
		if entries == nil {
			entries = make(map[string]string)
		}
		entries[keyString] = value.String()
		return true
	})
	return entries
}

func codexMergeResponsesAPIClientMetadataIntoTurnMetadataHeader(headers http.Header, responsesAPIClientMetadata map[string]string) {
	if headers == nil || len(responsesAPIClientMetadata) == 0 {
		return
	}
	raw := strings.TrimSpace(headers.Get(codexHeaderTurnMetadata))
	if raw == "" || !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return
	}
	changed := false
	for key, value := range responsesAPIClientMetadata {
		key = strings.TrimSpace(key)
		if key == "" || codexShouldSkipResponsesAPIClientMetadataForTurnMetadata(key) {
			continue
		}
		if _, exists := metadata[key]; exists {
			continue
		}
		metadata[key] = value
		changed = true
	}
	if !changed {
		return
	}
	if updated, err := json.Marshal(metadata); err == nil && len(updated) > 0 {
		updated = codexEscapeNonASCIIJSONBytes(updated)
		headers.Set(codexHeaderTurnMetadata, string(updated))
	}
}

func codexShouldSkipResponsesAPIClientMetadataForTurnMetadata(key string) bool {
	if _, ok := codexResponsesAPIClientMetadataReservedKeys[key]; ok {
		return true
	}
	lowerKey := strings.ToLower(key)
	if _, ok := codexResponsesAPIClientMetadataTransportKeys[lowerKey]; ok {
		return true
	}
	return false
}

func codexEscapeNonASCIIJSONBytes(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var builder strings.Builder
	changed := false
	for _, r := range string(raw) {
		if r < 0x80 {
			builder.WriteRune(r)
			continue
		}
		changed = true
		if r <= 0xffff {
			builder.WriteString(`\u`)
			builder.WriteString(codexHex4(uint16(r)))
			continue
		}
		r1, r2 := utf16.EncodeRune(r)
		builder.WriteString(`\u`)
		builder.WriteString(codexHex4(uint16(r1)))
		builder.WriteString(`\u`)
		builder.WriteString(codexHex4(uint16(r2)))
	}
	if !changed {
		return raw
	}
	return []byte(builder.String())
}

func codexHex4(value uint16) string {
	const digits = "0123456789abcdef"
	return string([]byte{
		digits[value>>12&0xf],
		digits[value>>8&0xf],
		digits[value>>4&0xf],
		digits[value&0xf],
	})
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
