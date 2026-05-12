package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// RoundRobinSelector provides a simple provider scoped round-robin selection strategy.
//
// Cursor state is stored in a sync.Map of *atomic.Uint64, so Pick advances the
// per-(provider,model) counter with a single atomic add and no mutex on the hot
// path. A rarely-taken resetMu protects the whole-map reset used to cap memory
// growth when the number of distinct keys exceeds maxKeys.
type RoundRobinSelector struct {
	cursors atomic.Pointer[sync.Map] // map[string]*atomic.Uint64
	size    atomic.Int64
	resetMu sync.Mutex
	maxKeys int
}

// FillFirstSelector selects the first available credential (deterministic ordering).
// This "burns" one account before moving to the next, which can help stagger
// rolling-window subscription caps (e.g. chat message limits).
type FillFirstSelector struct{}

var authPriorityParseCache sync.Map

type blockReason int

const (
	blockReasonNone blockReason = iota
	blockReasonCooldown
	blockReasonDisabled
	blockReasonOther
)

type modelCooldownError struct {
	model    string
	resetIn  time.Duration
	provider string
}

func newModelCooldownError(model, provider string, resetIn time.Duration) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
	}
}

func (e *modelCooldownError) Error() string {
	modelName := e.model
	if modelName == "" {
		modelName = "requested model"
	}
	message := fmt.Sprintf("All credentials for model %s are cooling down", modelName)
	if e.provider != "" {
		message = fmt.Sprintf("%s via provider %s", message, e.provider)
	}
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	displayDuration := e.resetIn
	if displayDuration > 0 && displayDuration < time.Second {
		displayDuration = time.Second
	} else {
		displayDuration = displayDuration.Round(time.Second)
	}
	errorBody := map[string]any{
		"code":          "model_cooldown",
		"message":       message,
		"model":         e.model,
		"reset_time":    displayDuration.String(),
		"reset_seconds": resetSeconds,
	}
	if e.provider != "" {
		errorBody["provider"] = e.provider
	}
	payload := map[string]any{"error": errorBody}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"model_cooldown","message":"%s"}}`, message)
	}
	return string(data)
}

func (e *modelCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *modelCooldownError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	headers.Set("Retry-After", strconv.Itoa(resetSeconds))
	return headers
}

func authPriority(auth *Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	if cached, ok := authPriorityParseCache.Load(raw); ok {
		if priority, okPriority := cached.(int); okPriority {
			return priority
		}
		authPriorityParseCache.Delete(raw)
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		authPriorityParseCache.Store(raw, 0)
		return 0
	}
	authPriorityParseCache.Store(raw, parsed)
	return parsed
}

func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	modelName := strings.TrimSpace(parsed.ModelName)
	if modelName == "" {
		return model
	}
	return modelName
}

func authWebsocketsEnabled(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		for _, key := range []string{"websockets", "websocket"} {
			if raw := strings.TrimSpace(auth.Attributes[key]); raw != "" {
				parsed, errParse := strconv.ParseBool(raw)
				if errParse == nil {
					return parsed
				}
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	for _, key := range []string{"websockets", "websocket"} {
		raw, ok := auth.Metadata[key]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case bool:
			return v
		case string:
			parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
			if errParse == nil {
				return parsed
			}
		default:
		}
	}
	return false
}

func preferCodexWebsocketAuths(ctx context.Context, provider string, available []*Auth) []*Auth {
	if len(available) == 0 {
		return available
	}
	if !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return available
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return available
	}

	wsEnabled := make([]*Auth, 0, len(available))
	for i := 0; i < len(available); i++ {
		candidate := available[i]
		if authWebsocketsEnabled(candidate) {
			wsEnabled = append(wsEnabled, candidate)
		}
	}
	if len(wsEnabled) > 0 {
		return wsEnabled
	}
	return available
}

func collectAvailableByPriority(auths []*Auth, model string, now time.Time) (available map[int][]*Auth, cooldownCount int, earliest time.Time) {
	available = make(map[int][]*Auth)
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked {
			priority := authPriority(candidate)
			available[priority] = append(available[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}
	return available, cooldownCount, earliest
}

func getAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority, cooldownCount, earliest := collectAvailableByPriority(auths, model, now)
	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	bestPriority := 0
	found := false
	for priority := range availableByPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}

	available := availableByPriority[bestPriority]
	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, nil
}

// Pick selects the next available auth for the provider in a round-robin manner.
// For gemini-cli virtual auths (identified by the gemini_virtual_parent attribute),
// a two-level round-robin is used: first cycling across credential groups (parent
// accounts), then cycling within each group's project auths.
func (s *RoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	key := provider + ":" + canonicalModelKey(model)

	// Check if any available auth has gemini_virtual_parent attribute,
	// indicating gemini-cli virtual auths that should use credential-level polling.
	groups, parentOrder := groupByVirtualParent(available)
	if len(parentOrder) > 1 {
		// Two-level round-robin: first select a credential group, then pick within it.
		groupKey := key + "::group"
		groupIndex := s.nextCursor(groupKey, uint64(len(parentOrder)))
		selectedParent := parentOrder[groupIndex%uint64(len(parentOrder))]
		group := groups[selectedParent]

		// Second level: round-robin within the selected credential group.
		innerKey := key + "::cred:" + selectedParent
		innerIndex := s.nextCursor(innerKey, 0)
		return group[innerIndex%uint64(len(group))], nil
	}

	// Flat round-robin for non-grouped auths (original behavior).
	index := s.nextCursor(key, 0)
	return available[index%uint64(len(available))], nil
}

// nextCursor atomically returns the current cursor value for key and advances it
// by one. When seedMod > 1 and the cursor is first created, the initial value is
// randomized within [0, seedMod) so independent goroutines do not all start at 0.
//
// To bound memory, the underlying map is rotated (replaced with a fresh empty
// map) the first time an insertion would cross maxKeys. Rotation is rare and
// guarded by resetMu; the hot path (cache hit) is purely atomic.
func (s *RoundRobinSelector) nextCursor(key string, seedMod uint64) uint64 {
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}

	for {
		m := s.loadCursors()
		if cursor, ok := m.Load(key); ok {
			return cursor.(*atomic.Uint64).Add(1) - 1
		}

		// Adding a new key. Rotate first if we'd exceed the cap.
		if s.size.Load() >= int64(limit) {
			s.resetMu.Lock()
			if s.size.Load() >= int64(limit) {
				s.cursors.Store(&sync.Map{})
				s.size.Store(0)
			}
			s.resetMu.Unlock()
			continue
		}

		counter := &atomic.Uint64{}
		if seedMod > 1 {
			counter.Store(uint64(rand.IntN(int(seedMod))))
		}
		if existing, loaded := m.LoadOrStore(key, counter); loaded {
			return existing.(*atomic.Uint64).Add(1) - 1
		}
		s.size.Add(1)
		return counter.Add(1) - 1
	}
}

func (s *RoundRobinSelector) loadCursors() *sync.Map {
	if m := s.cursors.Load(); m != nil {
		return m
	}
	fresh := &sync.Map{}
	if s.cursors.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return s.cursors.Load()
}

// groupByVirtualParent groups auths by their gemini_virtual_parent attribute.
// Returns a map of parentID -> auths and a sorted slice of parent IDs for stable iteration.
// Only auths with a non-empty gemini_virtual_parent are grouped; if any auth lacks
// this attribute, nil/nil is returned so the caller falls back to flat round-robin.
func groupByVirtualParent(auths []*Auth) (map[string][]*Auth, []string) {
	if len(auths) == 0 {
		return nil, nil
	}
	groups := make(map[string][]*Auth)
	for _, a := range auths {
		parent := ""
		if a.Attributes != nil {
			parent = strings.TrimSpace(a.Attributes["gemini_virtual_parent"])
		}
		if parent == "" {
			// Non-virtual auth present; fall back to flat round-robin.
			return nil, nil
		}
		groups[parent] = append(groups[parent], a)
	}
	// Collect parent IDs in sorted order for stable cursor indexing.
	parentOrder := make([]string, 0, len(groups))
	for p := range groups {
		parentOrder = append(parentOrder, p)
	}
	sort.Strings(parentOrder)
	return groups, parentOrder
}

// Pick selects the first available auth for the provider in a deterministic manner.
func (s *FillFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	return available[0], nil
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			state, ok := auth.ModelStates[model]
			if (!ok || state == nil) && model != "" {
				baseModel := canonicalModelKey(model)
				if baseModel != "" && baseModel != model {
					state, ok = auth.ModelStates[baseModel]
				}
			}
			if ok && state != nil {
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return false, blockReasonNone, time.Time{}
					}
					if state.NextRetryAfter.After(now) {
						next := state.NextRetryAfter
						if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						if next.Before(now) {
							next = now
						}
						if state.Quota.Exceeded {
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				return false, blockReasonNone, time.Time{}
			}
		}
		// Fall through to auth-level check ONLY when this credential carries
		// the auth-scope quota flag — i.e. an AuthScopedFailure like Kiro's
		// shared AGENTIC_REQUEST 429 marked every model on this auth as
		// exhausted. Without this narrow scope, ordinary per-model 429s
		// would accidentally block unrelated models through the aggregate
		// Unavailable flag, breaking multi-model routing on other providers.
		if auth.Quota.AuthScope && auth.Unavailable && auth.NextRetryAfter.After(now) {
			next := auth.NextRetryAfter
			if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
				next = auth.Quota.NextRecoverAt
			}
			if next.Before(now) {
				next = now
			}
			return true, blockReasonCooldown, next
		}
		return false, blockReasonNone, time.Time{}
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		next := auth.NextRetryAfter
		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			next = auth.Quota.NextRecoverAt
		}
		if next.Before(now) {
			next = now
		}
		if auth.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

// AuthAvailableForModel reports whether the auth is currently usable for the model.
func AuthAvailableForModel(auth *Auth, model string, now time.Time) bool {
	blocked, _, _ := isAuthBlockedForModel(auth, model, now)
	return !blocked
}

// sessionPattern matches Claude Code user_id format:
// user_{hash}_account__session_{uuid}
var sessionPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// SessionAffinitySelector wraps another selector with session-sticky behavior.
// It extracts session ID from multiple sources and maintains session-to-auth
// mappings with automatic failover when the bound auth becomes unavailable.
//
// Pick is lock-free at the selector level: SessionCache is already thread-safe
// and the fallback selector handles its own synchronization, so a shared mutex
// here would serialize every session-affinity request for no benefit.
type SessionAffinitySelector struct {
	fallback Selector
	cache    *SessionCache
}

// SessionAffinityConfig configures the session affinity selector.
type SessionAffinityConfig struct {
	Fallback Selector
	TTL      time.Duration
}

// NewSessionAffinitySelector creates a new session-aware selector.
func NewSessionAffinitySelector(fallback Selector) *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: fallback,
		TTL:      time.Hour,
	})
}

// NewSessionAffinitySelectorWithConfig creates a selector with custom configuration.
func NewSessionAffinitySelectorWithConfig(cfg SessionAffinityConfig) *SessionAffinitySelector {
	if cfg.Fallback == nil {
		cfg.Fallback = &RoundRobinSelector{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = time.Hour
	}
	return &SessionAffinitySelector{
		fallback: cfg.Fallback,
		cache:    NewSessionCache(cfg.TTL),
	}
}

// Pick selects an auth with session affinity when possible.
// Priority for session ID extraction:
//  1. metadata.user_id (Claude Code format) - highest priority
//  2. X-Session-ID / Session_id / Conversation_id header
//  3. execution session metadata / explicit body session identifiers
//  4. metadata.user_id (non-Claude Code format)
//  5. prompt_cache_key / metadata.prompt_cache_key
//  6. Kiro native conversationState identifiers
//  7. Hash-based fallback from messages
//
// Note: The cache key includes provider and session ID, but intentionally does
// not include model. Claude Code and similar clients can issue multiple model
// requests concurrently for one session; binding by model would split one
// client across multiple credentials.
func (s *SessionAffinitySelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	var entry *log.Entry
	debugLog := func(format string, args ...any) {
		if !log.IsLevelEnabled(log.DebugLevel) {
			return
		}
		if entry == nil {
			entry = selectorLogEntry(ctx)
		}
		entry.Debugf(format, args...)
	}
	infoLog := func(format string, args ...any) {
		if !log.IsLevelEnabled(log.InfoLevel) {
			return
		}
		if entry == nil {
			entry = selectorLogEntry(ctx)
		}
		entry.Infof(format, args...)
	}

	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		debugLog("session-affinity: no session ID extracted, falling back to default selector | provider=%s model=%s", provider, model)
		return s.fallback.Pick(ctx, provider, model, opts, auths)
	}

	// No outer lock: SessionCache is thread-safe on its own and the fallback
	// selector handles its own synchronization. Holding a shared mutex here
	// would serialize every request and defeat the purpose of sharding work
	// across sessions.
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}

	cacheKey := sessionAffinityCacheKey(provider, primaryID)

	if cachedAuthID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		for _, auth := range available {
			if auth.ID == cachedAuthID {
				debugLog("session-affinity: cache hit | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
				return auth, nil
			}
		}
		// Cached auth not available, reselect via fallback selector for even distribution
		auth, err := s.fallback.Pick(ctx, provider, model, opts, auths)
		if err != nil {
			return nil, err
		}
		s.cache.Set(cacheKey, auth.ID)
		infoLog("session-affinity: cache hit but auth unavailable, reselected | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
		return auth, nil
	}

	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey := sessionAffinityCacheKey(provider, fallbackID)
		if cachedAuthID, ok := s.cache.Get(fallbackKey); ok {
			for _, auth := range available {
				if auth.ID == cachedAuthID {
					s.cache.Set(cacheKey, auth.ID)
					infoLog("session-affinity: fallback cache hit | session=%s fallback=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), auth.ID, provider, model)
					return auth, nil
				}
			}
		}
	}

	auth, err := s.fallback.Pick(ctx, provider, model, opts, auths)
	if err != nil {
		return nil, err
	}
	s.cache.Set(cacheKey, auth.ID)
	infoLog("session-affinity: cache miss, new binding | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
	return auth, nil
}

func sessionAffinityCacheKey(provider, sessionID string) string {
	return strings.TrimSpace(strings.ToLower(provider)) + "::" + sessionID
}

func selectorLogEntry(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

// truncateSessionID shortens session ID for logging (first 8 chars + "...")
func truncateSessionID(id string) string {
	if len(id) <= 20 {
		return id
	}
	return id[:8] + "..."
}

// Stop releases resources held by the selector.
func (s *SessionAffinitySelector) Stop() {
	if s.cache != nil {
		s.cache.Stop()
	}
}

// InvalidateAuth removes all session bindings for a specific auth.
// Called when an auth becomes rate-limited or unavailable.
func (s *SessionAffinitySelector) InvalidateAuth(authID string) {
	if s.cache != nil {
		s.cache.InvalidateAuth(authID)
	}
}

// ExtractSessionID extracts session identifier from multiple sources.
// Priority order:
//  1. metadata.user_id (Claude Code format with _session_{uuid}) - highest priority for Claude Code clients
//  2. X-Session-ID header
//  3. Session_id header (Codex)
//  4. X-Amp-Thread-Id header (Amp CLI thread ID)
//  5. X-Client-Request-Id header (PI)
//  6. Conversation_id header / execution session metadata
//  7. explicit body session_id / thread_id fields
//  8. metadata.user_id (non-Claude Code format)
//  9. prompt_cache_key / metadata.prompt_cache_key in request body
//  10. explicit conversation_id fields
//  11. Kiro native conversationState identifiers
//  12. Stable hash from first few messages content (fallback)
func ExtractSessionID(headers http.Header, payload []byte, metadata map[string]any) string {
	primary, _ := extractSessionIDs(headers, payload, metadata)
	return primary
}

// extractSessionIDs returns (primaryID, fallbackID) for session affinity.
// primaryID: full hash including assistant response (stable after first turn)
// fallbackID: short hash without assistant (used to inherit binding from first turn)
func extractSessionIDs(headers http.Header, payload []byte, metadata map[string]any) (string, string) {
	// 1. metadata.user_id with Claude Code session format (highest priority)
	if len(payload) > 0 {
		userID := gjson.GetBytes(payload, "metadata.user_id").String()
		if userID != "" {
			// Old format: user_{hash}_account__session_{uuid}
			if matches := sessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
				id := "claude:" + matches[1]
				return id, ""
			}
			// New format: JSON object with session_id field
			// e.g. {"device_id":"...","account_uuid":"...","session_id":"uuid"}
			if len(userID) > 0 && userID[0] == '{' {
				if sid := gjson.Get(userID, "session_id").String(); sid != "" {
					return "claude:" + sid, ""
				}
			}
		}
	}

	// 2. X-Session-ID header
	if headers != nil {
		if sid := headers.Get("X-Session-ID"); sid != "" {
			return "header:" + sid, ""
		}
	}

	// 3. Session_id header (Codex)
	if headers != nil {
		if sid := headers.Get("Session_id"); sid != "" {
			return "codex:" + sid, ""
		}
	}

	// 4. X-Amp-Thread-Id header (Amp CLI thread ID)
	if headers != nil {
		if tid := headers.Get("X-Amp-Thread-Id"); tid != "" {
			return "amp:" + tid, ""
		}
	}

	// 5. X-Client-Request-Id header (PI)
	if headers != nil {
		if rid := headers.Get("X-Client-Request-Id"); rid != "" {
			return "clientreq:" + rid, ""
		}
	}

	if headers != nil {
		if convID := headers.Get("Conversation_id"); convID != "" {
			return "conv:" + convID, ""
		}
	}

	// 6. explicit execution session metadata (Responses websocket/session handlers)
	if sessionID := metadataStringValue(metadata, cliproxyexecutor.ExecutionSessionMetadataKey); sessionID != "" {
		return "exec:" + sessionID, ""
	}

	if len(payload) == 0 {
		return "", ""
	}

	// 7. explicit body session/thread identifiers
	for _, candidate := range []struct {
		path   string
		prefix string
	}{
		{path: "metadata.session_id", prefix: "session:"},
		{path: "metadata.sessionId", prefix: "session:"},
		{path: "session_id", prefix: "session:"},
		{path: "sessionId", prefix: "session:"},
		{path: "metadata.thread_id", prefix: "thread:"},
		{path: "metadata.threadId", prefix: "thread:"},
		{path: "thread_id", prefix: "thread:"},
		{path: "threadId", prefix: "thread:"},
	} {
		if sessionID := strings.TrimSpace(gjson.GetBytes(payload, candidate.path).String()); sessionID != "" {
			return candidate.prefix + sessionID, ""
		}
	}

	// 8. metadata.user_id (non-Claude Code format)
	userID := gjson.GetBytes(payload, "metadata.user_id").String()
	if userID != "" {
		return "user:" + userID, ""
	}

	// 9. prompt_cache_key / metadata.prompt_cache_key
	if promptCacheKey := gjson.GetBytes(payload, "prompt_cache_key").String(); promptCacheKey != "" {
		return "cache:" + promptCacheKey, ""
	}
	if promptCacheKey := gjson.GetBytes(payload, "metadata.prompt_cache_key").String(); promptCacheKey != "" {
		return "cache:" + promptCacheKey, ""
	}

	// 10. explicit conversation identifiers
	for _, path := range []string{
		"metadata.conversation_id",
		"metadata.conversationId",
		"conversation_id",
		"conversationId",
	} {
		if convID := strings.TrimSpace(gjson.GetBytes(payload, path).String()); convID != "" {
			return "conv:" + convID, ""
		}
	}

	// 11. Kiro native conversationState identifiers
	if convID := gjson.GetBytes(payload, "conversationState.conversationId").String(); convID != "" {
		fallbackID := ""
		if continuationID := gjson.GetBytes(payload, "conversationState.agentContinuationId").String(); continuationID != "" && continuationID != convID {
			fallbackID = "kiro-cont:" + continuationID
		}
		return "conv:" + convID, fallbackID
	}
	if convID := gjson.GetBytes(payload, "conversationState.currentMessage.userInputMessage.userInputMessageContext.conversationId").String(); convID != "" {
		return "conv:" + convID, ""
	}
	if continuationID := gjson.GetBytes(payload, "conversationState.agentContinuationId").String(); continuationID != "" {
		return "kiro-cont:" + continuationID, ""
	}

	// 12. Hash-based fallback from message content
	return extractMessageHashIDs(payload)
}

func metadataStringValue(metadata map[string]any, key string) string {
	if len(metadata) == 0 || key == "" {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func extractMessageHashIDs(payload []byte) (primaryID, fallbackID string) {
	var systemPrompt, firstUserMsg, firstAssistantMsg string

	// OpenAI/Claude messages format
	messages := gjson.GetBytes(payload, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			content := extractMessageContent(msg.Get("content"))
			if content == "" {
				return true
			}

			switch role {
			case "system":
				if systemPrompt == "" {
					systemPrompt = truncateString(content, 100)
				}
			case "user":
				if firstUserMsg == "" {
					firstUserMsg = truncateString(content, 100)
				}
			case "assistant":
				if firstAssistantMsg == "" {
					firstAssistantMsg = truncateString(content, 100)
				}
			}

			if systemPrompt != "" && firstUserMsg != "" && firstAssistantMsg != "" {
				return false
			}
			return true
		})
	}

	// Claude API: top-level "system" field (array or string)
	if systemPrompt == "" {
		topSystem := gjson.GetBytes(payload, "system")
		if topSystem.Exists() {
			if topSystem.IsArray() {
				topSystem.ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text").String(); text != "" && systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
						return false
					}
					return true
				})
			} else if topSystem.Type == gjson.String {
				systemPrompt = truncateString(topSystem.String(), 100)
			}
		}
	}

	// Gemini format
	if systemPrompt == "" && firstUserMsg == "" {
		sysInstr := gjson.GetBytes(payload, "systemInstruction.parts")
		if sysInstr.Exists() && sysInstr.IsArray() {
			sysInstr.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" && systemPrompt == "" {
					systemPrompt = truncateString(text, 100)
					return false
				}
				return true
			})
		}

		contents := gjson.GetBytes(payload, "contents")
		if contents.Exists() && contents.IsArray() {
			contents.ForEach(func(_, msg gjson.Result) bool {
				role := msg.Get("role").String()
				msg.Get("parts").ForEach(func(_, part gjson.Result) bool {
					text := part.Get("text").String()
					if text == "" {
						return true
					}
					switch role {
					case "user":
						if firstUserMsg == "" {
							firstUserMsg = truncateString(text, 100)
						}
					case "model":
						if firstAssistantMsg == "" {
							firstAssistantMsg = truncateString(text, 100)
						}
					}
					return false
				})
				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	// OpenAI Responses API format (v1/responses)
	if systemPrompt == "" && firstUserMsg == "" {
		if instr := gjson.GetBytes(payload, "instructions").String(); instr != "" {
			systemPrompt = truncateString(instr, 100)
		}

		input := gjson.GetBytes(payload, "input")
		if input.Exists() && input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				itemType := item.Get("type").String()
				if itemType == "reasoning" {
					return true
				}
				// Skip non-message typed items (function_call, function_call_output, etc.)
				// but allow items with no type that have a role (inline message format).
				if itemType != "" && itemType != "message" {
					return true
				}

				role := item.Get("role").String()
				if itemType == "" && role == "" {
					return true
				}

				// Handle both string content and array content (multimodal).
				content := item.Get("content")
				var text string
				if content.Type == gjson.String {
					text = content.String()
				} else {
					text = extractResponsesAPIContent(content)
				}
				if text == "" {
					return true
				}

				switch role {
				case "developer", "system":
					if systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
					}
				case "user":
					if firstUserMsg == "" {
						firstUserMsg = truncateString(text, 100)
					}
				case "assistant":
					if firstAssistantMsg == "" {
						firstAssistantMsg = truncateString(text, 100)
					}
				}

				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	if systemPrompt == "" && firstUserMsg == "" {
		return "", ""
	}

	shortHash := computeSessionHash(systemPrompt, firstUserMsg, "")
	if firstAssistantMsg == "" {
		return shortHash, ""
	}

	fullHash := computeSessionHash(systemPrompt, firstUserMsg, firstAssistantMsg)
	return fullHash, shortHash
}

func computeSessionHash(systemPrompt, userMsg, assistantMsg string) string {
	h := fnv.New64a()
	if systemPrompt != "" {
		h.Write([]byte("sys:" + systemPrompt + "\n"))
	}
	if userMsg != "" {
		h.Write([]byte("usr:" + userMsg + "\n"))
	}
	if assistantMsg != "" {
		h.Write([]byte("ast:" + assistantMsg + "\n"))
	}
	return fmt.Sprintf("msg:%016x", h.Sum64())
}

func truncateString(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

// extractMessageContent extracts text content from a message content field.
// Handles both string content and array content (multimodal messages).
// For array content, extracts text from all text-type elements.
func extractMessageContent(content gjson.Result) string {
	// String content: "Hello world"
	if content.Type == gjson.String {
		return content.String()
	}

	// Array content: [{"type":"text","text":"Hello"},{"type":"image",...}]
	if content.IsArray() {
		var texts []string
		content.ForEach(func(_, part gjson.Result) bool {
			// Handle Claude format: {"type":"text","text":"content"}
			if part.Get("type").String() == "text" {
				if text := part.Get("text").String(); text != "" {
					texts = append(texts, text)
				}
			}
			// Handle OpenAI format: {"type":"text","text":"content"}
			// Same structure as Claude, already handled above
			return true
		})
		if len(texts) > 0 {
			return strings.Join(texts, " ")
		}
	}

	return ""
}

func extractResponsesAPIContent(content gjson.Result) string {
	if !content.IsArray() {
		return ""
	}
	var texts []string
	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		if partType == "input_text" || partType == "output_text" || partType == "text" {
			if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		}
		return true
	})
	if len(texts) > 0 {
		return strings.Join(texts, " ")
	}
	return ""
}

// extractSessionID is kept for backward compatibility.
// Deprecated: Use ExtractSessionID instead.
func extractSessionID(payload []byte) string {
	return ExtractSessionID(nil, payload, nil)
}
