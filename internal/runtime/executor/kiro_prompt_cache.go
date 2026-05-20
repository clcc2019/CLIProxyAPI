package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const (
	kiroPromptCacheDefaultTTL          = 5 * time.Minute
	kiroPromptCacheOneHourTTL          = time.Hour
	kiroPromptCacheDefaultMinTokens    = int64(1024)
	kiroPromptCacheOpusMinTokens       = int64(4096)
	kiroPromptCacheMaxRatio            = 0.85
	kiroPromptCacheMaxEntriesPerAuth   = 200
	kiroPromptCachePruneInterval       = time.Minute
	kiroPromptCacheDefaultAccountLabel = ""
)

var defaultKiroPromptCacheTracker = newKiroPromptCacheTracker(time.Now)

type kiroPromptCacheTracker struct {
	mu               sync.Mutex
	entriesByAccount map[string]map[string]kiroPromptCacheEntry
	now              func() time.Time
	lastPrune        time.Time
}

type kiroPromptCacheEntry struct {
	expiresAt time.Time
	ttl       time.Duration
}

type kiroPromptCacheBreakpoint struct {
	fingerprint      string
	cumulativeTokens int64
	ttl              time.Duration
}

type kiroPromptCacheProfile struct {
	breakpoints      []kiroPromptCacheBreakpoint
	totalInputTokens int64
	model            string
}

type kiroPromptCacheUsage struct {
	cacheCreationInputTokens int64
	cacheReadInputTokens     int64
	cacheCreation5mTokens    int64
	cacheCreation1hTokens    int64
}

type kiroPromptCachePlan struct {
	tracker   *kiroPromptCacheTracker
	accountID string
	profile   *kiroPromptCacheProfile
	usage     kiroPromptCacheUsage
	updated   bool
}

type kiroPromptCacheBlock struct {
	value        string
	tokens       int64
	ttl          time.Duration
	isMessageEnd bool
}

func newKiroPromptCacheTracker(now func() time.Time) *kiroPromptCacheTracker {
	if now == nil {
		now = time.Now
	}
	return &kiroPromptCacheTracker{
		entriesByAccount: make(map[string]map[string]kiroPromptCacheEntry),
		now:              now,
		lastPrune:        now(),
	}
}

func buildKiroPromptCachePlan(auth *cliproxyauth.Auth, prepared *kiroPreparedRequest, actualPayload []byte, model string) *kiroPromptCachePlan {
	if prepared == nil {
		return nil
	}
	accountID := strings.TrimSpace(kiroPromptCacheAccountID(auth))
	if accountID == "" {
		return nil
	}
	totalInputTokens := kiroEstimateInputTokensFromKiroRequest(kiroUsageRequestPayload(prepared, actualPayload))
	return defaultKiroPromptCacheTracker.plan(accountID, prepared.from, prepared.sourceBody, totalInputTokens, model)
}

func kiroPromptCacheAccountID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return kiroPromptCacheDefaultAccountLabel
	}
	return auth.ID
}

func (t *kiroPromptCacheTracker) plan(accountID string, sourceFormat sdktranslator.Format, sourceBody []byte, totalInputTokens int64, model string) *kiroPromptCachePlan {
	if t == nil || strings.TrimSpace(accountID) == "" {
		return nil
	}
	profile := buildKiroPromptCacheProfile(sourceFormat, sourceBody, totalInputTokens, model)
	if profile == nil || len(profile.breakpoints) == 0 {
		return nil
	}
	return &kiroPromptCachePlan{
		tracker:   t,
		accountID: strings.TrimSpace(accountID),
		profile:   profile,
		usage:     t.compute(strings.TrimSpace(accountID), profile),
	}
}

func applyKiroPromptCachePlan(detail *usage.Detail, plan *kiroPromptCachePlan) bool {
	if detail == nil || plan == nil {
		return false
	}
	changed := false
	if detail.CachedTokens == 0 && plan.usage.cacheReadInputTokens > 0 {
		detail.CachedTokens = plan.usage.cacheReadInputTokens
		changed = true
	}
	if detail.CacheCreationTokens == 0 && plan.usage.cacheCreationInputTokens > 0 {
		detail.CacheCreationTokens = plan.usage.cacheCreationInputTokens
		changed = true
	}
	minimumInput := detail.CachedTokens + detail.CacheCreationTokens
	if detail.InputTokens < minimumInput {
		detail.InputTokens = minimumInput
		changed = true
	}
	componentTotal := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	if componentTotal > 0 && detail.TotalTokens < componentTotal {
		detail.TotalTokens = componentTotal
		changed = true
	}
	return changed
}

