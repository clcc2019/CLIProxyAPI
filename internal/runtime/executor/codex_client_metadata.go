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

	codexClientMetadataInstallationID = "x-codex-installation-id"
	codexClientMetadataWindowID       = "x-codex-window-id"
	codexClientMetadataParentThreadID = "x-codex-parent-thread-id"
	codexClientMetadataSubagent       = "x-openai-subagent"
	codexClientMetadataTurnMetadata   = "x-codex-turn-metadata"
	codexWSClientMetadataTraceparent  = "ws_request_header_traceparent"
	codexWSClientMetadataTracestate   = "ws_request_header_tracestate"
)

var (
	codexInstallationIDOnce sync.Once
	codexInstallationID     string
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
	if len(bytes.TrimSpace(body)) == 0 || req == nil {
		return body
	}
	return codexApplyHTTPClientMetadataWithSource(body, req.Header, codexGinHeadersFromContext(req.Context()), auth, cfg)
}

func codexApplyHTTPClientMetadataWithSource(body []byte, target http.Header, source http.Header, auth *cliproxyauth.Auth, cfg *config.Config) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	return codexSetClientMetadataString(
		body,
		codexClientMetadataInstallationID,
		codexResolvedInstallationID(target, source, auth, cfg),
		true,
	)
}

func codexApplyWebsocketClientMetadata(ctx context.Context, body []byte, headers http.Header, auth *cliproxyauth.Auth, cfg *config.Config) []byte {
	return codexApplyWebsocketClientMetadataWithStreamStartMS(ctx, body, headers, auth, cfg, "")
}

func codexApplyWebsocketClientMetadataWithStreamStartMS(ctx context.Context, body []byte, headers http.Header, auth *cliproxyauth.Auth, cfg *config.Config, streamStartMS string) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}

	source := codexGinHeadersFromContext(ctx)
	body = codexSetClientMetadataStrings(body, []codexClientMetadataEntry{
		{key: codexClientMetadataInstallationID, value: codexResolvedInstallationID(headers, source, auth, cfg)},
		{key: codexClientMetadataWindowID, value: firstNonEmptyHeaderValue(headers, source, codexHeaderWindowID)},
		{key: codexClientMetadataSubagent, value: firstNonEmptyHeaderValue(headers, source, codexWireHeaderOpenAISubagent)},
		{key: codexClientMetadataParentThreadID, value: firstNonEmptyHeaderValue(headers, source, codexHeaderParentThreadID)},
		{key: codexClientMetadataTurnMetadata, value: firstNonEmptyHeaderValue(headers, source, codexHeaderTurnMetadata)},
		{key: codexWSClientMetadataTraceparent, value: firstNonEmptyHeaderValue(headers, source, "Traceparent")},
		{key: codexWSClientMetadataTracestate, value: firstNonEmptyHeaderValue(headers, source, "Tracestate")},
		{key: codexClientMetadataWSStreamRequestStartMS, value: streamStartMS},
	}, true)

	// codex-rs carries websocket trace context through client_metadata, not a
	// top-level trace field.
	if bytes.Contains(body, []byte(`"trace"`)) && gjson.GetBytes(body, "trace").Exists() {
		if updated, err := sjson.DeleteBytes(body, "trace"); err == nil {
			body = updated
		}
	}
	return body
}

func codexEnsureResponsesIdentityHeaders(target http.Header, source http.Header) {
	if target == nil {
		return
	}
	ensureHeaderWithPriority(target, source, codexHeaderParentThreadID, "", "")
	ensureHeaderWithPriority(target, source, codexWireHeaderMemgenRequest, "", "")
	ensureHeaderWithPriority(target, source, codexHeaderWindowID, "", "")
	if strings.TrimSpace(target.Get(codexHeaderWindowID)) == "" {
		windowKey := firstNonEmptyHeaderValue(target, source, codexHeaderThreadID)
		if windowKey == "" {
			windowKey = strings.TrimSpace(target.Get(codexHeaderSessionID))
		}
		if windowKey != "" {
			if windowID := codexCurrentWindowID(windowKey); windowID != "" {
				target.Set(codexHeaderWindowID, windowID)
			}
		}
	}
}

