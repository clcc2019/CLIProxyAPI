package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const kiroMaxGeneratePayloadBytes = 600 << 10

type kiroPayloadPrepareStats struct {
	OriginalBytes          int
	FinalBytes             int
	OriginalHistoryEntries int
	FinalHistoryEntries    int
	NormalizedHistory      bool
	StrippedToolContext    bool
	RepairedToolResults    bool
	TrimmedHistory         bool
	Compacted              bool
}

func (s kiroPayloadPrepareStats) changed() bool {
	return s.NormalizedHistory || s.StrippedToolContext || s.RepairedToolResults || s.TrimmedHistory || s.Compacted
}

func prepareKiroPayloadForUpstream(payload []byte) ([]byte, kiroPayloadPrepareStats, error) {
	return prepareKiroPayloadForUpstreamWithLimit(payload, kiroMaxGeneratePayloadBytes)
}

func prepareKiroPayloadForUpstreamWithLimit(payload []byte, maxBytes int) ([]byte, kiroPayloadPrepareStats, error) {
	stats := kiroPayloadPrepareStats{OriginalBytes: len(payload), FinalBytes: len(payload)}

	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return payload, stats, nil
	}

	stats.OriginalHistoryEntries = kiroPayloadHistoryLen(root)
	if normalizeKiroPayloadHistoryShape(root) {
		stats.NormalizedHistory = true
	}
	if stripKiroToolContextWithoutTools(root) {
		stats.StrippedToolContext = true
	}
	if repairKiroOrphanedToolResults(root) {
		stats.RepairedToolResults = true
	}
	if stripKiroEmptyToolUses(root) {
		stats.NormalizedHistory = true
	}
	if maxBytes > 0 && trimKiroPayloadHistoryToLimit(root, maxBytes) {
		stats.TrimmedHistory = true
	}

	prepared, err := json.Marshal(root)
	if err != nil {
		return payload, stats, nil
	}
	stats.FinalBytes = len(prepared)
	stats.FinalHistoryEntries = kiroPayloadHistoryLen(root)
	if maxBytes > 0 && len(payload) > maxBytes && len(prepared) < len(payload) {
		stats.Compacted = true
	}

	if maxBytes > 0 && len(prepared) > maxBytes {
		return prepared, stats, statusErr{
			code: http.StatusBadRequest,
			msg:  fmt.Sprintf("kiro: payload exceeds upstream size limit after trimming history: %d bytes > %d bytes", len(prepared), maxBytes),
		}
	}

	if !stats.changed() {
		stats.FinalBytes = len(payload)
		return payload, stats, nil
	}
	return prepared, stats, nil
}

func kiroPayloadHistoryLen(root map[string]any) int {
	state := kiroObject(root, "conversationState")
	if state == nil {
		return 0
	}
	history, _ := state["history"].([]any)
	return len(history)
}

func normalizeKiroPayloadHistoryShape(root map[string]any) bool {
	state := kiroObject(root, "conversationState")
	if state == nil {
		return false
	}
	history, ok := state["history"].([]any)
	if !ok || len(history) == 0 {
		return false
	}

	currentUser := kiroObject(kiroObject(state, "currentMessage"), "userInputMessage")
	modelID := strings.TrimSpace(stringFromAny(currentUser["modelId"]))
	origin := strings.TrimSpace(stringFromAny(currentUser["origin"]))

	changed := false
	normalized := make([]any, 0, len(history)+2)
	previousRole := ""
	for _, raw := range history {
		entry, _ := raw.(map[string]any)
		role := kiroHistoryEntryRole(entry)
		if role == "" {
			changed = true
			continue
		}

		if len(normalized) == 0 && role == "assistant" {
			normalized = append(normalized, syntheticKiroUserHistoryMessage(modelID, origin))
			previousRole = "user"
			changed = true
		}
		if previousRole == role {
			if role == "user" {
				normalized = append(normalized, syntheticKiroAssistantHistoryMessage())
				previousRole = "assistant"
			} else {
				normalized = append(normalized, syntheticKiroUserHistoryMessage(modelID, origin))
				previousRole = "user"
			}
			changed = true
		}

		normalized = append(normalized, raw)
		previousRole = role
	}
	if len(normalized) > 0 && previousRole == "user" {
		normalized = append(normalized, syntheticKiroAssistantHistoryMessage())
		changed = true
	}

	if changed {
		state["history"] = normalized
	}
	return changed
}

func stripKiroToolContextWithoutTools(root map[string]any) bool {
	if kiroPayloadHasCurrentTools(root) {
		return false
	}

	state := kiroObject(root, "conversationState")
	if state == nil {
		return false
	}
	changed := false
	if history, ok := state["history"].([]any); ok {
		for _, raw := range history {
			entry, _ := raw.(map[string]any)
			if entry == nil {
				continue
			}
			if user := kiroObject(entry, "userInputMessage"); user != nil {
				changed = stripKiroToolResultsFromUser(user) || changed
			}
			if assistant := kiroObject(entry, "assistantResponseMessage"); assistant != nil {
				changed = stripKiroToolUsesFromAssistant(assistant) || changed
			}
		}
	}
	currentUser := kiroObject(kiroObject(state, "currentMessage"), "userInputMessage")
	if currentUser != nil {
		changed = stripKiroToolResultsFromUser(currentUser) || changed
	}
	return changed
}