func markKiroPromptCachePlanSuccess(plan *kiroPromptCachePlan) {
	if plan == nil || plan.updated || plan.tracker == nil || plan.profile == nil || strings.TrimSpace(plan.accountID) == "" {
		return
	}
	plan.updated = true
	plan.tracker.update(plan.accountID, plan.profile)
}

func kiroPromptCacheUsageLogKV(detail usage.Detail, plan *kiroPromptCachePlan) map[string]any {
	fields := map[string]any{
		"input_tokens":          detail.InputTokens,
		"cache_read_tokens":     detail.CachedTokens,
		"cache_creation_tokens": detail.CacheCreationTokens,
	}
	if plan != nil && plan.profile != nil {
		fields["cache_breakpoints"] = len(plan.profile.breakpoints)
		fields["simulated_cache_read_tokens"] = plan.usage.cacheReadInputTokens
		fields["simulated_cache_creation_tokens"] = plan.usage.cacheCreationInputTokens
	}
	return fields
}

func (t *kiroPromptCacheTracker) compute(accountID string, profile *kiroPromptCacheProfile) kiroPromptCacheUsage {
	if t == nil || profile == nil || len(profile.breakpoints) == 0 || strings.TrimSpace(accountID) == "" {
		return kiroPromptCacheUsage{}
	}
	last := profile.breakpoints[len(profile.breakpoints)-1]
	lastTokens := minKiroInt64(last.cumulativeTokens, profile.totalInputTokens)
	if lastTokens <= 0 {
		return kiroPromptCacheUsage{}
	}
	minTokens := kiroPromptCacheMinTokens(profile.model)
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)

	entries := t.entriesByAccount[strings.TrimSpace(accountID)]
	if len(entries) == 0 {
		effectiveCreation := int64(0)
		if lastTokens >= minTokens {
			effectiveCreation = lastTokens
		}
		cache5m, cache1h := kiroPromptCacheTTLBreakdown(profile, 0)
		if effectiveCreation == 0 {
			cache5m = 0
			cache1h = 0
		}
		return kiroPromptCacheUsage{
			cacheCreationInputTokens: effectiveCreation,
			cacheCreation5mTokens:    cache5m,
			cacheCreation1hTokens:    cache1h,
		}
	}

	maxCacheable := int64(math.Floor(float64(profile.totalInputTokens) * kiroPromptCacheMaxRatio))
	if maxCacheable > 0 && lastTokens > maxCacheable {
		lastTokens = maxCacheable
	}

	matchedTokens := int64(0)
	for i := len(profile.breakpoints) - 1; i >= 0; i-- {
		bp := profile.breakpoints[i]
		if bp.cumulativeTokens < minTokens {
			continue
		}
		entry, ok := entries[bp.fingerprint]
		if !ok || entry.expiresAt.Before(now) {
			continue
		}
		entry.expiresAt = now.Add(entry.ttl)
		entries[bp.fingerprint] = entry
		matchedTokens = minKiroInt64(bp.cumulativeTokens, profile.totalInputTokens)
		if matchedTokens > lastTokens {
			matchedTokens = lastTokens
		}
		break
	}

	creation := lastTokens - matchedTokens
	if creation < 0 {
		creation = 0
	}
	cache5m, cache1h := kiroPromptCacheTTLBreakdown(profile, matchedTokens)
	return kiroPromptCacheUsage{
		cacheCreationInputTokens: creation,
		cacheReadInputTokens:     matchedTokens,
		cacheCreation5mTokens:    cache5m,
		cacheCreation1hTokens:    cache1h,
	}
}

func (t *kiroPromptCacheTracker) update(accountID string, profile *kiroPromptCacheProfile) {
	if t == nil || profile == nil || len(profile.breakpoints) == 0 || strings.TrimSpace(accountID) == "" {
		return
	}
	accountID = strings.TrimSpace(accountID)
	minTokens := kiroPromptCacheMinTokens(profile.model)
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()

	entries := t.entriesByAccount[accountID]
	if entries == nil {
		entries = make(map[string]kiroPromptCacheEntry)
		t.entriesByAccount[accountID] = entries
	}
	for _, bp := range profile.breakpoints {
		if bp.cumulativeTokens < minTokens {
			continue
		}
		entries[bp.fingerprint] = kiroPromptCacheEntry{
			expiresAt: now.Add(bp.ttl),
			ttl:       bp.ttl,
		}
	}
	t.limitEntriesLocked(accountID, entries)
}