func codexResetRequestBody(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

type codexClientMetadataEntry struct {
	key   string
	value string
}

func codexSetClientMetadataString(body []byte, key string, value string, overwrite bool) []byte {
	return codexSetClientMetadataStrings(body, []codexClientMetadataEntry{{key: key, value: value}}, overwrite)
}

func codexSetClientMetadataStrings(body []byte, entries []codexClientMetadataEntry, overwrite bool) []byte {
	if len(entries) == 0 || len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if overwrite && (!metadata.Exists() || metadata.Type == gjson.Null || !metadata.IsObject()) {
		if !metadata.Exists() {
			if updated, ok := codexAppendTopLevelClientMetadataObject(body, entries); ok {
				return updated
			}
		}
		if metadataBody, ok := codexBuildClientMetadataObject(entries); ok {
			if !metadata.Exists() {
				if updated, ok := codexAppendTopLevelRawField(body, "client_metadata", metadataBody); ok {
					return updated
				}
			}
			if updated, ok := codexReplaceClientMetadataRaw(body, metadata, metadataBody); ok {
				return updated
			}
			if updated, errSet := sjson.SetRawBytes(body, "client_metadata", metadataBody); errSet == nil {
				return updated
			}
		}
	}
	if overwrite && metadata.IsObject() {
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
		key := strings.TrimSpace(entry.key)
		value := strings.TrimSpace(entry.value)
		if value == "" || key == "" {
			continue
		}
		if !overwrite && existingKeys != nil {
			if _, ok := existingKeys[key]; ok {
				continue
			}
		}
		updated, errSet := sjson.SetBytes(metadataBody, key, value)
		if errSet != nil {
			continue
		}
		metadataBody = updated
		if existingKeys != nil {
			existingKeys[key] = struct{}{}
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
	overrideCount := codexClientMetadataValidOverrideCount(entries)
	if overrideCount == 0 {
		return nil, false
	}
	trimmed, suffix, hasFields, ok := codexPrepareTopLevelObjectAppend(body)
	if !ok {
		return nil, false
	}

	extra := len("client_metadata") + overrideCount*48 + 6
	updated := make([]byte, 0, len(body)+extra)
	updated = append(updated, trimmed[:len(trimmed)-1]...)
	if hasFields {
		updated = append(updated, ',')
	}
	updated = strconv.AppendQuote(updated, "client_metadata")
	updated = append(updated, ':', '{')
	wrote := false
	for i, entry := range entries {
		key := strings.TrimSpace(entry.key)
		value := strings.TrimSpace(entry.value)
		if key == "" || value == "" || codexClientMetadataHasLaterOverride(entries, i, key) {
			continue
		}
		if wrote {
			updated = append(updated, ',')
		}
		updated = strconv.AppendQuote(updated, key)
		updated = append(updated, ':')
		updated = strconv.AppendQuote(updated, value)
		wrote = true
	}
	updated = append(updated, '}', '}')
	updated = append(updated, suffix...)
	return updated, true
}

func codexBuildClientMetadataObject(entries []codexClientMetadataEntry) ([]byte, bool) {
	if len(entries) == 0 {
		return nil, false
	}
	totalLen := 2
	for _, entry := range entries {
		key := strings.TrimSpace(entry.key)
		value := strings.TrimSpace(entry.value)
		if key == "" || value == "" {
			continue
		}
		totalLen += len(key) + len(value) + 8
	}
	body := make([]byte, 0, totalLen)
	body = append(body, '{')
	wrote := false
	for _, entry := range entries {
		key := strings.TrimSpace(entry.key)
		value := strings.TrimSpace(entry.value)
		if key == "" || value == "" {
			continue
		}
		if wrote {
			body = append(body, ',')
		}
		body = strconv.AppendQuote(body, key)
		body = append(body, ':')
		body = strconv.AppendQuote(body, value)
		wrote = true
	}
	if !wrote {
		return nil, false
	}
	body = append(body, '}')
	return body, true
}

func codexBuildMergedClientMetadataObject(metadata gjson.Result, entries []codexClientMetadataEntry) ([]byte, bool) {
	overrideCount := codexClientMetadataValidOverrideCount(entries)
	if overrideCount == 0 && (!metadata.Exists() || !metadata.IsObject()) {
		return nil, false
	}

	body := make([]byte, 0, len(metadata.Raw)+overrideCount*48)
	body = append(body, '{')
	wrote := false
	changed := overrideCount > 0
	appendField := func(key string, rawValue string) {
		if wrote {
			body = append(body, ',')
		}
		body = strconv.AppendQuote(body, key)
		body = append(body, ':')
		body = append(body, rawValue...)
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
			rawValue = strconv.Quote(value.String())
		}
		appendField(keyString, rawValue)
		return true
	})
	for i, entry := range entries {
		key := strings.TrimSpace(entry.key)
		value := strings.TrimSpace(entry.value)
		if key == "" || value == "" || codexClientMetadataHasLaterOverride(entries, i, key) {
			continue
		}
		appendField(key, strconv.Quote(value))
	}
	body = append(body, '}')
	if !changed {
		return nil, false
	}
	return body, true
}

func codexClientMetadataValidOverrideCount(entries []codexClientMetadataEntry) int {
	count := 0
	for _, entry := range entries {
		if strings.TrimSpace(entry.key) != "" && strings.TrimSpace(entry.value) != "" {
			count++
		}
	}
	return count
}

func codexClientMetadataOverrideValue(entries []codexClientMetadataEntry, key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	value := ""
	found := false
	for _, entry := range entries {
		if strings.TrimSpace(entry.key) != key {
			continue
		}
		if candidate := strings.TrimSpace(entry.value); candidate != "" {
			value = candidate
			found = true
		}
	}
	return value, found
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

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	changed := false
	metadata.ForEach(func(key, value gjson.Result) bool {
		keyString := key.String()
		if value.Type != gjson.String {
			changed = true
			return true
		}
		if !first {
			buf.WriteByte(',')
		}
		buf.Write(strconv.AppendQuote(nil, keyString))
		buf.WriteByte(':')
		buf.Write(strconv.AppendQuote(nil, value.String()))
		if existingKeys != nil {
			existingKeys[keyString] = struct{}{}
		}
		first = false
		return true
	})
	buf.WriteByte('}')
	return buf.Bytes(), existingKeys, changed
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
