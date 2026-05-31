package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const defaultMCPImageDetail = "high"

var responseInputConversionMarkers = []string{
	`"mcp_tool_call_output"`,
	`"compaction_summary"`,
	`"compaction_trigger"`,
}

var responseInputFullTranscriptMarkers = []string{
	`"mcp_tool_call_output"`,
	`"compaction_summary"`,
	`"compaction_trigger"`,
	`"function_call"`,
	`"function_call_output"`,
	`"local_shell_call"`,
	`"custom_tool_call"`,
	`"custom_tool_call_output"`,
	`"tool_search_call"`,
	`"tool_search_output"`,
}

type normalizedResponseInputItem struct {
	raw       []byte
	itemType  string
	callID    string
	execution string
}

// NormalizeResponseInputItems mirrors codex-rs ResponseInputItem -> ResponseItem
// conversion for Codex-only input item variants before the payload reaches the
// OpenAI Responses API.
func NormalizeResponseInputItems(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}
	if !rawContainsAny(inputResult.Raw, responseInputConversionMarkers) {
		return rawJSON
	}

	inputItems := inputResult.Array()
	if len(inputItems) == 0 {
		return rawJSON
	}

	var normalizedInput []byte
	keptItems := 0
	appendItem := func(raw []byte) {
		if keptItems > 0 {
			normalizedInput = append(normalizedInput, ',')
		}
		normalizedInput = append(normalizedInput, raw...)
		keptItems++
	}
	for idx, item := range inputItems {
		itemRaw, keep, itemChanged := normalizeResponseInputItem(item)
		if !itemChanged && normalizedInput == nil {
			continue
		}
		if normalizedInput == nil {
			normalizedInput = make([]byte, 0, len(inputResult.Raw))
			normalizedInput = append(normalizedInput, '[')
			for _, previous := range inputItems[:idx] {
				appendItem([]byte(previous.Raw))
			}
		}
		if !keep {
			continue
		}
		appendItem(itemRaw)
	}
	if normalizedInput == nil {
		return rawJSON
	}
	normalizedInput = append(normalizedInput, ']')

	updated, err := sjson.SetRawBytes(rawJSON, "input", normalizedInput)
	if err != nil {
		return rawJSON
	}
	return updated
}

// NormalizeFullTranscriptResponseInputItems applies Codex-only item conversion
// plus the official full-history invariant repair used before sending a
// complete transcript upstream.
func NormalizeFullTranscriptResponseInputItems(rawJSON []byte) []byte {
	return normalizeFullTranscriptResponseInputItems(rawJSON)
}

