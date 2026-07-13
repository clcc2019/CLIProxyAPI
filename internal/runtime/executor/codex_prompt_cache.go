package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const codexPromptCacheKeyMaxLen = 64

// codexPromptCacheResolution carries the chosen prompt_cache_key alongside a
// second identifier that may be used as the fallback value for the
// Session_id/Thread_id headers. Both can be empty when the request lacks any
// conversation hint.
type codexPromptCacheResolution struct {
	cache            helps.CodexCache
	headerEligibleID string
	sessionHeaderID  string
	threadHeaderID   string
}

// resolvePromptCache decides which prompt_cache_key value to send upstream.
//
// Goals:
//  1. Keep requests belonging to the same logical conversation locked to a
//     single, stable key so upstream prompt caches actually get reused.
//  2. Keep unrelated conversations from the same API key *separated* so the
//     proxy doesn't accidentally stitch independent threads together (which
//     confuses both upstream cache and any server-side routing).
//  3. Preserve the legacy behaviour for clients that supply no conversation
//     hint at all, so existing API-key-only deployments still benefit from
//     some degree of caching.
//
// The precedence mirrors codex-rs: caller-owned thread/conversation IDs become
// prompt_cache_key, with additional fallbacks that mine conversation-scoped
// fields out of common client payloads.
func (e *CodexExecutor) resolvePromptCache(ctx context.Context, from sdktranslator.Format, req cliproxyexecutor.Request) helps.CodexCache {
	return e.resolvePromptCacheResolution(ctx, from, "", req).cache
}

