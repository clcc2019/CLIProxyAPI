package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexHeaderInstallationID    = "X-Codex-Installation-Id"
	codexHeaderWindowID          = "X-Codex-Window-Id"
	codexHeaderParentThreadID    = "X-Codex-Parent-Thread-Id"
	codexHeaderMemgenRequest     = "X-OpenAI-Memgen-Request"
	codexWireHeaderMemgenRequest = "X-Openai-Memgen-Request"

	codexClientMetadataInstallationID  = "x-codex-installation-id"
	codexClientMetadataSessionID       = "session_id"
	codexClientMetadataThreadID        = "thread_id"
	codexClientMetadataTurnID          = "turn_id"
	codexClientMetadataWindowID        = "x-codex-window-id"
	codexClientMetadataParentThreadID  = "x-codex-parent-thread-id"
	codexClientMetadataSubagent        = "x-openai-subagent"
	codexClientMetadataTurnMetadata    = "x-codex-turn-metadata"
	codexWSClientMetadataTraceparent   = "ws_request_header_traceparent"
	codexWSClientMetadataTracestate    = "ws_request_header_tracestate"
	codexWSClientMetadataResponsesLite = "ws_request_header_x_openai_internal_codex_responses_lite"
)

var (
	codexInstallationIDOnce      sync.Once
	codexInstallationID          string
	codexClientMetadataJSONField = []byte(`"client_metadata"`)
)

// codexGinHeadersCtxKey is the typed context key under which a resolved
// http.Header (from the inbound gin request) is cached for the lifetime of the
// per-request hot path. Using a typed zero-size struct avoids collisions with
// the untyped "gin" string key that the gin middleware itself uses.
type codexGinHeadersCtxKey struct{}

// contextWithCachedCodexGinHeaders returns ctx annotated with a cached copy of
// the gin request headers. The cache is a no-op when ctx already carries one
// or when no gin request is in scope, so it is safe to call at every prepare
// entry point without checking first.
func contextWithCachedCodexGinHeaders(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	if _, ok := ctx.Value(codexGinHeadersCtxKey{}).(http.Header); ok {
		return ctx
	}
	headers := codexGinHeadersFromContextUncached(ctx)
	if headers == nil {
		return ctx
	}
	return context.WithValue(ctx, codexGinHeadersCtxKey{}, headers)
}

func codexGinHeadersFromContext(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	if cached, ok := ctx.Value(codexGinHeadersCtxKey{}).(http.Header); ok {
		return cached
	}
	return codexGinHeadersFromContextUncached(ctx)
}

func codexGinHeadersFromContextUncached(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return nil
	}
	return ginCtx.Request.Header
}

func codexApplyHTTPClientMetadata(body []byte, req *http.Request, auth *cliproxyauth.Auth, cfg *config.Config) []byte {
	if req == nil {
		return body
	}
	return codexApplyHTTPClientMetadataWithSource(body, req.Header, codexGinHeadersFromContext(req.Context()), auth, cfg)
}

func codexApplyHTTPClientMetadataWithSource(body []byte, target http.Header, source http.Header, auth *cliproxyauth.Auth, cfg *config.Config) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	var entries [11]codexClientMetadataEntry
	metadataEntries := codexCompactClientMetadataEntries(codexResponsesClientMetadataEntries(entries[:0], target, source, auth, cfg, false, ""))
	return codexSetClientMetadataNormalized(
		body,
		metadataEntries,
		true,
	)
}

func codexApplyHTTPClientMetadataWithSourceAndPromptCacheKey(body []byte, target http.Header, source http.Header, auth *cliproxyauth.Auth, cfg *config.Config, promptCacheKey string) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	var entries [11]codexClientMetadataEntry
	metadataEntries := codexCompactClientMetadataEntries(codexResponsesClientMetadataEntries(entries[:0], target, source, auth, cfg, false, ""))
	return codexSetClientMetadataAndPromptCacheKeyNormalized(body, metadataEntries, promptCacheKey)
}

func codexApplyWebsocketClientMetadata(ctx context.Context, body []byte, headers http.Header, auth *cliproxyauth.Auth, cfg *config.Config) []byte {
	return codexApplyWebsocketClientMetadataWithStreamStartMS(ctx, body, headers, auth, cfg, "")
}