func normalizeFullTranscriptResponseInputItems(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}
	if !rawContainsAny(inputResult.Raw, responseInputFullTranscriptMarkers) {
		return rawJSON
	}

	inputItems := inputResult.Array()
	if len(inputItems) == 0 {
		return rawJSON
	}

	changed := false
	normalizedItems := make([]normalizedResponseInputItem, 0, len(inputItems))
	for _, item := range inputItems {
		itemRaw, keep, itemChanged := normalizeResponseInputItem(item)
		if itemChanged {
			changed = true
		}
		if !keep {
			continue
		}
		normalized := gjson.ParseBytes(itemRaw)
		normalizedItems = append(normalizedItems, normalizedResponseInputItem{
			raw:       itemRaw,
			itemType:  strings.TrimSpace(normalized.Get("type").String()),
			callID:    strings.TrimSpace(normalized.Get("call_id").String()),
			execution: strings.TrimSpace(normalized.Get("execution").String()),
		})
	}
	if len(normalizedItems) == 0 {
		if !changed {
			return rawJSON
		}
		updated, err := sjson.SetRawBytes(rawJSON, "input", []byte("[]"))
		if err != nil {
			return rawJSON
		}
		return updated
	}

	functionCallIDs := make(map[string]struct{})
	localShellCallIDs := make(map[string]struct{})
	customToolCallIDs := make(map[string]struct{})
	toolSearchCallIDs := make(map[string]struct{})
	functionCallOutputIDs := make(map[string]struct{})
	customToolCallOutputIDs := make(map[string]struct{})
	toolSearchOutputIDs := make(map[string]struct{})
	for _, item := range normalizedItems {
		if item.callID == "" {
			continue
		}
		switch item.itemType {
		case "function_call":
			functionCallIDs[item.callID] = struct{}{}
		case "local_shell_call":
			localShellCallIDs[item.callID] = struct{}{}
		case "custom_tool_call":
			customToolCallIDs[item.callID] = struct{}{}
		case "tool_search_call":
			if !isServerToolSearchExecution(item.execution) {
				toolSearchCallIDs[item.callID] = struct{}{}
			}
		case "function_call_output":
			functionCallOutputIDs[item.callID] = struct{}{}
		case "custom_tool_call_output":
			customToolCallOutputIDs[item.callID] = struct{}{}
		case "tool_search_output":
			if !isServerToolSearchExecution(item.execution) {
				toolSearchOutputIDs[item.callID] = struct{}{}
			}
		}
	}

	normalizedInput := make([]byte, 0, len(inputResult.Raw))
	normalizedInput = append(normalizedInput, '[')
	keptItems := 0
	appendItem := func(raw []byte) {
		if keptItems > 0 {
			normalizedInput = append(normalizedInput, ',')
		}
		normalizedInput = append(normalizedInput, raw...)
		keptItems++
	}
	for _, item := range normalizedItems {
		if isOrphanResponseInputOutput(item, functionCallIDs, localShellCallIDs, customToolCallIDs, toolSearchCallIDs) {
			changed = true
			continue
		}

		appendItem(item.raw)
		if missingOutput := missingResponseInputOutputForCall(item, functionCallOutputIDs, customToolCallOutputIDs, toolSearchOutputIDs); len(missingOutput) > 0 {
			appendItem(missingOutput)
			changed = true
		}
	}
	normalizedInput = append(normalizedInput, ']')

	if !changed {
		return rawJSON
	}
	updated, err := sjson.SetRawBytes(rawJSON, "input", normalizedInput)
	if err != nil {
		return rawJSON
	}
	return updated
}

func rawContainsAny(raw string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(raw, marker) {
			return true
		}
	}
	return false
}

func normalizeResponseInputItem(item gjson.Result) ([]byte, bool, bool) {
	itemRaw := []byte(item.Raw)
	switch item.Get("type").String() {
	case "mcp_tool_call_output":
		updated, err := sjson.SetBytes(itemRaw, "type", "function_call_output")
		if err != nil {
			return itemRaw, true, false
		}
		if normalized, ok := normalizeMCPToolCallOutput(updated); ok {
			updated = normalized
		}
		return updated, true, true
	case "compaction_summary":
		updated, err := sjson.SetBytes(itemRaw, "type", "compaction")
		if err != nil {
			return itemRaw, true, false
		}
		return updated, true, true
	case "compaction_trigger":
		return nil, false, true
	default:
		return itemRaw, true, false
	}
}

func isOrphanResponseInputOutput(
	item normalizedResponseInputItem,
	functionCallIDs map[string]struct{},
	localShellCallIDs map[string]struct{},
	customToolCallIDs map[string]struct{},
	toolSearchCallIDs map[string]struct{},
) bool {
	switch item.itemType {
	case "function_call_output":
		if item.callID == "" {
			return true
		}
		if _, ok := functionCallIDs[item.callID]; ok {
			return false
		}
		_, ok := localShellCallIDs[item.callID]
		return !ok
	case "custom_tool_call_output":
		if item.callID == "" {
			return true
		}
		_, ok := customToolCallIDs[item.callID]
		return !ok
	case "tool_search_output":
		if isServerToolSearchExecution(item.execution) || item.callID == "" {
			return false
		}
		_, ok := toolSearchCallIDs[item.callID]
		return !ok
	default:
		return false
	}
}