func kiroPayloadHasCurrentTools(root map[string]any) bool {
	state := kiroObject(root, "conversationState")
	currentUser := kiroObject(kiroObject(state, "currentMessage"), "userInputMessage")
	ctx := kiroObject(currentUser, "userInputMessageContext")
	tools, _ := ctx["tools"].([]any)
	return len(tools) > 0
}

func stripKiroEmptyToolUses(root map[string]any) bool {
	state := kiroObject(root, "conversationState")
	if state == nil {
		return false
	}
	changed := false
	if history, ok := state["history"].([]any); ok {
		for _, raw := range history {
			entry, _ := raw.(map[string]any)
			assistant := kiroObject(entry, "assistantResponseMessage")
			if assistant == nil {
				continue
			}
			if toolUses, ok := assistant["toolUses"].([]any); ok && len(toolUses) == 0 {
				delete(assistant, "toolUses")
				changed = true
			}
		}
	}
	return changed
}

func trimKiroPayloadHistoryToLimit(root map[string]any, maxBytes int) bool {
	if maxBytes <= 0 || compactKiroPayloadSize(root) <= maxBytes {
		return false
	}
	state := kiroObject(root, "conversationState")
	if state == nil {
		return false
	}
	history, ok := state["history"].([]any)
	if !ok || len(history) == 0 {
		return false
	}

	originalLen := len(history)
	for len(history) > 0 && compactKiroPayloadSize(root) > maxBytes {
		remove := 1
		if len(history) > 2 {
			remove = 2
		}
		if remove > len(history) {
			remove = len(history)
		}
		history = history[remove:]
		history = alignKiroHistoryToUser(history)
		state["history"] = history
		repairKiroOrphanedToolResults(root)
	}
	if len(history) == 0 {
		delete(state, "history")
	}
	return len(history) != originalLen
}

func repairKiroOrphanedToolResults(root map[string]any) bool {
	state := kiroObject(root, "conversationState")
	if state == nil {
		return false
	}
	history, _ := state["history"].([]any)
	changed := false
	for i, raw := range history {
		entry, _ := raw.(map[string]any)
		user := kiroObject(entry, "userInputMessage")
		if user == nil {
			continue
		}
		valid := map[string]struct{}{}
		if i > 0 {
			prev, _ := history[i-1].(map[string]any)
			collectKiroToolUseIDs(kiroObject(prev, "assistantResponseMessage"), valid)
		}
		changed = filterKiroToolResultsByIDs(user, valid) || changed
	}

	currentUser := kiroObject(kiroObject(state, "currentMessage"), "userInputMessage")
	if currentUser != nil {
		valid := map[string]struct{}{}
		if len(history) > 0 {
			last, _ := history[len(history)-1].(map[string]any)
			collectKiroToolUseIDs(kiroObject(last, "assistantResponseMessage"), valid)
		}
		changed = filterKiroToolResultsByIDs(currentUser, valid) || changed
	}
	return changed
}

func filterKiroToolResultsByIDs(user map[string]any, valid map[string]struct{}) bool {
	ctx := kiroObject(user, "userInputMessageContext")
	if ctx == nil {
		return false
	}
	results, ok := ctx["toolResults"].([]any)
	if !ok || len(results) == 0 {
		return false
	}

	kept := make([]any, 0, len(results))
	orphaned := make([]any, 0)
	for _, raw := range results {
		result, _ := raw.(map[string]any)
		toolUseID := strings.TrimSpace(stringFromAny(result["toolUseId"]))
		if _, ok := valid[toolUseID]; ok && toolUseID != "" {
			kept = append(kept, raw)
			continue
		}
		orphaned = append(orphaned, raw)
	}
	if len(orphaned) == 0 {
		return false
	}
	if len(kept) > 0 {
		ctx["toolResults"] = kept
	} else {
		delete(ctx, "toolResults")
	}
	appendKiroMessageText(user, formatKiroToolResultsText(orphaned))
	removeKiroContextIfEmpty(user, ctx)
	return true
}

func stripKiroToolResultsFromUser(user map[string]any) bool {
	ctx := kiroObject(user, "userInputMessageContext")
	if ctx == nil {
		return false
	}
	changed := false
	if tools, ok := ctx["tools"].([]any); ok && len(tools) == 0 {
		delete(ctx, "tools")
		changed = true
	}
	if results, ok := ctx["toolResults"].([]any); ok && len(results) > 0 {
		appendKiroMessageText(user, formatKiroToolResultsText(results))
		delete(ctx, "toolResults")
		changed = true
	}
	removeKiroContextIfEmpty(user, ctx)
	return changed
}