func codexApplyWebsocketClientMetadataWithStreamStartMS(ctx context.Context, body []byte, headers http.Header, auth *cliproxyauth.Auth, cfg *config.Config, streamStartMS string) []byte {
	return codexApplyWebsocketClientMetadataWithOptions(ctx, body, headers, auth, cfg, streamStartMS, false)
}

func codexApplyWebsocketClientMetadataWithResponseCreateType(ctx context.Context, body []byte, headers http.Header, auth *cliproxyauth.Auth, cfg *config.Config, streamStartMS string) []byte {
	return codexApplyWebsocketClientMetadataWithOptions(ctx, body, headers, auth, cfg, streamStartMS, true)
}

func codexApplyWebsocketClientMetadataWithOptions(ctx context.Context, body []byte, headers http.Header, auth *cliproxyauth.Auth, cfg *config.Config, streamStartMS string, appendResponseCreateType bool) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}

	// codex-rs carries websocket trace context through client_metadata, not a
	// top-level trace field. Remove it before appending metadata so the fast path
	// scans and copies the smaller original body.
	if bytes.Contains(body, []byte(`"trace"`)) && codexGJSONGetImmutableBytes(body, "trace").Exists() {
		if updated, err := sjson.DeleteBytes(body, "trace"); err == nil {
			body = updated
		}
	}

	source := codexGinHeadersFromContext(ctx)
	var entries [11]codexClientMetadataEntry
	metadataEntries := codexCompactClientMetadataEntries(codexResponsesClientMetadataEntries(entries[:0], headers, source, auth, cfg, true, streamStartMS))
	if appendResponseCreateType {
		body = codexSetClientMetadataAndResponseCreateTypeNormalized(body, metadataEntries)
	} else {
		body = codexSetClientMetadataNormalized(body, metadataEntries, true)
	}
	return body
}

func codexResponsesClientMetadataEntries(dst []codexClientMetadataEntry, target http.Header, source http.Header, auth *cliproxyauth.Auth, cfg *config.Config, websocket bool, streamStartMS string) []codexClientMetadataEntry {
	turnMetadata := firstNonEmptyHeaderValue(target, source, codexHeaderTurnMetadata)
	dst = append(dst,
		codexClientMetadataEntry{key: codexClientMetadataInstallationID, value: codexResolvedInstallationID(target, source, auth, cfg)},
		codexClientMetadataEntry{key: codexClientMetadataSessionID, value: codexClientMetadataSessionIDValue(target, source, turnMetadata)},
		codexClientMetadataEntry{key: codexClientMetadataThreadID, value: codexClientMetadataThreadIDValue(target, source, turnMetadata)},
		codexClientMetadataEntry{key: codexClientMetadataTurnID, value: codexClientMetadataTurnIDValue(turnMetadata)},
		codexClientMetadataEntry{key: codexClientMetadataWindowID, value: codexClientMetadataWindowIDValue(target, source, turnMetadata)},
		codexClientMetadataEntry{key: codexClientMetadataSubagent, value: firstNonEmptyHeaderValue(target, source, codexWireHeaderOpenAISubagent)},
		codexClientMetadataEntry{key: codexClientMetadataParentThreadID, value: firstNonEmptyHeaderValue(target, source, codexHeaderParentThreadID)},
		codexClientMetadataEntry{key: codexClientMetadataTurnMetadata, value: turnMetadata},
	)
	if websocket {
		dst = append(dst,
			codexClientMetadataEntry{key: codexWSClientMetadataTraceparent, value: firstNonEmptyHeaderValue(target, source, "Traceparent")},
			codexClientMetadataEntry{key: codexWSClientMetadataTracestate, value: firstNonEmptyHeaderValue(target, source, "Tracestate")},
			codexClientMetadataEntry{key: codexClientMetadataWSStreamRequestStartMS, value: streamStartMS},
			codexClientMetadataEntry{key: codexWSClientMetadataResponsesLite, value: firstNonEmptyHeaderValue(target, source, codexWireHeaderOpenAIInternalCodexResponsesLite)},
		)
	}
	return dst
}

func codexClientMetadataSessionIDValue(target http.Header, source http.Header, turnMetadata string) string {
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderSessionID); value != "" {
		return value
	}
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderOfficialSessionID); value != "" {
		return value
	}
	if value := firstNonEmptyHeaderValue(target, source, "X-Session-ID"); value != "" {
		return value
	}
	if turnMetadata == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(turnMetadata, "session_id").String())
}