func (e *CodexExecutor) resolvePromptCacheResolution(ctx context.Context, from sdktranslator.Format, executionSessionID string, req cliproxyexecutor.Request) codexPromptCacheResolution {
	if key := codexForcedUpstreamSessionID(ctx); key != "" {
		key = codexNormalizePromptCacheKey(key)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
			sessionHeaderID:  key,
			threadHeaderID:   key,
		}
	}

	// Path 1: the caller already supplied a prompt_cache_key. Trust it; this
	// is the codex-rs native path (prompt_cache_key == conversation_id).
	if key := strings.TrimSpace(codexGJSONGetImmutableBytes(req.Payload, "prompt_cache_key").String()); key != "" {
		key = codexNormalizePromptCacheKey(key)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
		}
	}
	if key := strings.TrimSpace(codexGJSONGetImmutableBytes(req.Payload, "metadata.prompt_cache_key").String()); key != "" {
		key = codexNormalizePromptCacheKey(key)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
		}
	}

	// Path 2: Claude path retains legacy behaviour (model + user_id) so
	// existing deployments keep warming the same cache entry. Claude Code's
	// structured session metadata gets a deterministic tenant-scoped key,
	// matching sub2api's metadata-session strategy without rotating after the
	// old one-hour process-local TTL. Unrecognised legacy user_id values keep
	// their historical mapping.
	if from == "claude" {
		if userID := strings.TrimSpace(codexGJSONGetImmutableBytes(req.Payload, "metadata.user_id").String()); userID != "" {
			if sessionID := extractClaudeCodeSessionIDForCodexReplay(req.Payload); sessionID != "" {
				cache := deterministicCodexPromptCache(stableCodexPromptCacheScope(ctx), req.Model, "claude-session:"+sessionID)
				return codexPromptCacheResolution{cache: cache}
			}
			key := fmt.Sprintf("%s-%s", req.Model, userID)
			return codexPromptCacheResolution{cache: loadOrCreateCodexCache(key)}
		}
	}

	// Path 3: native Codex/OpenAI clients may carry their conversation
	// identity in the body. Use the caller-owned value directly so the proxy
	// does not replace a valid upstream cache key with its own synthetic UUID.
	if key := codexPromptCachePayloadConversationHint(req.Payload); key != "" {
		key = codexNormalizePromptCacheKey(key)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
		}
	}

	// Path 4: websocket clients may carry the official turn metadata in
	// client_metadata instead of an HTTP header. Keep it ahead of header
	// fallbacks because it belongs to this specific request body.
	if threadID, sessionID := codexPromptCachePayloadTurnMetadataIDs(req.Payload); threadID != "" || sessionID != "" {
		key := threadID
		if key == "" {
			key = sessionID
		}
		key = codexNormalizePromptCacheKey(key)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
			sessionHeaderID:  sessionID,
			threadHeaderID:   threadID,
		}
	}

	// Path 5: native Codex/OpenAI clients often carry their conversation
	// identity in headers. Use that exact caller-owned key before deriving a
	// synthetic fingerprint, while preserving official Session_id/Thread_id
	// separation when both headers are present.
	if key := codexPromptCacheHeaderHint(ctx); key != "" {
		key = codexNormalizePromptCacheKey(key)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
		}
	}

	// Path 6: downstream websocket sessions already have a proxy-owned execution
	// session ID. Codex CLI uses its thread_id directly as prompt_cache_key; mirror
	// that before payload fingerprinting so long multi-turn requests do not pay to
	// hash large bodies just to arrive at the same session-scoped key.
	if executionSessionID = strings.TrimSpace(executionSessionID); executionSessionID != "" {
		key := codexNormalizePromptCacheKey(executionSessionID)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
		}
	}

	// Large, per-turn payloads rarely repeat. The memo deliberately excludes
	// them, so avoid also hashing the entire body merely to construct a
	// singleflight key that cannot lead to a memo hit.
	if len(req.Payload) > codexPromptResolutionMemoMaxPayload {
		codexMetrics.memoPromptMiss.Add(1)
		return resolveCodexPromptCacheResolutionUncached(ctx, from, req)
	}

	// Only the small-payload memo needs the fast process-local caller scope.
	// Explicit session paths and oversized requests have already returned.
	scope := codexPromptCacheScope(ctx)

	// Reuse one precomputed hash across the optimistic lookup, the in-flight
	// key, the post-singleflight lookup, and insertion. A cold small request
	// previously hashed its full payload up to four times on this hot path.
	memoHash := hashCodexPromptResolutionMemoKey(from, req.Model, scope, executionSessionID, req.Payload)
	if cached, ok := globalCodexPromptResolutionMemo.getWithHash(memoHash, from, req.Model, scope, executionSessionID, req.Payload); ok {
		return cached
	}

	flightKey := promptResolutionMemoInflightKeyWithHash(from, req.Model, scope, executionSessionID, memoHash)
	// Use WithoutCancel so the singleflight work keeps its tracing/request-id
	// context (used inside the callback for logging) but is not cancelled when
	// one particular caller's ctx is cancelled. Several callers may share this
	// flight; honouring any single caller's Done() would needlessly abort
	// useful work for the others.
	flightCtx := context.WithoutCancel(ctx)
	resolution, _, _, err := globalCodexPromptResolutionGroup.Do(flightCtx, flightKey, func() (codexPromptCacheResolution, error) {
		if err := flightCtx.Err(); err != nil {
			return codexPromptCacheResolution{}, err
		}
		if cached, ok := globalCodexPromptResolutionMemo.getWithHash(memoHash, from, req.Model, scope, executionSessionID, req.Payload); ok {
			return cached, nil
		}
		resolution := resolveCodexPromptCacheResolutionUncached(ctx, from, req)
		globalCodexPromptResolutionMemo.setWithHash(memoHash, from, req.Model, scope, executionSessionID, req.Payload, resolution)
		return resolution, nil
	})
	if err != nil {
		return codexPromptCacheResolution{}
	}
	return resolution
}