func missingResponseInputOutputForCall(
	item normalizedResponseInputItem,
	functionCallOutputIDs map[string]struct{},
	customToolCallOutputIDs map[string]struct{},
	toolSearchOutputIDs map[string]struct{},
) []byte {
	if item.callID == "" {
		return nil
	}
	switch item.itemType {
	case "function_call", "local_shell_call":
		if _, ok := functionCallOutputIDs[item.callID]; ok {
			return nil
		}
		return buildMissingFunctionCallOutput(item.callID)
	case "custom_tool_call":
		if _, ok := customToolCallOutputIDs[item.callID]; ok {
			return nil
		}
		return buildMissingCustomToolCallOutput(item.callID)
	case "tool_search_call":
		if isServerToolSearchExecution(item.execution) {
			return nil
		}
		if _, ok := toolSearchOutputIDs[item.callID]; ok {
			return nil
		}
		return buildMissingToolSearchOutput(item.callID)
	default:
		return nil
	}
}

func isServerToolSearchExecution(execution string) bool {
	return strings.EqualFold(strings.TrimSpace(execution), "server")
}

func buildMissingFunctionCallOutput(callID string) []byte {
	payload := []byte(`{"type":"function_call_output","call_id":"","output":"aborted"}`)
	payload, _ = sjson.SetBytes(payload, "call_id", callID)
	return payload
}

func buildMissingCustomToolCallOutput(callID string) []byte {
	payload := []byte(`{"type":"custom_tool_call_output","call_id":"","output":"aborted"}`)
	payload, _ = sjson.SetBytes(payload, "call_id", callID)
	return payload
}

func buildMissingToolSearchOutput(callID string) []byte {
	payload := []byte(`{"type":"tool_search_output","call_id":"","status":"completed","execution":"client","tools":[]}`)
	payload, _ = sjson.SetBytes(payload, "call_id", callID)
	return payload
}

func normalizeMCPToolCallOutput(itemRaw []byte) ([]byte, bool) {
	output := gjson.GetBytes(itemRaw, "output")
	if !output.IsObject() {
		return itemRaw, false
	}
	wallTimeHeader := mcpToolOutputWallTimeHeader(itemRaw, output)

	if structured := firstExisting(output, "structuredContent", "structured_content"); structured.Exists() && structured.Type != gjson.Null {
		text, ok := compactJSONStringFromResult(structured)
		if !ok {
			return setFunctionCallOutputText(itemRaw, mcpToolOutputTextWithHeader(wallTimeHeader, "failed to serialize structuredContent"))
		}
		return setFunctionCallOutputText(itemRaw, mcpToolOutputTextWithHeader(wallTimeHeader, text))
	}

	content := output.Get("content")
	if !content.IsArray() {
		text, ok := compactJSONStringFromResult(output)
		if !ok {
			return itemRaw, false
		}
		return setFunctionCallOutputText(itemRaw, mcpToolOutputTextWithHeader(wallTimeHeader, text))
	}

	if contentItems, ok := mcpContentArrayToFunctionOutputItems(content, wallTimeHeader); ok {
		updated, err := sjson.SetRawBytes(itemRaw, "output", contentItems)
		return updated, err == nil
	}

	text, ok := compactJSONStringFromResult(content)
	if !ok {
		return itemRaw, false
	}
	return setFunctionCallOutputText(itemRaw, mcpToolOutputTextWithHeader(wallTimeHeader, text))
}

func setFunctionCallOutputText(itemRaw []byte, text string) ([]byte, bool) {
	updated, err := sjson.SetBytes(itemRaw, "output", text)
	return updated, err == nil
}