func codexClientMetadataThreadIDValue(target http.Header, source http.Header, turnMetadata string) string {
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderThreadID); value != "" {
		return value
	}
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderOfficialThreadID); value != "" {
		return value
	}
	if value := firstNonEmptyHeaderValue(target, source, "X-Thread-ID"); value != "" {
		return value
	}
	if turnMetadata == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(turnMetadata, "thread_id").String())
}

func codexClientMetadataTurnIDValue(turnMetadata string) string {
	if turnMetadata == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(turnMetadata, "turn_id").String())
}

func codexClientMetadataWindowIDValue(target http.Header, source http.Header, turnMetadata string) string {
	if value := firstNonEmptyHeaderValue(target, source, codexHeaderWindowID); value != "" {
		return value
	}
	if turnMetadata == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(turnMetadata, codexWindowIDMetadataPath).String())
}

func codexEnsureResponsesIdentityHeaders(target http.Header, source http.Header) {
	if target == nil {
		return
	}
	ensureHeaderWithPriority(target, source, codexHeaderParentThreadID, "", "")
	ensureHeaderWithPriority(target, source, codexWireHeaderMemgenRequest, "", "")
	ensureHeaderWithPriority(target, source, codexHeaderWindowID, "", "")
	if trimHeaderValue(target, codexHeaderWindowID) == "" {
		windowKey := firstNonEmptyHeaderValue(target, source, codexHeaderThreadID)
		if windowKey == "" {
			windowKey = trimHeaderValue(target, codexHeaderSessionID)
		}
		if windowKey != "" {
			if windowID := codexCurrentWindowID(windowKey); windowID != "" {
				codexSetSingleHeaderValue(target, codexHeaderWindowID, windowID)
			}
		}
	}
}

func codexResetRequestBody(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	req.Body = newCodexRequestBody(body)
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return newCodexRequestBody(body), nil
	}
}

type codexRequestBody struct {
	bytes.Reader
}

func newCodexRequestBody(body []byte) *codexRequestBody {
	reader := &codexRequestBody{}
	reader.Reset(body)
	return reader
}

func (*codexRequestBody) Close() error { return nil }

type codexClientMetadataEntry struct {
	key   string
	value string
}

func codexSetClientMetadataString(body []byte, key string, value string, overwrite bool) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	return codexSetClientMetadata(body, []codexClientMetadataEntry{{key: key, value: value}}, overwrite)
}

func codexSetClientMetadata(body []byte, entries []codexClientMetadataEntry, overwrite bool) []byte {
	entries = codexNormalizeClientMetadataEntries(entries)
	return codexSetClientMetadataNormalized(body, entries, overwrite)
}

func codexSetClientMetadataNormalized(body []byte, entries []codexClientMetadataEntry, overwrite bool) []byte {
	if len(entries) == 0 {
		return body
	}
	if overwrite && !codexBodyMayContainClientMetadata(body) {
		if updated, ok := codexAppendTopLevelClientMetadataObject(body, entries); ok {
			return updated
		}
	}
	metadata := codexGJSONGetImmutableBytes(body, "client_metadata")
	if overwrite && (!metadata.Exists() || metadata.Type == gjson.Null || !metadata.IsObject()) {
		if !metadata.Exists() {
			if updated, ok := codexAppendTopLevelClientMetadataObject(body, entries); ok {
				return updated
			}
		}
		if metadataBody, ok := codexBuildClientMetadataObject(entries); ok {
			if updated, ok := codexReplaceClientMetadataRaw(body, metadata, metadataBody); ok {
				return updated
			}
			if updated, errSet := sjson.SetRawBytes(body, "client_metadata", metadataBody); errSet == nil {
				return updated
			}
		}
	}
	if overwrite && metadata.IsObject() {
		if updated, changed, ok := codexReplaceMergedClientMetadataObject(body, metadata, entries); ok {
			if !changed {
				return body
			}
			return updated
		}
		if metadataBody, ok := codexBuildMergedClientMetadataObject(metadata, entries); ok {
			if updated, ok := codexReplaceClientMetadataRaw(body, metadata, metadataBody); ok {
				return updated
			}
			if updated, errSet := sjson.SetRawBytes(body, "client_metadata", metadataBody); errSet == nil {
				return updated
			}
		}
	}

	metadataBody, existingKeys, changed := codexClientMetadataStringMapRaw(metadata, !overwrite)
	// existingKeys captures the keys already present in the existing metadata
	// object, so the loop below can skip them without parsing metadataBody
	// once per entry. Only populated when we actually need to respect existing
	// values (overwrite == false), otherwise existence checks are unnecessary.
	for _, entry := range entries {
		if !overwrite && existingKeys != nil {
			if _, ok := existingKeys[entry.key]; ok {
				continue
			}
		}
		updated, errSet := sjson.SetBytes(metadataBody, entry.key, entry.value)
		if errSet != nil {
			continue
		}
		metadataBody = updated
		if existingKeys != nil {
			existingKeys[entry.key] = struct{}{}
		}
		changed = true
	}
	if !changed {
		return body
	}
	if !metadata.Exists() {
		if updated, ok := codexAppendTopLevelRawField(body, "client_metadata", metadataBody); ok {
			return updated
		}
	}
	updated, errSet := sjson.SetRawBytes(body, "client_metadata", metadataBody)
	if errSet != nil {
		return body
	}
	return updated
}