func resolveCodexPromptCacheResolutionUncached(ctx context.Context, from sdktranslator.Format, req cliproxyexecutor.Request) codexPromptCacheResolution {
	resolution := codexPromptCacheResolution{}
	// Path 7: final structured fallback for clients without explicit session
	// signals. The stable prefix fingerprint mirrors sub2api's compatibility
	// strategy: system/instructions, tools, tool choice, reasoning settings, and
	// the first user turn identify a logical cache bucket while later appended
	// turns do not churn it. Use a deterministic upstream key so a process
	// restart or the legacy one-hour local UUID expiry cannot discard a still
	// useful upstream prompt cache.
	if fp := conversationContentFingerprint(req); fp != "" {
		return codexPromptCacheResolution{cache: deterministicCodexPromptCache(stableCodexPromptCacheScope(ctx), req.Model, fp)}
	}

	// Path 8 (fallback): api_key-level stable UUID. This is strictly less
	// precise than a real conversation id but preserves backwards-compatible
	// behaviour for callers that send neither prompt_cache_key nor any
	// identifiable content (e.g. the upstream smoke tests that post just
	// {"model": "..."}).
	if from == "openai" {
		if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
			resolution.cache = helps.CodexCache{
				ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String(),
			}
		}
	}
	return resolution
}

func codexNormalizePromptCacheKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= codexPromptCacheKeyMaxLen {
		return key
	}
	return "pc-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache-key:"+key)).String()
}

var codexPromptCacheHeaderHintKeys = []string{
	codexHeaderThreadID,
	codexHeaderOfficialThreadID,
	"X-Thread-ID",
	"Conversation_id",
	"conversation_id",
	"Conversation-Id",
	"conversation-id",
	"X-Conversation-ID",
	codexHeaderSessionID,
	codexHeaderOfficialSessionID,
	"X-Session-ID",
}

var codexPromptCacheSessionHeaderKeys = []string{
	codexHeaderSessionID,
	codexHeaderOfficialSessionID,
	"X-Session-ID",
}

var codexPromptCacheThreadHeaderKeys = []string{
	codexHeaderThreadID,
	codexHeaderOfficialThreadID,
	"X-Thread-ID",
}

func codexPromptCachePayloadConversationHint(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	root := codexGJSONParseImmutableBytes(payload)
	if !root.IsObject() {
		return ""
	}

	var topLevel [6]string
	var metadata gjson.Result
	codexCollectPromptCacheConversationHints(root, &topLevel, &metadata)
	if value := firstCodexPromptCacheConversationHint(topLevel[:]); value != "" {
		return value
	}
	if !metadata.IsObject() {
		return ""
	}
	var nested [6]string
	codexCollectPromptCacheConversationHints(metadata, &nested, nil)
	return firstCodexPromptCacheConversationHint(nested[:])
}

func codexCollectPromptCacheConversationHints(object gjson.Result, values *[6]string, metadata *gjson.Result) {
	if values == nil || !object.IsObject() {
		return
	}
	object.ForEach(func(key, value gjson.Result) bool {
		keyString := key.String()
		if metadata != nil && keyString == "metadata" {
			if !metadata.Exists() && value.IsObject() {
				*metadata = value
			}
			return true
		}
		index := codexPromptCacheConversationHintIndex(keyString)
		if index < 0 || values[index] != "" {
			return true
		}
		values[index] = strings.TrimSpace(value.String())
		return true
	})
}

func codexPromptCacheConversationHintIndex(key string) int {
	switch key {
	case "conversation_id":
		return 0
	case "conversationId":
		return 1
	case "thread_id":
		return 2
	case "threadId":
		return 3
	case "session_id":
		return 4
	case "sessionId":
		return 5
	default:
		return -1
	}
}

func firstCodexPromptCacheConversationHint(values []string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func codexPromptCachePayloadTurnMetadataIDs(payload []byte) (threadID string, sessionID string) {
	return codexPromptCachePayloadTurnMetadataValue(payload, "thread_id"),
		codexPromptCachePayloadTurnMetadataValue(payload, "session_id")
}

func codexPromptCachePayloadTurnMetadataValue(payload []byte, path string) string {
	if len(payload) == 0 {
		return ""
	}
	metadata := codexGJSONGetImmutableBytes(payload, "client_metadata."+codexClientMetadataTurnMetadata)
	if metadata.IsObject() {
		return strings.TrimSpace(metadata.Get(path).String())
	}
	if metadata.Type != gjson.String {
		return ""
	}
	raw := strings.TrimSpace(metadata.String())
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, path).String())
}