func mcpContentArrayToFunctionOutputItems(content gjson.Result, wallTimeHeader string) ([]byte, bool) {
	type outputItem struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		ImageURL string `json:"image_url,omitempty"`
		Detail   string `json:"detail,omitempty"`
	}

	items := make([]outputItem, 0, len(content.Array())+1)
	items = append(items, outputItem{Type: "input_text", Text: wallTimeHeader})
	sawImage := false
	content.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "text":
			items = append(items, outputItem{Type: "input_text", Text: item.Get("text").String()})
		case "image":
			sawImage = true
			data := item.Get("data").String()
			imageURL := data
			if !strings.HasPrefix(data, "data:") {
				mimeType := firstExisting(item, "mimeType", "mime_type").String()
				if strings.TrimSpace(mimeType) == "" {
					mimeType = "application/octet-stream"
				}
				imageURL = fmt.Sprintf("data:%s;base64,%s", mimeType, data)
			}
			items = append(items, outputItem{
				Type:     "input_image",
				ImageURL: imageURL,
				Detail:   mcpImageDetail(item.Get("_meta")),
			})
		default:
			text, ok := compactJSONStringFromResult(item)
			if !ok {
				text = "<content>"
			}
			items = append(items, outputItem{Type: "input_text", Text: text})
		}
		return true
	})

	if !sawImage {
		return nil, false
	}
	rawItems, err := json.Marshal(items)
	return rawItems, err == nil
}

func mcpToolOutputTextWithHeader(header, text string) string {
	if text == "" {
		return header
	}
	return header + "\n" + text
}

func mcpToolOutputWallTimeHeader(itemRaw []byte, output gjson.Result) string {
	seconds := firstMCPToolOutputWallTimeSeconds(gjson.ParseBytes(itemRaw), output)
	return fmt.Sprintf("Wall time: %.4f seconds\nOutput:", seconds)
}

func firstMCPToolOutputWallTimeSeconds(item gjson.Result, output gjson.Result) float64 {
	for _, root := range []gjson.Result{item, output} {
		if seconds, ok := firstSecondsResult(root,
			"wall_time_seconds", "wallTimeSeconds",
			"duration_seconds", "durationSeconds",
			"elapsed_seconds", "elapsedSeconds",
			"wall_time", "wallTime",
			"_meta.wall_time_seconds", "_meta.wallTimeSeconds",
			"_meta.duration_seconds", "_meta.durationSeconds",
			"_meta.elapsed_seconds", "_meta.elapsedSeconds",
		); ok {
			return seconds
		}
		if seconds, ok := firstMillisecondsResult(root,
			"wall_time_ms", "wallTimeMs",
			"duration_ms", "durationMs",
			"elapsed_ms", "elapsedMs",
			"_meta.wall_time_ms", "_meta.wallTimeMs",
			"_meta.duration_ms", "_meta.durationMs",
			"_meta.elapsed_ms", "_meta.elapsedMs",
		); ok {
			return seconds
		}
	}
	return 0
}

func firstSecondsResult(root gjson.Result, paths ...string) (float64, bool) {
	for _, path := range paths {
		if seconds, ok := nonNegativeFloatResult(root.Get(path)); ok {
			return seconds, true
		}
	}
	return 0, false
}

func firstMillisecondsResult(root gjson.Result, paths ...string) (float64, bool) {
	for _, path := range paths {
		if milliseconds, ok := nonNegativeFloatResult(root.Get(path)); ok {
			return milliseconds / 1000, true
		}
	}
	return 0, false
}

func nonNegativeFloatResult(value gjson.Result) (float64, bool) {
	if !value.Exists() {
		return 0, false
	}
	var number float64
	switch value.Type {
	case gjson.Number:
		number = value.Float()
	case gjson.String:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value.String()), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if number < 0 {
		return 0, false
	}
	return number, true
}

func mcpImageDetail(meta gjson.Result) string {
	switch detail := meta.Get("codex/imageDetail").String(); detail {
	case "auto", "low", "high", "original":
		return detail
	default:
		return defaultMCPImageDetail
	}
}

func firstExisting(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		value := root.Get(path)
		if value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func compactJSONStringFromResult(result gjson.Result) (string, bool) {
	raw := bytes.TrimSpace([]byte(result.Raw))
	if len(raw) == 0 {
		return "", false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw), true
	}
	compact, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return string(compact), true
}