// codexBodyMayContainClientMetadata cheaply proves absence for the common
// unescaped-key case. A backslash keeps the full gjson lookup enabled because
// JSON object keys such as "client\u005fmetadata" are semantically equivalent.
func codexBodyMayContainClientMetadata(body []byte) bool {
	return bytes.Contains(body, codexClientMetadataJSONField) || bytes.IndexByte(body, '\\') >= 0
}

func codexSetClientMetadataAndResponseCreateType(body []byte, entries []codexClientMetadataEntry) []byte {
	entries = codexNormalizeClientMetadataEntries(entries)
	return codexSetClientMetadataAndResponseCreateTypeNormalized(body, entries)
}

func codexSetClientMetadataAndResponseCreateTypeNormalized(body []byte, entries []codexClientMetadataEntry) []byte {
	metadata := codexGJSONGetImmutableBytes(body, "client_metadata")
	requestType := codexGJSONGetImmutableBytes(body, "type")
	if !metadata.Exists() && !requestType.Exists() {
		if updated, ok := codexAppendTopLevelClientMetadataObjectAndResponseCreateType(body, entries); ok {
			return updated
		}
	}
	return codexSetClientMetadataNormalized(body, entries, true)
}

func codexSetClientMetadataAndPromptCacheKeyNormalized(body []byte, entries []codexClientMetadataEntry, promptCacheKey string) []byte {
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	if promptCacheKey == "" {
		return codexSetClientMetadataNormalized(body, entries, true)
	}
	existingPromptCacheKey := codexGJSONGetImmutableBytes(body, "prompt_cache_key")
	if len(entries) > 0 &&
		!existingPromptCacheKey.Exists() &&
		!codexBodyMayContainClientMetadata(body) {
		if updated, ok := codexAppendTopLevelPromptCacheKeyAndClientMetadataObject(body, promptCacheKey, entries); ok {
			return updated
		}
	}
	body = codexSetClientMetadataNormalized(body, entries, true)
	if existingPromptCacheKey.Exists() && existingPromptCacheKey.Type == gjson.String && existingPromptCacheKey.String() == promptCacheKey {
		return body
	}
	if !existingPromptCacheKey.Exists() {
		if updated, ok := codexAppendTopLevelStringField(body, "prompt_cache_key", promptCacheKey); ok {
			return updated
		}
	}
	updated, err := sjson.SetBytes(body, "prompt_cache_key", promptCacheKey)
	if err != nil {
		return body
	}
	return updated
}

func codexReplaceClientMetadataRaw(body []byte, metadata gjson.Result, metadataBody []byte) ([]byte, bool) {
	start, end, ok := codexJSONResultRawRange(body, metadata)
	if !ok {
		return nil, false
	}
	updated := make([]byte, 0, len(body)-len(metadata.Raw)+len(metadataBody))
	updated = append(updated, body[:start]...)
	updated = append(updated, metadataBody...)
	updated = append(updated, body[end:]...)
	return updated, true
}