func codexPromptCacheHeaderHint(ctx context.Context) string {
	headers := codexGinHeadersFromContext(ctx)
	if headers == nil {
		return ""
	}
	for _, key := range codexPromptCacheHeaderHintKeys {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			return value
		}
	}
	if value := codexPromptCacheTurnMetadataValue(headers, "thread_id"); value != "" {
		return value
	}
	if value := codexPromptCacheTurnMetadataValue(headers, "session_id"); value != "" {
		return value
	}
	return ""
}

func codexPromptCacheSessionHeaderValue(ctx context.Context, fallback string) string {
	headers := codexGinHeadersFromContext(ctx)
	for _, key := range codexPromptCacheSessionHeaderKeys {
		if headers != nil {
			if value := strings.TrimSpace(headers.Get(key)); value != "" {
				return value
			}
		}
	}
	if value := codexPromptCacheTurnMetadataValue(headers, "session_id"); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func codexPromptCacheThreadHeaderValue(ctx context.Context, fallback string) string {
	headers := codexGinHeadersFromContext(ctx)
	for _, key := range codexPromptCacheThreadHeaderKeys {
		if headers != nil {
			if value := strings.TrimSpace(headers.Get(key)); value != "" {
				return value
			}
		}
	}
	if value := codexPromptCacheTurnMetadataValue(headers, "thread_id"); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func codexPromptCacheTurnMetadataValue(headers http.Header, path string) string {
	if headers == nil {
		return ""
	}
	raw := trimHeaderValue(headers, codexHeaderTurnMetadata)
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, path).String())
}

func conversationContentFingerprint(req cliproxyexecutor.Request) string {
	payload := req.Payload
	if len(payload) == 0 {
		return ""
	}
	root := codexGJSONParseImmutableBytes(payload)
	if !root.IsObject() {
		return ""
	}

	// Build the key from the stable request prefix rather than the entire body.
	// Appended assistant/user turns therefore keep the same key, while requests
	// that happen to share a first user message no longer collide when their
	// instructions, tools, tool choice, or reasoning configuration differ.
	var seed strings.Builder
	appendField := func(name, value string) {
		value = strings.TrimSpace(value)
		if value == "" || value == "null" {
			return
		}
		seed.WriteString(name)
		seed.WriteByte('=')
		// Hash each component before composing the seed. This keeps the builder
		// bounded even when tools or the first user turn are very large.
		seed.WriteString(stableCodexPromptCacheFingerprint(value))
		seed.WriteByte('\x00')
	}
	appendResult := func(name string, result gjson.Result) {
		if !result.Exists() {
			return
		}
		appendField(name, result.Raw)
	}

	var instructions, system, tools, functions, toolChoice, functionCall gjson.Result
	var reasoning, reasoningEffort, verbosity, messages, input, user, prompt gjson.Result
	root.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "instructions":
			instructions = value
		case "system":
			system = value
		case "tools":
			tools = value
		case "functions":
			functions = value
		case "tool_choice":
			toolChoice = value
		case "function_call":
			functionCall = value
		case "reasoning":
			reasoning = value
		case "reasoning_effort":
			reasoningEffort = value
		case "verbosity":
			verbosity = value
		case "messages":
			messages = value
		case "input":
			input = value
		case "user":
			user = value
		case "prompt":
			prompt = value
		}
		return true
	})

	appendResult("instructions", instructions)
	appendResult("system", system)
	appendResult("tools", tools)
	appendResult("functions", functions)
	appendResult("tool_choice", toolChoice)
	appendResult("function_call", functionCall)
	appendResult("reasoning", reasoning)
	appendResult("reasoning_effort", reasoningEffort)
	appendResult("verbosity", verbosity)

	// Chat Completions and Anthropic messages may encode system/developer
	// instructions inside messages rather than in a top-level field.
	if messages.IsArray() {
		messages.ForEach(func(_, message gjson.Result) bool {
			role := strings.TrimSpace(message.Get("role").String())
			if strings.EqualFold(role, "system") || strings.EqualFold(role, "developer") {
				appendResult("message_"+strings.ToLower(role), message.Get("content"))
			}
			return true
		})
	}
	// Responses input can also carry developer/system messages in-band.
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			role := strings.TrimSpace(item.Get("role").String())
			if strings.EqualFold(role, "system") || strings.EqualFold(role, "developer") {
				appendResult("input_"+strings.ToLower(role), item.Get("content"))
			}
			return true
		})
	}

	if userValue := strings.TrimSpace(user.String()); userValue != "" {
		appendField("user", userValue)
	}
	if content := firstUserContent(messages, input, prompt); content != "" {
		appendField("first_user", content)
	}
	if seed.Len() == 0 {
		return ""
	}
	return "c:" + stableCodexPromptCacheFingerprint(seed.String())
}

