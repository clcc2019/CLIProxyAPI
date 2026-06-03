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
	codexHeaderInstallationID = "X-Codex-Installation-Id"
	codexHeaderWindowID       = "X-Codex-Window-Id"
	codexHeaderParentThreadID = "X-Codex-Parent-Thread-Id"
	codexHeaderMemgenRequest  = "X-OpenAI-Memgen-Request"

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
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}

	source := codexGinHeadersFromContext(ctx)
	body = codexSetClientMetadataStrings(body, []codexClientMetadataEntry{
		{key: codexClientMetadataInstallationID, value: codexResolvedInstallationID(headers, source, auth, cfg)},
		{key: codexClientMetadataWindowID, value: firstNonEmptyHeaderValue(headers, source, codexHeaderWindowID)},
		{key: codexClientMetadataSubagent, value: firstNonEmptyHeaderValue(headers, source, "X-OpenAI-Subagent")},
		{key: codexClientMetadataParentThreadID, value: firstNonEmptyHeaderValue(headers, source, codexHeaderParentThreadID)},
		{key: codexClientMetadataTurnMetadata, value: firstNonEmptyHeaderValue(headers, source, codexHeaderTurnMetadata)},
		{key: codexWSClientMetadataTraceparent, value: firstNonEmptyHeaderValue(headers, source, "Traceparent")},
		{key: codexWSClientMetadataTracestate, value: firstNonEmptyHeaderValue(headers, source, "Tracestate")},
	}, true)

	// codex-rs carries websocket trace context through client_metadata, not a
	// top-level trace field.
	if gjson.GetBytes(body, "trace").Exists() {
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
	ensureHeaderWithPriority(target, source, codexHeaderMemgenRequest, "", "")
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

	metadataBody, existingKeys, changed := codexClientMetadataStringMapRaw(metadata)
	// existingKeys captures the keys already present in the existing metadata
	// object, so the loop below can skip them without parsing metadataBody
	// once per entry. Only populated when we actually need to respect existing
	// values (overwrite == false), otherwise existence checks are unnecessary.
	if overwrite {
		existingKeys = nil
	}
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

func codexClientMetadataStringMapRaw(metadata gjson.Result) ([]byte, map[string]struct{}, bool) {
	existingKeys := make(map[string]struct{})
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
		existingKeys[keyString] = struct{}{}
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