func codexAppendTopLevelClientMetadataObject(body []byte, entries []codexClientMetadataEntry) ([]byte, bool) {
	fieldCount, fieldsCap := codexClientMetadataOverrideFieldsCapacity(entries)
	if fieldCount == 0 {
		return nil, false
	}
	trimmed, suffix, hasFields, ok := codexPrepareTopLevelObjectAppend(body)
	if !ok {
		return nil, false
	}

	extra := codexJSONStringCapacity("client_metadata") + fieldsCap + 3
	if hasFields {
		extra++
	}
	updated := make([]byte, 0, len(body)+extra)
	updated = append(updated, trimmed[:len(trimmed)-1]...)
	if hasFields {
		updated = append(updated, ',')
	}
	updated = codexAppendJSONString(updated, "client_metadata")
	updated = append(updated, ':', '{')
	wrote := false
	for _, entry := range entries {
		if wrote {
			updated = append(updated, ',')
		}
		updated = codexAppendJSONString(updated, entry.key)
		updated = append(updated, ':')
		updated = codexAppendJSONString(updated, entry.value)
		wrote = true
	}
	updated = append(updated, '}', '}')
	updated = append(updated, suffix...)
	return updated, true
}

func codexAppendTopLevelPromptCacheKeyAndClientMetadataObject(body []byte, promptCacheKey string, entries []codexClientMetadataEntry) ([]byte, bool) {
	fieldCount, fieldsCap := codexClientMetadataOverrideFieldsCapacity(entries)
	if fieldCount == 0 || promptCacheKey == "" {
		return nil, false
	}
	trimmed, suffix, hasFields, ok := codexPrepareTopLevelObjectAppend(body)
	if !ok {
		return nil, false
	}

	extra := codexJSONStringCapacity("prompt_cache_key") + codexJSONStringCapacity(promptCacheKey) + 1
	extra += codexJSONStringCapacity("client_metadata") + fieldsCap + 4
	if hasFields {
		extra++
	}
	updated := make([]byte, 0, len(body)+extra)
	updated = append(updated, trimmed[:len(trimmed)-1]...)
	if hasFields {
		updated = append(updated, ',')
	}
	updated = codexAppendJSONString(updated, "prompt_cache_key")
	updated = append(updated, ':')
	updated = codexAppendJSONString(updated, promptCacheKey)
	updated = append(updated, ',')
	updated = codexAppendJSONString(updated, "client_metadata")
	updated = append(updated, ':', '{')
	for i, entry := range entries {
		if i > 0 {
			updated = append(updated, ',')
		}
		updated = codexAppendJSONString(updated, entry.key)
		updated = append(updated, ':')
		updated = codexAppendJSONString(updated, entry.value)
	}
	updated = append(updated, '}', '}')
	updated = append(updated, suffix...)
	return updated, true
}

func codexAppendTopLevelClientMetadataObjectAndResponseCreateType(body []byte, entries []codexClientMetadataEntry) ([]byte, bool) {
	fieldCount, fieldsCap := codexClientMetadataOverrideFieldsCapacity(entries)
	if fieldCount == 0 {
		return nil, false
	}
	trimmed, suffix, hasFields, ok := codexPrepareTopLevelObjectAppend(body)
	if !ok {
		return nil, false
	}

	extra := codexJSONStringCapacity("client_metadata") + fieldsCap + 3
	extra += codexJSONStringCapacity("type") + codexJSONStringCapacity("response.create") + 2
	if hasFields {
		extra++
	}
	updated := make([]byte, 0, len(body)+extra)
	updated = append(updated, trimmed[:len(trimmed)-1]...)
	if hasFields {
		updated = append(updated, ',')
	}
	updated = codexAppendJSONString(updated, "client_metadata")
	updated = append(updated, ':', '{')
	wrote := false
	for _, entry := range entries {
		if wrote {
			updated = append(updated, ',')
		}
		updated = codexAppendJSONString(updated, entry.key)
		updated = append(updated, ':')
		updated = codexAppendJSONString(updated, entry.value)
		wrote = true
	}
	updated = append(updated, '}', ',')
	updated = codexAppendJSONString(updated, "type")
	updated = append(updated, ':')
	updated = codexAppendJSONString(updated, "response.create")
	updated = append(updated, '}')
	updated = append(updated, suffix...)
	return updated, true
}