func (t *kiroPromptCacheTracker) clear() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, entries := range t.entriesByAccount {
		count += len(entries)
	}
	t.entriesByAccount = make(map[string]map[string]kiroPromptCacheEntry)
	return count
}

func (t *kiroPromptCacheTracker) pruneLocked(now time.Time) {
	if t == nil {
		return
	}
	if !t.lastPrune.IsZero() && now.Sub(t.lastPrune) < kiroPromptCachePruneInterval {
		return
	}
	t.lastPrune = now
	for accountID, entries := range t.entriesByAccount {
		for fingerprint, entry := range entries {
			if entry.expiresAt.Before(now) {
				delete(entries, fingerprint)
			}
		}
		if len(entries) == 0 {
			delete(t.entriesByAccount, accountID)
		}
	}
}

func (t *kiroPromptCacheTracker) limitEntriesLocked(accountID string, entries map[string]kiroPromptCacheEntry) {
	if len(entries) <= kiroPromptCacheMaxEntriesPerAuth {
		return
	}
	type kv struct {
		key   string
		entry kiroPromptCacheEntry
	}
	items := make([]kv, 0, len(entries))
	for key, entry := range entries {
		items = append(items, kv{key: key, entry: entry})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].entry.expiresAt.Before(items[j].entry.expiresAt)
	})
	for i := 0; i < len(items)-kiroPromptCacheMaxEntriesPerAuth; i++ {
		delete(entries, items[i].key)
	}
	if len(entries) == 0 {
		delete(t.entriesByAccount, accountID)
	}
}

func buildKiroPromptCacheProfile(sourceFormat sdktranslator.Format, sourceBody []byte, totalInputTokens int64, model string) *kiroPromptCacheProfile {
	root, ok := decodeKiroPromptCacheJSON(sourceBody)
	if !ok {
		return nil
	}
	var blocks []kiroPromptCacheBlock
	switch strings.ToLower(strings.TrimSpace(sourceFormat.String())) {
	case sdktranslator.FormatOpenAI.String():
		blocks = flattenKiroOpenAIPromptCacheBlocks(root)
	case sdktranslator.FormatKiro.String():
		return nil
	default:
		blocks = flattenKiroClaudePromptCacheBlocks(root)
	}
	if len(blocks) == 0 {
		return nil
	}

	var hashInput strings.Builder
	breakpoints := make([]kiroPromptCacheBreakpoint, 0, len(blocks))
	cumulativeTokens := int64(0)
	activeTTL := time.Duration(0)
	for _, block := range blocks {
		kiroPromptCacheAppendHashChunk(&hashInput, block.value)
		if block.tokens > 0 {
			cumulativeTokens += block.tokens
		}

		breakpointTTL := time.Duration(0)
		if block.ttl > 0 {
			breakpointTTL = block.ttl
			activeTTL = block.ttl
		} else if block.isMessageEnd && activeTTL > 0 {
			breakpointTTL = activeTTL
		}
		if breakpointTTL <= 0 {
			continue
		}
		breakpoints = append(breakpoints, kiroPromptCacheBreakpoint{
			fingerprint:      kiroPromptCacheFingerprint(hashInput.String()),
			cumulativeTokens: cumulativeTokens,
			ttl:              breakpointTTL,
		})
	}
	if len(breakpoints) == 0 {
		return nil
	}
	if totalInputTokens < cumulativeTokens {
		totalInputTokens = cumulativeTokens
	}
	return &kiroPromptCacheProfile{
		breakpoints:      breakpoints,
		totalInputTokens: totalInputTokens,
		model:            model,
	}
}

func decodeKiroPromptCacheJSON(body []byte) (map[string]any, bool) {
	if len(body) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil || len(root) == 0 {
		return nil, false
	}
	return root, true
}

func flattenKiroClaudePromptCacheBlocks(root map[string]any) []kiroPromptCacheBlock {
	var blocks []kiroPromptCacheBlock
	appendKiroClaudeToolCacheBlocks(&blocks, root["tools"])
	appendKiroClaudeSystemCacheBlocks(&blocks, root["system"])
	appendKiroPromptMessageCacheBlocks(&blocks, root["messages"])
	return blocks
}

