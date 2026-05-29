package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	websocketToolOutputCacheMaxPerSession = 256
	websocketToolOutputCacheMaxItemBytes  = 256 << 10
	websocketToolOutputCacheMaxBytes      = 2 << 20
	websocketToolOutputCacheTTL           = 10 * time.Minute
	websocketToolCallCacheMaxPerSession   = 256
	websocketToolCallCacheMaxItemBytes    = 128 << 10
	websocketToolCallCacheMaxBytes        = 1 << 20
)

var defaultWebsocketToolOutputCache = newWebsocketToolOutputCache(websocketToolOutputCacheTTL, websocketToolOutputCacheMaxPerSession)
var defaultWebsocketToolCallCache = newWebsocketToolOutputCacheWithLimits(websocketToolOutputCacheTTL, websocketToolCallCacheMaxPerSession, websocketToolCallCacheMaxItemBytes, websocketToolCallCacheMaxBytes)
var defaultWebsocketToolSessionRefs = newWebsocketToolSessionRefCounter()
var defaultWebsocketToolCachesMu sync.RWMutex

var responsesWebsocketToolRepairMarkers = []string{
	`"function_call"`,
	`"function_call_output"`,
	`"custom_tool_call"`,
	`"custom_tool_call_output"`,
	`"local_shell_call"`,
	`"tool_search_call"`,
	`"tool_search_output"`,
}

func currentDefaultWebsocketToolCaches() (*websocketToolOutputCache, *websocketToolOutputCache, *websocketToolSessionRefCounter) {
	defaultWebsocketToolCachesMu.RLock()
	defer defaultWebsocketToolCachesMu.RUnlock()
	return defaultWebsocketToolOutputCache, defaultWebsocketToolCallCache, defaultWebsocketToolSessionRefs
}

type websocketToolOutputCache struct {
	mu            sync.Mutex
	ttl           time.Duration
	maxPerSession int
	maxItemBytes  int
	maxBytes      int
	lastCleanup   time.Time
	sessions      map[string]*websocketToolOutputSession
}

type websocketToolOutputSession struct {
	lastSeen time.Time
	outputs  map[string]json.RawMessage
	order    []string
	bytes    int
}

func newWebsocketToolOutputCache(ttl time.Duration, maxPerSession int) *websocketToolOutputCache {
	return newWebsocketToolOutputCacheWithLimits(ttl, maxPerSession, websocketToolOutputCacheMaxItemBytes, websocketToolOutputCacheMaxBytes)
}

func newWebsocketToolOutputCacheWithLimits(ttl time.Duration, maxPerSession int, maxItemBytes int, maxBytes int) *websocketToolOutputCache {
	if ttl <= 0 {
		ttl = websocketToolOutputCacheTTL
	}
	if maxPerSession <= 0 {
		maxPerSession = websocketToolOutputCacheMaxPerSession
	}
	if maxItemBytes <= 0 {
		maxItemBytes = websocketToolOutputCacheMaxItemBytes
	}
	if maxBytes <= 0 {
		maxBytes = websocketToolOutputCacheMaxBytes
	}
	return &websocketToolOutputCache{
		ttl:           ttl,
		maxPerSession: maxPerSession,
		maxItemBytes:  maxItemBytes,
		maxBytes:      maxBytes,
		sessions:      make(map[string]*websocketToolOutputSession),
	}
}