func codexReplaceMergedClientMetadataObject(body []byte, metadata gjson.Result, entries []codexClientMetadataEntry) ([]byte, bool, bool) {
	overrideCount, overrideCap := codexClientMetadataOverrideFieldsCapacity(entries)
	if overrideCount == 0 && (!metadata.Exists() || !metadata.IsObject()) {
		return nil, false, false
	}
	start, end, ok := codexJSONResultRawRange(body, metadata)
	if !ok {
		return nil, false, false
	}

	updated := make([]byte, 0, len(body)-len(metadata.Raw)+len(metadata.Raw)+overrideCap)
	updated = append(updated, body[:start]...)
	updated = append(updated, '{')
	wrote := false
	changed := overrideCount > 0
	appendFieldRaw := func(key string, rawValue string) {
		if wrote {
			updated = append(updated, ',')
		}
		updated = codexAppendJSONString(updated, key)
		updated = append(updated, ':')
		updated = append(updated, rawValue...)
		wrote = true
	}
	appendFieldString := func(key string, value string) {
		if wrote {
			updated = append(updated, ',')
		}
		updated = codexAppendJSONString(updated, key)
		updated = append(updated, ':')
		updated = codexAppendJSONString(updated, value)
		wrote = true
	}

	metadata.ForEach(func(key, value gjson.Result) bool {
		keyString := key.String()
		if _, overwritten := codexClientMetadataOverrideValue(entries, keyString); overwritten {
			changed = true
			return true
		}
		if value.Type != gjson.String {
			changed = true
			return true
		}
		rawValue := value.Raw
		if rawValue == "" {
			appendFieldString(keyString, value.String())
			return true
		}
		appendFieldRaw(keyString, rawValue)
		return true
	})
	for _, entry := range entries {
		appendFieldString(entry.key, entry.value)
	}
	updated = append(updated, '}')
	updated = append(updated, body[end:]...)
	return updated, changed, true
}

func codexBuildClientMetadataObject(entries []codexClientMetadataEntry) ([]byte, bool) {
	if len(entries) == 0 {
		return nil, false
	}
	totalLen := 2
	for _, entry := range entries {
		totalLen += len(entry.key) + len(entry.value) + 8
	}
	body := make([]byte, 0, totalLen)
	body = append(body, '{')
	wrote := false
	for _, entry := range entries {
		if wrote {
			body = append(body, ',')
		}
		body = codexAppendJSONString(body, entry.key)
		body = append(body, ':')
		body = codexAppendJSONString(body, entry.value)
		wrote = true
	}
	if !wrote {
		return nil, false
	}
	body = append(body, '}')
	return body, true
}

func codexBuildMergedClientMetadataObject(metadata gjson.Result, entries []codexClientMetadataEntry) ([]byte, bool) {
	overrideCount, overrideCap := codexClientMetadataOverrideFieldsCapacity(entries)
	if overrideCount == 0 && (!metadata.Exists() || !metadata.IsObject()) {
		return nil, false
	}

	body := make([]byte, 0, len(metadata.Raw)+overrideCap)
	body = append(body, '{')
	wrote := false
	changed := overrideCount > 0
	appendFieldRaw := func(key string, rawValue string) {
		if wrote {
			body = append(body, ',')
		}
		body = codexAppendJSONString(body, key)
		body = append(body, ':')
		body = append(body, rawValue...)
		wrote = true
	}
	appendFieldString := func(key string, value string) {
		if wrote {
			body = append(body, ',')
		}
		body = codexAppendJSONString(body, key)
		body = append(body, ':')
		body = codexAppendJSONString(body, value)
		wrote = true
	}

	metadata.ForEach(func(key, value gjson.Result) bool {
		keyString := key.String()
		if _, overwritten := codexClientMetadataOverrideValue(entries, keyString); overwritten {
			changed = true
			return true
		}
		if value.Type != gjson.String {
			changed = true
			return true
		}
		rawValue := value.Raw
		if rawValue == "" {
			appendFieldString(keyString, value.String())
			return true
		}
		appendFieldRaw(keyString, rawValue)
		return true
	})
	for _, entry := range entries {
		appendFieldString(entry.key, entry.value)
	}
	body = append(body, '}')
	if !changed {
		return nil, false
	}
	return body, true
}

func codexClientMetadataOverrideFieldsCapacity(entries []codexClientMetadataEntry) (int, int) {
	capacity := 0
	for i, entry := range entries {
		if i > 0 {
			capacity++
		}
		capacity += codexJSONStringCapacity(entry.key) + codexJSONStringCapacity(entry.value) + 1
	}
	return len(entries), capacity
}

func codexClientMetadataOverrideValue(entries []codexClientMetadataEntry, key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	for _, entry := range entries {
		if entry.key == key {
			return entry.value, true
		}
	}
	return "", false
}

