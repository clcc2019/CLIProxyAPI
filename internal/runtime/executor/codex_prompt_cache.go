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
	if key := strings.TrimSpace(gjson.GetBytes(req.Payload, "prompt_cache_key").String()); key != "" {
		key = codexNormalizePromptCacheKey(key)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
		}
	}
	if key := strings.TrimSpace(gjson.GetBytes(req.Payload, "metadata.prompt_cache_key").String()); key != "" {
		key = codexNormalizePromptCacheKey(key)
		return codexPromptCacheResolution{
			cache:            helps.CodexCache{ID: key},
			headerEligibleID: key,
		}
	}

	scope := codexPromptCacheScope(ctx)

	// Path 2: Claude path retains legacy behaviour (model + user_id) so
	// existing deployments keep warming the same cache entry. We only fall
	// back to the generic fingerprinting logic when user_id is missing.
	if from == "claude" {
		if userID := strings.TrimSpace(gjson.GetBytes(req.Payload, "metadata.user_id").String()); userID != "" {
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

	if cached, ok := globalCodexPromptResolutionMemo.get(from, req.Model, scope, executionSessionID, req.Payload); ok {
		return cached
	}

	flightKey := promptResolutionMemoInflightKey(from, req.Model, scope, executionSessionID, req.Payload)
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
		if cached, ok := globalCodexPromptResolutionMemo.get(from, req.Model, scope, executionSessionID, req.Payload); ok {
			return cached, nil
		}
		resolution := codexPromptCacheResolution{}
		// Path 7: derive a conversation fingerprint from whatever
		// conversation-scoped fields the caller happened to include. The
		// fingerprint goes through codexCacheStore, so repeated requests with
		// the same fingerprint map to the same stable UUID even after the
		// translator re-renders the payload.
		if fp := conversationIdentifierFingerprint(req); fp != "" {
			key := "fp:" + scope + ":" + req.Model + ":" + fp
			cache := loadOrCreateCodexCache(key)
			resolution = codexPromptCacheResolution{
				cache:            cache,
				headerEligibleID: cache.ID,
			}
			globalCodexPromptResolutionMemo.set(from, req.Model, scope, executionSessionID, req.Payload, resolution)
			return resolution, nil
		}
		// Path 8: final structured fallback for clients without explicit
		// session signals. This preserves the existing content-based reuse.
		if fp := conversationContentFingerprint(req); fp != "" {
			key := "fp:" + scope + ":" + req.Model + ":" + fp
			resolution = codexPromptCacheResolution{cache: loadOrCreateCodexCache(key)}
			globalCodexPromptResolutionMemo.set(from, req.Model, scope, executionSessionID, req.Payload, resolution)
			return resolution, nil
		}

		// Path 9 (fallback): api_key-level stable UUID. This is strictly less
		// precise than a real conversation id but preserves backwards-compatible
		// behaviour for callers that send neither prompt_cache_key nor any
		// identifiable content (e.g. the upstream smoke tests that post just
		// {"model": "..."}).
		if from == "openai" {
			if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
				resolution = codexPromptCacheResolution{
					cache: helps.CodexCache{
						ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String(),
					},
				}
				globalCodexPromptResolutionMemo.set(from, req.Model, scope, executionSessionID, req.Payload, resolution)
				return resolution, nil
			}
		}

		globalCodexPromptResolutionMemo.set(from, req.Model, scope, executionSessionID, req.Payload, resolution)
		return resolution, nil
	})
	if err != nil {
		return codexPromptCacheResolution{}
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

// codexPromptCacheConversationHintFields lists the JSON paths inspected for a
// caller-owned conversation identifier, in precedence order. Declared as a
// package-level variable so it is constructed once rather than on every
// request.
var codexPromptCacheConversationHintFields = []string{
	"conversation_id",
	"conversationId",
	"thread_id",
	"threadId",
	"session_id",
	"sessionId",
	"metadata.conversation_id",
	"metadata.conversationId",
	"metadata.thread_id",
	"metadata.threadId",
	"metadata.session_id",
	"metadata.sessionId",
}

var codexPromptCacheHeaderHintKeys = []string{
	codexHeaderThreadID,
	codexHeaderOfficialThreadID,
	"X-Thread-ID",
	"Conversation_id",
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
	// gjson.GetManyBytes parses the payload once and resolves all paths in a
	// single traversal, avoiding the N re-parses that the old
	// `for path := range paths { gjson.GetBytes(...) }` loop incurred.
	results := gjson.GetManyBytes(payload, codexPromptCacheConversationHintFields...)
	for _, result := range results {
		if !result.Exists() {
			continue
		}
		if value := strings.TrimSpace(result.String()); value != "" {
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
	metadata := gjson.GetBytes(payload, "client_metadata."+codexClientMetadataTurnMetadata)
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
	raw := strings.TrimSpace(headers.Get(codexHeaderTurnMetadata))
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, path).String())
}

// codexConversationIdentifierFields lists the JSON paths that may carry an
// explicit conversation identifier, ordered from most-specific to least.
// Declared at package scope so gjson.GetManyBytes can resolve the whole set in
// a single payload traversal instead of re-parsing the body once per field.
var codexConversationIdentifierFields = []string{
	"metadata.conversation_id",
	"metadata.conversationId",
	"metadata.thread_id",
	"metadata.threadId",
	"metadata.session_id",
	"metadata.sessionId",
	"conversation_id",
	"conversationId",
	"thread_id",
	"threadId",
}

// conversationFingerprint extracts a conversation-scoped hint out of req.Payload.
// It intentionally inspects many candidate fields because we serve several
// provider schemas (Claude, OpenAI Chat, OpenAI Responses) and each encodes
// conversation identity differently. Returns an empty string when no stable
// hint can be found.
func conversationIdentifierFingerprint(req cliproxyexecutor.Request) string {
	payload := req.Payload
	if len(payload) == 0 {
		return ""
	}

	// Prefer explicit conversation identifiers before falling back to a
	// content hash. Order matters: more-specific wins.
	//
	// gjson.GetManyBytes parses the payload once and resolves all paths in
	// that single traversal, so the 11-field scan costs one parse instead of
	// eleven.
	results := gjson.GetManyBytes(payload, codexConversationIdentifierFields...)
	for i, result := range results {
		if !result.Exists() {
			continue
		}
		if v := strings.TrimSpace(result.String()); v != "" {
			return "id:" + shortHashString(codexConversationIdentifierFields[i]+"="+v)
		}
	}
	return ""
}

func conversationContentFingerprint(req cliproxyexecutor.Request) string {
	payload := req.Payload
	if len(payload) == 0 {
		return ""
	}

	// Content-derived fingerprint: hash the first user turn. Same first user
	// message + same model ⇒ same conversation, which is the assumption
	// prompt caching is built on anyway.
	if content := firstUserContent(payload); content != "" {
		if user := strings.TrimSpace(gjson.GetBytes(payload, "user").String()); user != "" {
			return "c:" + shortHashString("user="+user+"\x00content="+content)
		}
		return "c:" + shortHashString(content)
	}

	return ""
}

// firstUserContent returns a normalized string representation of the first
// user message, looking under the common field names used by the provider
// schemas this proxy accepts.
func firstUserContent(payload []byte) string {
	// OpenAI Chat Completions: messages[*].role == "user"
	if msgs := gjson.GetBytes(payload, "messages"); msgs.IsArray() {
		for _, m := range msgs.Array() {
			if strings.EqualFold(strings.TrimSpace(m.Get("role").String()), "user") {
				if c := strings.TrimSpace(m.Get("content").Raw); c != "" && c != "null" {
					return c
				}
			}
		}
	}
	// OpenAI Responses: input[*].role == "user"
	if inputs := gjson.GetBytes(payload, "input"); inputs.IsArray() {
		for _, m := range inputs.Array() {
			if strings.EqualFold(strings.TrimSpace(m.Get("role").String()), "user") {
				if c := strings.TrimSpace(m.Get("content").Raw); c != "" && c != "null" {
					return c
				}
			}
		}
		// If "input" is a flat array of strings/objects with no explicit role,
		// hash the whole first element.
		if first := inputs.Array(); len(first) > 0 {
			if c := strings.TrimSpace(first[0].Raw); c != "" && c != "null" {
				return c
			}
		}
	}
	// Anthropic Messages API: messages[*].role == "user"; same field name as
	// OpenAI chat so the first branch already handles it. Fall back to
	// top-level "prompt" for older / non-standard clients.
	if p := strings.TrimSpace(gjson.GetBytes(payload, "prompt").Raw); p != "" && p != "null" {
		return p
	}
	return ""
}

// codexPromptCacheScope produces a stable per-caller scope string. Scoping by
// api key (or gin client identity when available) keeps fingerprints from
// colliding across tenants — two different users asking "hello" should not
// share a prompt_cache_key even though their first-user-message hashes match.
func codexPromptCacheScope(ctx context.Context) string {
	if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
		return "api:" + shortHashString(apiKey)
	}
	return "anon"
}

// loadOrCreateCodexCache returns the cached UUID for key, creating a new
// entry with a 1-hour TTL when absent. Centralising this logic keeps the
// derivation paths consistent and avoids drift between Claude/OpenAI.
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