// firstUserContent returns a normalized string representation of the first
// user message, looking under the common field names used by the provider
// schemas this proxy accepts.
func firstUserContent(messages, input, prompt gjson.Result) string {
	// OpenAI Chat Completions: messages[*].role == "user"
	if messages.IsArray() {
		var content string
		messages.ForEach(func(_, message gjson.Result) bool {
			if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "user") {
				content = strings.TrimSpace(message.Get("content").Raw)
				if content != "" && content != "null" {
					return false
				}
				content = ""
			}
			return true
		})
		if content != "" {
			return content
		}
	}
	// OpenAI Responses: input[*].role == "user"
	if input.IsArray() {
		var content, first string
		input.ForEach(func(_, item gjson.Result) bool {
			if first == "" {
				first = strings.TrimSpace(item.Raw)
			}
			if strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
				content = strings.TrimSpace(item.Get("content").Raw)
				if content != "" && content != "null" {
					return false
				}
				content = ""
			}
			return true
		})
		if content != "" {
			return content
		}
		// If "input" is a flat array of strings/objects with no explicit role,
		// hash the whole first element.
		if first != "" && first != "null" {
			return first
		}
	}
	// Anthropic Messages API: messages[*].role == "user"; same field name as
	// OpenAI chat so the first branch already handles it. Fall back to
	// top-level "prompt" for older / non-standard clients.
	if p := strings.TrimSpace(prompt.Raw); p != "" && p != "null" {
		return p
	}
	return ""
}

// codexPromptCacheScope produces a cheap process-local caller scope for memo
// keys. Upstream synthetic keys use stableCodexPromptCacheScope instead.
func codexPromptCacheScope(ctx context.Context) string {
	if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
		return "api:" + shortHashString(apiKey)
	}
	return "anon"
}

func stableCodexPromptCacheScope(ctx context.Context) string {
	if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
		return "api:" + stableCodexPromptCacheFingerprint(apiKey)
	}
	return "anon"
}

// stableCodexPromptCacheFingerprint is intentionally process-independent.
// maphash is ideal for local memo/dedupe tables but is randomly seeded at
// startup, which would rotate synthetic prompt_cache_key values after every
// restart and throw away reusable upstream cache state.
var codexPromptCacheFingerprintNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache-fingerprint"))
var codexSyntheticPromptCacheNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:synthetic-prompt-cache"))

func stableCodexPromptCacheFingerprint(value string) string {
	return uuid.NewSHA1(codexPromptCacheFingerprintNamespace, []byte(value)).String()
}

func deterministicCodexPromptCache(scope, model, fingerprint string) helps.CodexCache {
	seed := strings.Join([]string{
		strings.TrimSpace(scope),
		strings.TrimSpace(model),
		fingerprint,
	}, "\x00")
	return helps.CodexCache{ID: "pc-" + uuid.NewSHA1(codexSyntheticPromptCacheNamespace, []byte(seed)).String()}
}

// loadOrCreateCodexCache preserves the legacy model+metadata.user_id mapping
// used by Claude callers. Synthetic content fallbacks use deterministic keys
// instead, so their lifetime is governed only by the upstream prompt cache.
func loadOrCreateCodexCache(key string) helps.CodexCache {
	if cache, ok := helps.GetCodexCache(key); ok {
		return cache
	}
	cache := helps.CodexCache{
		ID:     uuid.New().String(),
		Expire: time.Now().Add(time.Hour),
	}
	helps.SetCodexCache(key, cache)
	return cache
}
