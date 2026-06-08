package openairesponses

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const CompactContextPruneMaxAttempts = 8

type CompactContextPruneResult struct {
	Body     []byte
	Kind     string
	OldItems int
	NewItems int
	OldBytes int
	NewBytes int
}

func PruneOldestInputContext(body []byte) (CompactContextPruneResult, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return CompactContextPruneResult{}, false
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return CompactContextPruneResult{}, false
	}
	switch {
	case input.IsArray():
		return pruneInputArray(body, input)
	case input.Type == gjson.String:
		return pruneInputString(body, "input", input.String(), "input_string", 1)
	case input.Type == gjson.JSON:
		return pruneLargestStringInValue(body, "input", input, "input_object_text", 1)
	default:
		return CompactContextPruneResult{}, false
	}
}

func pruneInputArray(body []byte, input gjson.Result) (CompactContextPruneResult, bool) {
	items, ok := rawArrayItems(input)
	if !ok {
		return CompactContextPruneResult{}, false
	}
	if len(items) <= 1 {
		if len(items) == 1 {
			return pruneLargestStringInValue(body, "input.0", gjson.ParseBytes(items[0]), "input_item_text", 1)
		}
		return CompactContextPruneResult{OldItems: 0, NewItems: 0}, false
	}

	keep := len(items) / 2
	if keep < 1 {
		keep = 1
	}
	start := len(items) - keep
	start = pruneStartAtMessageBoundary(items, start)
	if start <= 0 || start >= len(items) {
		start = len(items) - 1
	}
	start = includePairedToolCall(items, start)

	kept := items[start:]
	raw := rawJSONArray(kept)
	updated, err := sjson.SetRawBytes(body, "input", raw)
	if err != nil || len(kept) >= len(items) {
		return CompactContextPruneResult{}, false
	}
	return CompactContextPruneResult{
		Body:     updated,
		Kind:     "input_array",
		OldItems: len(items),
		NewItems: len(kept),
		OldBytes: len(bytes.TrimSpace([]byte(input.Raw))),
		NewBytes: len(raw),
	}, true
}

func pruneInputString(body []byte, path string, value string, kind string, itemCount int) (CompactContextPruneResult, bool) {
	pruned, ok := pruneOldestString(value)
	if !ok {
		return CompactContextPruneResult{}, false
	}
	updated, err := sjson.SetBytes(body, path, pruned)
	if err != nil {
		return CompactContextPruneResult{}, false
	}
	return CompactContextPruneResult{
		Body:     updated,
		Kind:     kind,
		OldItems: itemCount,
		NewItems: itemCount,
		OldBytes: len(value),
		NewBytes: len(pruned),
	}, true
}

type stringCandidate struct {
	path  string
	value string
}

func pruneLargestStringInValue(body []byte, path string, value gjson.Result, kind string, itemCount int) (CompactContextPruneResult, bool) {
	var candidates []stringCandidate
	collectPrunableStrings(value, path, &candidates)
	if len(candidates) == 0 {
		return CompactContextPruneResult{}, false
	}
	largest := candidates[0]
	for _, candidate := range candidates[1:] {
		if len(candidate.value) > len(largest.value) {
			largest = candidate
		}
	}
	return pruneInputString(body, largest.path, largest.value, kind, itemCount)
}

func collectPrunableStrings(result gjson.Result, path string, candidates *[]stringCandidate) {
	switch {
	case result.Type == gjson.String:
		if stringPathPrunable(path, result.String()) {
			*candidates = append(*candidates, stringCandidate{path: path, value: result.String()})
		}
	case result.IsArray():
		for i, item := range result.Array() {
			collectPrunableStrings(item, path+"."+strconv.Itoa(i), candidates)
		}
	case result.Type == gjson.JSON && strings.HasPrefix(strings.TrimSpace(result.Raw), "{"):
		result.ForEach(func(key, value gjson.Result) bool {
			segment := key.String()
			if !jsonPathSegmentSafe(segment) {
				return true
			}
			childPath := segment
			if path != "" {
				childPath = path + "." + segment
			}
			collectPrunableStrings(value, childPath, candidates)
			return true
		})
	}
}

func stringPathPrunable(path string, value string) bool {
	if len(value) <= 1 {
		return false
	}
	key := path
	if idx := strings.LastIndexByte(key, '.'); idx >= 0 {
		key = key[idx+1:]
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "input", "text", "content", "output", "arguments", "summary_text", "transcript":
		return true
	case "id", "type", "role", "status", "name", "call_id", "item_id", "model", "previous_response_id", "encrypted_content":
		return false
	default:
		return len(value) >= 4096
	}
}

func pruneOldestString(value string) (string, bool) {
	if len(value) <= 1 {
		return "", false
	}
	start := len(value) / 2
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	if start <= 0 || start >= len(value) {
		return "", false
	}
	pruned := value[start:]
	return pruned, len(pruned) < len(value)
}

func pruneStartAtMessageBoundary(items [][]byte, start int) int {
	if start <= 0 {
		return 0
	}
	for i := start; i < len(items); i++ {
		if itemCanStartContext(items[i]) {
			return i
		}
	}
	return len(items) - 1
}

func itemCanStartContext(raw []byte) bool {
	role := strings.TrimSpace(gjson.GetBytes(raw, "role").String())
	if role != "" {
		return strings.EqualFold(role, "user") ||
			strings.EqualFold(role, "developer") ||
			strings.EqualFold(role, "system")
	}
	itemType := strings.TrimSpace(gjson.GetBytes(raw, "type").String())
	return strings.EqualFold(itemType, "message") ||
		strings.EqualFold(itemType, "compaction") ||
		strings.EqualFold(itemType, "compaction_summary")
}

func includePairedToolCall(items [][]byte, start int) int {
	if start <= 1 || start >= len(items) {
		return start
	}
	callID := strings.TrimSpace(gjson.GetBytes(items[start], "call_id").String())
	if callID == "" || !itemLooksLikeToolOutput(items[start]) {
		return start
	}
	for i := start - 1; i >= 0; i-- {
		itemType := strings.TrimSpace(gjson.GetBytes(items[i], "type").String())
		if strings.EqualFold(itemType, "message") {
			return start
		}
		if strings.TrimSpace(gjson.GetBytes(items[i], "call_id").String()) == callID && !itemLooksLikeToolOutput(items[i]) {
			return i
		}
	}
	return start
}

func itemLooksLikeToolOutput(raw []byte) bool {
	itemType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(raw, "type").String()))
	return strings.Contains(itemType, "output") || strings.HasSuffix(itemType, "_result")
}

func rawArrayItems(result gjson.Result) ([][]byte, bool) {
	if !result.Exists() || !result.IsArray() {
		return nil, false
	}
	results := result.Array()
	items := make([][]byte, len(results))
	for i := range results {
		items[i] = []byte(results[i].Raw)
	}
	return items, true
}

func rawJSONArray(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	totalLen := 2 + len(items) - 1
	for _, item := range items {
		totalLen += len(bytes.TrimSpace(item))
	}
	buf := make([]byte, 0, totalLen)
	buf = append(buf, '[')
	for i, item := range items {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, bytes.TrimSpace(item)...)
	}
	buf = append(buf, ']')
	return buf
}

func jsonPathSegmentSafe(segment string) bool {
	if segment == "" {
		return false
	}
	for _, r := range segment {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' ||
			r == '-' {
			continue
		}
		return false
	}
	return true
}