func (c *websocketToolOutputCache) record(sessionKey string, callID string, item json.RawMessage) {
	sessionKey = strings.TrimSpace(sessionKey)
	callID = strings.TrimSpace(callID)
	if sessionKey == "" || callID == "" || c == nil {
		return
	}
	oversized := len(item) > c.maxItemBytes

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupLocked(now)

	session, ok := c.sessions[sessionKey]
	if ok && c.sessionExpired(session, now) {
		delete(c.sessions, sessionKey)
		session = nil
		ok = false
	}
	if oversized && (!ok || session == nil) {
		return
	}
	if !ok || session == nil {
		session = &websocketToolOutputSession{
			lastSeen: now,
			outputs:  make(map[string]json.RawMessage),
		}
		c.sessions[sessionKey] = session
	}
	session.lastSeen = now
	if oversized {
		session.remove(callID)
		return
	}

	if _, exists := session.outputs[callID]; !exists {
		session.order = append(session.order, callID)
	} else {
		session.bytes -= len(session.outputs[callID])
	}
	session.outputs[callID] = append(json.RawMessage(nil), item...)
	session.bytes += len(item)

	for len(session.order) > c.maxPerSession || session.bytes > c.maxBytes {
		evict := session.order[0]
		session.order = session.order[1:]
		if previous, ok := session.outputs[evict]; ok {
			session.bytes -= len(previous)
		}
		delete(session.outputs, evict)
	}
	if session.bytes < 0 {
		session.bytes = 0
	}
}

func (s *websocketToolOutputSession) remove(callID string) {
	if s == nil || callID == "" || len(s.outputs) == 0 {
		return
	}
	if previous, ok := s.outputs[callID]; ok {
		s.bytes -= len(previous)
		delete(s.outputs, callID)
	}
	for i, existing := range s.order {
		if existing != callID {
			continue
		}
		copy(s.order[i:], s.order[i+1:])
		s.order = s.order[:len(s.order)-1]
		break
	}
	if s.bytes < 0 {
		s.bytes = 0
	}
}

func (c *websocketToolOutputCache) get(sessionKey string, callID string) (json.RawMessage, bool) {
	sessionKey = strings.TrimSpace(sessionKey)
	callID = strings.TrimSpace(callID)
	if sessionKey == "" || callID == "" || c == nil {
		return nil, false
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupLocked(now)

	session, ok := c.sessions[sessionKey]
	if !ok || session == nil {
		return nil, false
	}
	if c.sessionExpired(session, now) {
		delete(c.sessions, sessionKey)
		return nil, false
	}
	session.lastSeen = now
	item, ok := session.outputs[callID]
	if !ok || len(item) == 0 {
		return nil, false
	}
	return append(json.RawMessage(nil), item...), true
}

func (c *websocketToolOutputCache) cleanupLocked(now time.Time) {
	if c == nil || c.ttl <= 0 {
		return
	}
	interval := c.cleanupInterval()
	if !c.lastCleanup.IsZero() && now.Sub(c.lastCleanup) < interval {
		return
	}
	c.lastCleanup = now

	for key, session := range c.sessions {
		if c.sessionExpired(session, now) {
			delete(c.sessions, key)
		}
	}
}

func (c *websocketToolOutputCache) cleanupInterval() time.Duration {
	if c == nil || c.ttl <= 0 {
		return time.Minute
	}
	interval := c.ttl / 4
	if interval < time.Second {
		return time.Second
	}
	if interval > time.Minute {
		return time.Minute
	}
	return interval
}

func (c *websocketToolOutputCache) sessionExpired(session *websocketToolOutputSession, now time.Time) bool {
	if c == nil || session == nil {
		return true
	}
	return c.ttl > 0 && now.Sub(session.lastSeen) > c.ttl
}

func (c *websocketToolOutputCache) deleteSession(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" || c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.sessions, sessionKey)
}

func websocketDownstreamSessionKey(req *http.Request) string {
	if req == nil {
		return ""
	}
	for _, key := range []string{"Thread_id", "thread-id", "X-Thread-ID"} {
		if threadID := strings.TrimSpace(req.Header.Get(key)); threadID != "" {
			return threadID
		}
	}
	if conversationID := strings.TrimSpace(req.Header.Get("Conversation_id")); conversationID != "" {
		return conversationID
	}
	if raw := strings.TrimSpace(req.Header.Get("X-Codex-Turn-Metadata")); raw != "" {
		if threadID := strings.TrimSpace(gjson.Get(raw, "thread_id").String()); threadID != "" {
			return threadID
		}
		if sessionID := strings.TrimSpace(gjson.Get(raw, "session_id").String()); sessionID != "" {
			return sessionID
		}
	}
	for _, key := range []string{"Session_id", "session-id", "X-Session-ID"} {
		if sessionID := strings.TrimSpace(req.Header.Get(key)); sessionID != "" {
			return sessionID
		}
	}
	if requestID := strings.TrimSpace(req.Header.Get("X-Client-Request-Id")); requestID != "" {
		return requestID
	}
	return ""
}

type websocketToolSessionRefCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newWebsocketToolSessionRefCounter() *websocketToolSessionRefCounter {
	return &websocketToolSessionRefCounter{counts: make(map[string]int)}
}

func (c *websocketToolSessionRefCounter) acquire(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" || c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.counts[sessionKey]++
}

func (c *websocketToolSessionRefCounter) release(sessionKey string) bool {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" || c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	count := c.counts[sessionKey]
	if count <= 1 {
		delete(c.counts, sessionKey)
		return true
	}
	c.counts[sessionKey] = count - 1
	return false
}

func retainResponsesWebsocketToolCaches(sessionKey string) {
	_, _, refs := currentDefaultWebsocketToolCaches()
	if refs == nil {
		return
	}
	refs.acquire(sessionKey)
}

func releaseResponsesWebsocketToolCaches(sessionKey string) {
	outputCache, callCache, refs := currentDefaultWebsocketToolCaches()
	if refs == nil {
		return
	}
	if !refs.release(sessionKey) {
		return
	}

	if outputCache != nil {
		outputCache.deleteSession(sessionKey)
	}
	if callCache != nil {
		callCache.deleteSession(sessionKey)
	}
}

func resetResponsesWebsocketToolCaches(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	outputCache, callCache, _ := currentDefaultWebsocketToolCaches()
	if outputCache != nil {
		outputCache.deleteSession(sessionKey)
	}
	if callCache != nil {
		callCache.deleteSession(sessionKey)
	}
}

func repairResponsesWebsocketToolCalls(sessionKey string, payload []byte) []byte {
	outputCache, callCache, _ := currentDefaultWebsocketToolCaches()
	return repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, sessionKey, payload)
}