func flattenKiroOpenAIPromptCacheBlocks(root map[string]any) []kiroPromptCacheBlock {
	var blocks []kiroPromptCacheBlock
	appendKiroOpenAIToolCacheBlocks(&blocks, root["tools"])
	appendKiroPromptMessageCacheBlocks(&blocks, root["messages"])
	return blocks
}

func appendKiroClaudeToolCacheBlocks(blocks *[]kiroPromptCacheBlock, raw any) {
	items, ok := raw.([]any)
	if !ok || blocks == nil {
		return
	}
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		value := kiroPromptCacheCanonical(map[string]any{
			"kind":         "tool",
			"name":         tool["name"],
			"description":  tool["description"],
			"input_schema": tool["input_schema"],
		})
		if value == "" {
			continue
		}
		*blocks = append(*blocks, kiroPromptCacheBlock{
			value:  value,
			tokens: kiroCountText(value),
			ttl:    kiroPromptCacheTTL(tool),
		})
	}
}

func appendKiroOpenAIToolCacheBlocks(blocks *[]kiroPromptCacheBlock, raw any) {
	items, ok := raw.([]any)
	if !ok || blocks == nil {
		return
	}
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		canonicalTool := map[string]any{
			"kind": "tool",
			"type": tool["type"],
		}
		if len(fn) > 0 {
			canonicalTool["name"] = fn["name"]
			canonicalTool["description"] = fn["description"]
			canonicalTool["input_schema"] = firstKiroPromptCacheValue(fn, "parameters", "input_schema")
		} else {
			canonicalTool["tool"] = tool
		}
		value := kiroPromptCacheCanonical(canonicalTool)
		if value == "" {
			continue
		}
		ttl := kiroPromptCacheTTL(tool)
		if ttl == 0 {
			ttl = kiroPromptCacheTTL(fn)
		}
		*blocks = append(*blocks, kiroPromptCacheBlock{
			value:  value,
			tokens: kiroCountText(value),
			ttl:    ttl,
		})
	}
}

func appendKiroClaudeSystemCacheBlocks(blocks *[]kiroPromptCacheBlock, raw any) {
	if blocks == nil || raw == nil {
		return
	}
	switch system := raw.(type) {
	case string:
		value := kiroPromptCacheCanonical(map[string]any{
			"kind": "system",
			"type": "text",
			"text": system,
		})
		if value != "" {
			*blocks = append(*blocks, kiroPromptCacheBlock{value: value, tokens: kiroCountText(system)})
		}
	case []any:
		for _, item := range system {
			block := kiroPromptCacheNormalizeContentBlock(item)
			value := kiroPromptCacheCanonical(map[string]any{
				"kind":  "system",
				"block": block,
			})
			if value == "" {
				continue
			}
			blockMap, _ := block.(map[string]any)
			*blocks = append(*blocks, kiroPromptCacheBlock{
				value:  value,
				tokens: kiroPromptCacheTokenCountForContentBlock(block),
				ttl:    kiroPromptCacheTTL(blockMap),
			})
		}
	}
}

func appendKiroPromptMessageCacheBlocks(blocks *[]kiroPromptCacheBlock, raw any) {
	items, ok := raw.([]any)
	if !ok || blocks == nil {
		return
	}
	for messageIndex, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		appendKiroPromptMessageCacheBlock(blocks, msg, messageIndex)
	}
}

func appendKiroPromptMessageCacheBlock(blocks *[]kiroPromptCacheBlock, msg map[string]any, messageIndex int) {
	if blocks == nil || len(msg) == 0 {
		return
	}
	role := kiroPromptCacheString(msg["role"])
	messageTTL := kiroPromptCacheTTL(msg)
	content, ok := msg["content"]
	if !ok || content == nil {
		return
	}
	switch value := content.(type) {
	case string:
		canonical := kiroPromptCacheCanonical(map[string]any{
			"kind":  "message",
			"role":  role,
			"index": messageIndex,
			"type":  "text",
			"text":  value,
		})
		if canonical != "" {
			*blocks = append(*blocks, kiroPromptCacheBlock{
				value:        canonical,
				tokens:       kiroCountText(value),
				ttl:          messageTTL,
				isMessageEnd: true,
			})
		}
	case []any:
		lastIndex := len(value) - 1
		for blockIndex, item := range value {
			block := kiroPromptCacheNormalizeContentBlock(item)
			canonical := kiroPromptCacheCanonical(map[string]any{
				"kind":       "message",
				"role":       role,
				"index":      messageIndex,
				"blockIndex": blockIndex,
				"block":      block,
			})
			if canonical == "" {
				continue
			}
			blockMap, _ := block.(map[string]any)
			ttl := kiroPromptCacheTTL(blockMap)
			if ttl == 0 && blockIndex == lastIndex {
				ttl = messageTTL
			}
			*blocks = append(*blocks, kiroPromptCacheBlock{
				value:        canonical,
				tokens:       kiroPromptCacheTokenCountForContentBlock(block),
				ttl:          ttl,
				isMessageEnd: blockIndex == lastIndex,
			})
		}
	}
}