func stripKiroToolUsesFromAssistant(assistant map[string]any) bool {
	toolUses, ok := assistant["toolUses"].([]any)
	if !ok {
		return false
	}
	if len(toolUses) > 0 {
		appendKiroMessageText(assistant, formatKiroToolUsesText(toolUses))
	}
	delete(assistant, "toolUses")
	return true
}

func alignKiroHistoryToUser(history []any) []any {
	for len(history) > 0 {
		entry, _ := history[0].(map[string]any)
		if kiroHistoryEntryRole(entry) == "user" {
			return history
		}
		history = history[1:]
	}
	return history
}

func collectKiroToolUseIDs(assistant map[string]any, out map[string]struct{}) {
	if assistant == nil || out == nil {
		return
	}
	toolUses, _ := assistant["toolUses"].([]any)
	for _, raw := range toolUses {
		toolUse, _ := raw.(map[string]any)
		if id := strings.TrimSpace(stringFromAny(toolUse["toolUseId"])); id != "" {
			out[id] = struct{}{}
		}
	}
}

func formatKiroToolResultsText(results []any) string {
	if len(results) == 0 {
		return ""
	}
	parts := make([]string, 0, len(results))
	for _, raw := range results {
		result, _ := raw.(map[string]any)
		if result == nil {
			continue
		}
		id := strings.TrimSpace(stringFromAny(result["toolUseId"]))
		status := strings.TrimSpace(stringFromAny(result["status"]))
		label := "[Tool result"
		if id != "" {
			label += " " + id
		}
		if status != "" {
			label += " status=" + status
		}
		label += "]"
		text := strings.TrimSpace(kiroToolResultContentText(result["content"]))
		if text == "" {
			text = "(empty result)"
		}
		parts = append(parts, label+"\n"+text)
	}
	return strings.Join(parts, "\n\n")
}

func formatKiroToolUsesText(toolUses []any) string {
	if len(toolUses) == 0 {
		return ""
	}
	parts := make([]string, 0, len(toolUses))
	for _, raw := range toolUses {
		toolUse, _ := raw.(map[string]any)
		if toolUse == nil {
			continue
		}
		name := strings.TrimSpace(stringFromAny(toolUse["name"]))
		id := strings.TrimSpace(stringFromAny(toolUse["toolUseId"]))
		input := "{}"
		if rawInput, ok := toolUse["input"]; ok {
			if b, err := json.Marshal(rawInput); err == nil {
				input = string(b)
			}
		}
		label := "[Tool call"
		if name != "" {
			label += " " + name
		}
		if id != "" {
			label += " id=" + id
		}
		label += "]"
		parts = append(parts, label+"\n"+input)
	}
	return strings.Join(parts, "\n\n")
}

func kiroToolResultContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch part := item.(type) {
			case string:
				parts = append(parts, part)
			case map[string]any:
				if text := strings.TrimSpace(stringFromAny(part["text"])); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if content == nil {
			return ""
		}
		if b, err := json.Marshal(content); err == nil {
			return string(b)
		}
		return fmt.Sprint(content)
	}
}

func appendKiroMessageText(msg map[string]any, addition string) {
	addition = strings.TrimSpace(addition)
	if msg == nil || addition == "" {
		return
	}
	content := strings.TrimSpace(stringFromAny(msg["content"]))
	if content == "" {
		msg["content"] = addition
		return
	}
	msg["content"] = content + "\n\n" + addition
}

func removeKiroContextIfEmpty(user, ctx map[string]any) {
	if user == nil || ctx == nil {
		return
	}
	if len(ctx) == 0 {
		delete(user, "userInputMessageContext")
	}
}

func compactKiroPayloadSize(root map[string]any) int {
	b, err := json.Marshal(root)
	if err != nil {
		return 0
	}
	return len(b)
}

func kiroObject(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return nil
	}
	value, _ := parent[key].(map[string]any)
	return value
}

func kiroHistoryEntryRole(entry map[string]any) string {
	if entry == nil {
		return ""
	}
	if _, ok := entry["userInputMessage"].(map[string]any); ok {
		return "user"
	}
	if _, ok := entry["assistantResponseMessage"].(map[string]any); ok {
		return "assistant"
	}
	return ""
}

func syntheticKiroUserHistoryMessage(modelID, origin string) map[string]any {
	msg := map[string]any{
		"content": ".",
	}
	if modelID != "" {
		msg["modelId"] = modelID
	}
	if origin != "" {
		msg["origin"] = origin
	}
	return map[string]any{"userInputMessage": msg}
}

func syntheticKiroAssistantHistoryMessage() map[string]any {
	return map[string]any{
		"assistantResponseMessage": map[string]any{"content": "."},
	}
}