func repairResponsesWebsocketToolCallsWithCache(cache *websocketToolOutputCache, sessionKey string, payload []byte) []byte {
	return repairResponsesWebsocketToolCallsWithCaches(cache, nil, sessionKey, payload)
}

func repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache *websocketToolOutputCache, sessionKey string, payload []byte) []byte {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" || outputCache == nil || len(payload) == 0 {
		return payload
	}

	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return payload
	}
	if !rawStringContainsAny(input.Raw, responsesWebsocketToolRepairMarkers) {
		return payload
	}

	allowOrphanOutputs := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != ""
	updatedRaw, errRepair := repairResponsesToolCallsArray(outputCache, callCache, sessionKey, input.Raw, allowOrphanOutputs)
	if errRepair != nil || updatedRaw == "" || updatedRaw == input.Raw {
		return payload
	}

	updated, errSet := sjson.SetRawBytes(payload, "input", []byte(updatedRaw))
	if errSet != nil {
		return payload
	}
	return updated
}

func rawStringContainsAny(raw string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(raw, marker) {
			return true
		}
	}
	return false
}

func repairResponsesToolCallsArray(outputCache, callCache *websocketToolOutputCache, sessionKey string, rawArray string, allowOrphanOutputs bool) (string, error) {
	rawArray, err := validatedJSONArrayRawString(rawArray)
	if err != nil {
		return "", err
	}

	result := gjson.Parse(rawArray)
	itemCount := int(result.Get("#").Int())
	type repairItemMeta struct {
		raw      string
		callID   string
		itemType string
		isCall   bool
		isOutput bool
	}
	metas := make([]repairItemMeta, 0, itemCount)

	// First pass: remember typed call/output pairs in this payload. Outputs
	// are recorded into the session cache only after the second pass confirms
	// they are kept; otherwise dropped orphan outputs could poison later turns.
	outputPresent := make(map[string]struct{}, itemCount)
	callPresent := make(map[string]string, itemCount)
	result.ForEach(func(_, item gjson.Result) bool {
		itemRaw := strings.TrimSpace(item.Raw)
		if itemRaw == "" {
			return true
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		meta := repairItemMeta{
			raw:      itemRaw,
			callID:   strings.TrimSpace(item.Get("call_id").String()),
			itemType: itemType,
			isCall:   isResponsesToolCallType(itemType),
			isOutput: isResponsesToolCallOutputType(itemType),
		}
		metas = append(metas, meta)
		switch {
		case meta.isOutput:
			if meta.callID == "" {
				return true
			}
			outputPresent[toolOutputPresenceKey(meta.itemType, meta.callID)] = struct{}{}
		case meta.isCall:
			if meta.callID == "" {
				return true
			}
			callPresent[meta.callID] = meta.itemType
			if callCache != nil {
				callCache.record(sessionKey, meta.callID, json.RawMessage(itemRaw))
			}
		}
		return true
	})

	filtered := make([]string, 0, len(metas))
	appendFiltered := func(meta repairItemMeta, raw string) {
		filtered = append(filtered, raw)
		if meta.isOutput && meta.callID != "" {
			outputCache.record(sessionKey, meta.callID, json.RawMessage(raw))
		}
	}
	insertedCalls := make(map[string]struct{}, itemCount)
	changed := false
	for _, meta := range metas {
		item := meta.raw
		if meta.isOutput {
			callID := meta.callID
			if meta.itemType == "tool_search_output" && (callID == "" || strings.EqualFold(strings.TrimSpace(gjson.Get(meta.raw, "execution").String()), "server")) {
				appendFiltered(meta, item)
				continue
			}
			if callID == "" {
				// Upstream rejects ordinary client-side outputs without a call_id;
				// tool-search outputs were handled above because Codex allows
				// call_id-less search results to stand alone.
				changed = true
				continue
			}

			if callType, ok := callPresent[callID]; ok {
				if toolOutputTypeMatchesCallType(meta.itemType, callType) {
					appendFiltered(meta, item)
					continue
				}
				changed = true
				continue
			}

			if allowOrphanOutputs {
				appendFiltered(meta, item)
				continue
			}

			if callCache != nil {
				if cached, ok := callCache.get(sessionKey, callID); ok {
					cachedCallType := strings.TrimSpace(gjson.GetBytes(cached, "type").String())
					if !toolOutputTypeMatchesCallType(meta.itemType, cachedCallType) {
						changed = true
						continue
					}
					if _, already := insertedCalls[callID]; !already {
						filtered = append(filtered, string(cached))
						insertedCalls[callID] = struct{}{}
						callPresent[callID] = cachedCallType
						changed = true
					}
					appendFiltered(meta, item)
					continue
				}
			}

			// Drop orphaned function_call_output items; upstream rejects transcripts with missing calls.
			changed = true
			continue
		}
		if !meta.isCall {
			filtered = append(filtered, item)
			continue
		}

		callID := meta.callID
		if callID == "" {
			// Upstream rejects tool calls without a call_id; drop it.
			if meta.itemType == "local_shell_call" || meta.itemType == "tool_search_call" {
				filtered = append(filtered, item)
				continue
			}
			changed = true
			continue
		}

		expectedOutputType := toolOutputTypeForCallType(meta.itemType)
		if _, ok := outputPresent[toolOutputPresenceKey(expectedOutputType, callID)]; ok {
			filtered = append(filtered, item)
			continue
		}

		if allowOrphanOutputs {
			filtered = append(filtered, item)
			continue
		}

		if cached, ok := outputCache.get(sessionKey, callID); ok {
			cachedOutputType := strings.TrimSpace(gjson.GetBytes(cached, "type").String())
			if !toolOutputTypeMatchesCallType(cachedOutputType, meta.itemType) {
				changed = true
			} else {
				filtered = append(filtered, item)
				filtered = append(filtered, string(cached))
				outputPresent[toolOutputPresenceKey(cachedOutputType, callID)] = struct{}{}
				changed = true
				continue
			}
		}

		if missingOutput := buildResponsesMissingOutputForCall(meta.itemType, callID); len(missingOutput) > 0 {
			filtered = append(filtered, item)
			filtered = append(filtered, string(missingOutput))
			outputPresent[toolOutputPresenceKey(expectedOutputType, callID)] = struct{}{}
			changed = true
			continue
		}

		// Drop orphaned function_call items; upstream rejects transcripts with missing outputs.
		changed = true
	}

	if !changed {
		return rawArray, nil
	}
	return joinJSONArrayRaw(filtered), nil
}

func buildResponsesMissingOutputForCall(callType string, callID string) json.RawMessage {
	switch strings.TrimSpace(callType) {
	case "function_call":
		return buildResponsesFunctionCallMissingOutput(callID)
	case "local_shell_call":
		return buildResponsesFunctionCallMissingOutput(callID)
	case "custom_tool_call":
		return buildResponsesCustomToolMissingOutput(callID)
	case "tool_search_call":
		return buildResponsesToolSearchMissingOutput(callID)
	default:
		return nil
	}
}

func buildResponsesFunctionCallMissingOutput(callID string) json.RawMessage {
	payload := []byte(`{"type":"function_call_output","call_id":"","output":"aborted"}`)
	payload, _ = sjson.SetBytes(payload, "call_id", callID)
	return payload
}

func buildResponsesCustomToolMissingOutput(callID string) json.RawMessage {
	payload := []byte(`{"type":"custom_tool_call_output","call_id":"","output":"aborted"}`)
	payload, _ = sjson.SetBytes(payload, "call_id", callID)
	return payload
}

func toolOutputPresenceKey(outputType string, callID string) string {
	return strings.TrimSpace(outputType) + "\x00" + strings.TrimSpace(callID)
}

func toolOutputTypeForCallType(callType string) string {
	switch strings.TrimSpace(callType) {
	case "function_call", "local_shell_call":
		return "function_call_output"
	case "custom_tool_call":
		return "custom_tool_call_output"
	case "tool_search_call":
		return "tool_search_output"
	default:
		return ""
	}
}

func toolOutputTypeMatchesCallType(outputType string, callType string) bool {
	return strings.TrimSpace(outputType) != "" &&
		strings.TrimSpace(outputType) == toolOutputTypeForCallType(callType)
}

func buildResponsesToolSearchMissingOutput(callID string) json.RawMessage {
	payload := []byte(`{"type":"tool_search_output","call_id":"","status":"completed","execution":"client","tools":[]}`)
	payload, _ = sjson.SetBytes(payload, "call_id", callID)
	return payload
}

func recordResponsesWebsocketToolCallsFromPayload(sessionKey string, payload []byte) {
	_, callCache, _ := currentDefaultWebsocketToolCaches()
	recordResponsesWebsocketToolCallsFromPayloadWithCache(callCache, sessionKey, payload)
}

func recordResponsesWebsocketToolCallsFromPayloadWithCache(cache *websocketToolOutputCache, sessionKey string, payload []byte) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" || cache == nil || len(payload) == 0 {
		return
	}

	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	switch eventType {
	case "response.completed":
		output := gjson.GetBytes(payload, "response.output")
		if !output.Exists() || !output.IsArray() {
			return
		}
		output.ForEach(func(_, item gjson.Result) bool {
			if !isResponsesToolCallType(item.Get("type").String()) {
				return true
			}
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID == "" {
				return true
			}
			cache.record(sessionKey, callID, json.RawMessage(item.Raw))
			return true
		})
	case "response.output_item.added", "response.output_item.done":
		item := gjson.GetBytes(payload, "item")
		if !item.Exists() || !item.IsObject() {
			return
		}
		if !isResponsesToolCallType(item.Get("type").String()) {
			return
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			return
		}
		cache.record(sessionKey, callID, json.RawMessage(item.Raw))
	}
}

func isResponsesToolCallType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call":
		return true
	default:
		return false
	}
}

func isResponsesToolCallOutputType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		return true
	default:
		return false
	}
}