func kiroPromptCacheNormalizeContentBlock(value any) any {
	switch typed := value.(type) {
	case string:
		return map[string]any{"type": "text", "text": typed}
	case map[string]any:
		return typed
	default:
		return typed
	}
}

func kiroPromptCacheTokenCountForContentBlock(value any) int64 {
	switch block := value.(type) {
	case string:
		return kiroCountText(block)
	case map[string]any:
		if text := firstKiroPromptCacheString(block, "text", "thinking", "content"); text != "" {
			return kiroCountText(text)
		}
	}
	canonical := kiroPromptCacheCanonical(value)
	if canonical == "" {
		return 0
	}
	return kiroCountText(canonical)
}

func kiroPromptCacheTTL(record map[string]any) time.Duration {
	if len(record) == 0 {
		return 0
	}
	cacheControl, _ := firstKiroPromptCacheMap(record, "cache_control", "cacheControl")
	if len(cacheControl) == 0 {
		return 0
	}
	if !strings.EqualFold(strings.TrimSpace(kiroPromptCacheString(cacheControl["type"])), "ephemeral") {
		return 0
	}
	return kiroPromptCacheTTLValue(cacheControl["ttl"])
}

func kiroPromptCacheTTLValue(value any) time.Duration {
	switch ttl := value.(type) {
	case nil:
		return kiroPromptCacheDefaultTTL
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(ttl))
		if trimmed == "" {
			return kiroPromptCacheDefaultTTL
		}
		if trimmed == "1h" || trimmed == "60m" {
			return kiroPromptCacheOneHourTTL
		}
		if parsed, err := time.ParseDuration(trimmed); err == nil && parsed > 0 {
			return parsed
		}
		if seconds, err := strconv.ParseFloat(trimmed, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	case json.Number:
		if seconds, err := ttl.Float64(); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	case float64:
		if ttl > 0 {
			return time.Duration(ttl * float64(time.Second))
		}
	case int:
		if ttl > 0 {
			return time.Duration(ttl) * time.Second
		}
	case int64:
		if ttl > 0 {
			return time.Duration(ttl) * time.Second
		}
	}
	return kiroPromptCacheDefaultTTL
}

func kiroPromptCacheTTLBreakdown(profile *kiroPromptCacheProfile, matchedTokens int64) (int64, int64) {
	if profile == nil {
		return 0, 0
	}
	cache5m := int64(0)
	cache1h := int64(0)
	previous := matchedTokens
	for _, bp := range profile.breakpoints {
		current := minKiroInt64(bp.cumulativeTokens, profile.totalInputTokens)
		if current <= previous {
			continue
		}
		delta := current - previous
		if bp.ttl >= kiroPromptCacheOneHourTTL {
			cache1h += delta
		} else {
			cache5m += delta
		}
		previous = current
	}
	return cache5m, cache1h
}

func kiroPromptCacheMinTokens(model string) int64 {
	if strings.Contains(strings.ToLower(model), "opus") {
		return kiroPromptCacheOpusMinTokens
	}
	return kiroPromptCacheDefaultMinTokens
}

func kiroPromptCacheAppendHashChunk(dst *strings.Builder, chunk string) {
	if dst == nil {
		return
	}
	dst.WriteString(strconv.Itoa(len(chunk)))
	dst.WriteByte(0)
	dst.WriteString(chunk)
	dst.WriteByte(0)
}

func kiroPromptCacheFingerprint(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

func kiroPromptCacheCanonical(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

func firstKiroPromptCacheValue(record map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := record[key]; ok {
			return value
		}
	}
	return nil
}

func firstKiroPromptCacheString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := record[key]; ok {
			if text := kiroPromptCacheString(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func firstKiroPromptCacheMap(record map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		value, ok := record[key]
		if !ok {
			continue
		}
		m, ok := value.(map[string]any)
		if ok {
			return m, true
		}
	}
	return nil, false
}

func kiroPromptCacheString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func minKiroInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