func codexNormalizeClientMetadataEntries(entries []codexClientMetadataEntry) []codexClientMetadataEntry {
	write := 0
	for i, entry := range entries {
		key := strings.TrimSpace(entry.key)
		value := strings.TrimSpace(entry.value)
		if key == "" || value == "" || codexClientMetadataHasLaterOverride(entries, i, key) {
			continue
		}
		entries[write] = codexClientMetadataEntry{key: key, value: value}
		write++
	}
	return entries[:write]
}

func codexCompactClientMetadataEntries(entries []codexClientMetadataEntry) []codexClientMetadataEntry {
	write := 0
	for _, entry := range entries {
		if entry.key == "" || entry.value == "" {
			continue
		}
		entries[write] = entry
		write++
	}
	return entries[:write]
}

func codexClientMetadataHasLaterOverride(entries []codexClientMetadataEntry, index int, key string) bool {
	key = strings.TrimSpace(key)
	for i := index + 1; i < len(entries); i++ {
		if strings.TrimSpace(entries[i].key) == key && strings.TrimSpace(entries[i].value) != "" {
			return true
		}
	}
	return false
}

func codexClientMetadataStringMapRaw(metadata gjson.Result, collectExistingKeys bool) ([]byte, map[string]struct{}, bool) {
	var existingKeys map[string]struct{}
	if collectExistingKeys {
		existingKeys = make(map[string]struct{})
	}
	if !metadata.Exists() || metadata.Type == gjson.Null {
		return []byte(`{}`), existingKeys, false
	}
	if !metadata.IsObject() {
		return []byte(`{}`), existingKeys, true
	}

	buf := make([]byte, 0, len(metadata.Raw)+2)
	buf = append(buf, '{')
	first := true
	changed := false
	metadata.ForEach(func(key, value gjson.Result) bool {
		keyString := key.String()
		if value.Type != gjson.String {
			changed = true
			return true
		}
		if !first {
			buf = append(buf, ',')
		}
		buf = codexAppendJSONString(buf, keyString)
		buf = append(buf, ':')
		buf = codexAppendJSONString(buf, value.String())
		if existingKeys != nil {
			existingKeys[keyString] = struct{}{}
		}
		first = false
		return true
	})
	buf = append(buf, '}')
	return buf, existingKeys, changed
}

func codexAppendJSONString(dst []byte, value string) []byte {
	originalLen := len(dst)
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 0x80 {
			return strconv.AppendQuote(dst[:originalLen], value)
		}
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, value[start:i]...)
		switch c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			const hex = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0x0f])
		}
		start = i + 1
	}
	dst = append(dst, value[start:]...)
	dst = append(dst, '"')
	return dst
}

func codexJSONStringCapacity(value string) int {
	capacity := len(value) + 2
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch c {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			capacity++
		default:
			if c < 0x20 {
				capacity += 5
			}
		}
	}
	return capacity
}

func codexResolvedInstallationID(target http.Header, source http.Header, auth *cliproxyauth.Auth, cfg *config.Config) string {
	if id := firstNonEmptyHeaderValue(target, nil, codexHeaderInstallationID); id != "" {
		return id
	}
	if cfg != nil {
		if id := strings.TrimSpace(cfg.CodexHeaderDefaults.InstallationID); id != "" {
			return id
		}
	}
	if id := codexAuthStringValue(auth, []string{
		"header:x-codex-installation-id",
		"header:X-Codex-Installation-Id",
		"x-codex-installation-id",
		"installation_id",
		"codex_installation_id",
	}); id != "" {
		return id
	}
	if id := firstNonEmptyHeaderValue(nil, source, codexHeaderInstallationID); id != "" {
		return id
	}
	if id := strings.TrimSpace(os.Getenv("CODEX_INSTALLATION_ID")); id != "" {
		return id
	}
	return codexDefaultInstallationID()
}

func codexDefaultInstallationID() string {
	codexInstallationIDOnce.Do(func() {
		codexInstallationID = uuid.NewString()
	})
	return codexInstallationID
}

func codexAuthStringValue(auth *cliproxyauth.Auth, keys []string) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		for _, key := range keys {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
	}
	if auth.Metadata != nil {
		for _, key := range keys {
			if value, ok := auth.Metadata[key].(string); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}
