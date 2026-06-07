// Package claude provides request translation functionality for Claude Code API compatibility.
// It handles parsing and transforming Claude Code API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Claude Code API format and the internal client's expected format.
package claude

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	codexcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeRequestToCodex parses and transforms a Claude Code API request into the internal client format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the internal client.
// The function performs the following transformations:
// 1. Sets up a template with the model name and instructions field
// 2. Processes system messages and converts them to Codex instructions
// 3. Transforms message contents (text, image, tool_use, tool_result) to appropriate formats
// 4. Converts tools declarations to the expected format
// 5. Adds additional configuration parameters for the Codex API
// 6. Maps Claude thinking configuration to Codex reasoning settings
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Claude Code API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in internal client format
func ConvertClaudeRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	template := []byte(`{"model":"","instructions":"","input":[]}`)

	rootResult := gjson.ParseBytes(rawJSON)
	toolNameMap := buildReverseMapFromClaudeOriginalToShort(rawJSON)
	toolKindByCallID := map[string]codexClaudeToolCallKind{}
	template, _ = sjson.SetBytes(template, "model", modelName)

	instructions := collectClaudeSystemInstructions(rootResult.Get("system"))
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}
	template, _ = sjson.SetBytes(template, "instructions", instructions)

	// Process messages and transform their contents to appropriate formats.
	messagesResult := rootResult.Get("messages")
	if messagesResult.IsArray() {
		messageResults := messagesResult.Array()

		for i := 0; i < len(messageResults); i++ {
			messageResult := messageResults[i]
			messageRole := messageResult.Get("role").String()
			if messageRole == "system" {
				messageRole = "developer"
			}

			newMessage := func() []byte {
				msg := []byte(`{"type":"message","role":"","content":[]}`)
				msg, _ = sjson.SetBytes(msg, "role", messageRole)
				return msg
			}

			message := newMessage()
			contentIndex := 0
			hasContent := false

			flushMessage := func() {
				if hasContent {
					template, _ = sjson.SetRawBytes(template, "input.-1", message)
					message = newMessage()
					contentIndex = 0
					hasContent = false
				}
			}

			appendTextContent := func(text string) {
				partType := "input_text"
				if messageRole == "assistant" {
					partType = "output_text"
				}
				message, _ = sjson.SetBytes(message, codexIndexedPath("content", contentIndex, "type"), partType)
				message, _ = sjson.SetBytes(message, codexIndexedPath("content", contentIndex, "text"), text)
				contentIndex++
				hasContent = true
			}

			appendImageContent := func(dataURL string) {
				message, _ = sjson.SetBytes(message, codexIndexedPath("content", contentIndex, "type"), "input_image")
				message, _ = sjson.SetBytes(message, codexIndexedPath("content", contentIndex, "image_url"), dataURL)
				contentIndex++
				hasContent = true
			}

			appendReasoningContent := func(part gjson.Result) {
				if messageRole != "assistant" {
					return
				}

				signature := part.Get("signature").String()
				if !isFernetLikeReasoningSignature(signature) {
					return
				}

				flushMessage()
				reasoningItem := []byte(`{"type":"reasoning","summary":[],"content":null}`)
				reasoningItem, _ = sjson.SetBytes(reasoningItem, "encrypted_content", signature)
				template, _ = sjson.SetRawBytes(template, "input.-1", reasoningItem)
			}

			messageContentsResult := messageResult.Get("content")
			if messageContentsResult.IsArray() {
				messageContentResults := messageContentsResult.Array()
				for j := 0; j < len(messageContentResults); j++ {
					messageContentResult := messageContentResults[j]
					contentType := messageContentResult.Get("type").String()

					switch contentType {
					case "text":
						appendTextContent(messageContentResult.Get("text").String())
					case "thinking":
						appendReasoningContent(messageContentResult)
					case "image":
						sourceResult := messageContentResult.Get("source")
						if sourceResult.Exists() {
							data := sourceResult.Get("data").String()
							if data == "" {
								data = sourceResult.Get("base64").String()
							}
							if data != "" {
								mediaType := sourceResult.Get("media_type").String()
								if mediaType == "" {
									mediaType = sourceResult.Get("mime_type").String()
								}
								if mediaType == "" {
									mediaType = "application/octet-stream"
								}
								dataURL := codexDataURL(mediaType, data)
								appendImageContent(dataURL)
							}
						}
					case "tool_use":
						flushMessage()
						callID := shortenCodexCallIDIfNeeded(messageContentResult.Get("id").String())
						toolUseName := messageContentResult.Get("name").String()
						toolKind := codexClaudeToolCallKindForUse(toolUseName, messageContentResult.Get("input"))
						if callID != "" {
							toolKindByCallID[callID] = toolKind
						}
						template, _ = sjson.SetRawBytes(template, "input.-1", codexClaudeToolUseToInputItem(messageContentResult, callID, toolUseName, toolKind, toolNameMap))
					case "tool_result":
						flushMessage()
						callID := shortenCodexCallIDIfNeeded(messageContentResult.Get("tool_use_id").String())
						template, _ = sjson.SetRawBytes(template, "input.-1", codexClaudeToolResultToInputItem(messageContentResult, callID, toolKindByCallID[callID]))
					}
				}
				flushMessage()
			} else if messageContentsResult.Type == gjson.String {
				appendTextContent(messageContentsResult.String())
				flushMessage()
			}
		}

	}

	// Convert tools declarations to the expected format for the Codex API.
	toolsResult := rootResult.Get("tools")
	if toolsResult.IsArray() {
		template, _ = sjson.SetRawBytes(template, "tools", []byte(`[]`))
		webSearchToolNames := buildClaudeWebSearchToolNameSet(toolsResult)
		template, _ = sjson.SetRawBytes(template, "tool_choice", convertClaudeToolChoiceToCodex(rootResult.Get("tool_choice"), toolNameMap, webSearchToolNames))
		toolResults := toolsResult.Array()
		for i := 0; i < len(toolResults); i++ {
			toolResult := toolResults[i]
			// Special handling: map Claude web search tool to Codex web_search
			if isClaudeWebSearchToolType(toolResult.Get("type").String()) {
				template, _ = sjson.SetRawBytes(template, "tools.-1", convertClaudeWebSearchToolToCodex(toolResult))
				continue
			}
			tool := []byte(toolResult.Raw)
			tool, _ = sjson.SetBytes(tool, "type", "function")
			// Apply shortened name if needed
			if v := toolResult.Get("name"); v.Exists() {
				name := v.String()
				if short, ok := toolNameMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				tool, _ = sjson.SetBytes(tool, "name", name)
			}
			tool, _ = sjson.SetRawBytes(tool, "parameters", []byte(normalizeToolParameters(toolResult.Get("input_schema").Raw)))
			tool, _ = sjson.DeleteBytes(tool, "input_schema")
			tool, _ = sjson.DeleteBytes(tool, "parameters.$schema")
			tool, _ = sjson.DeleteBytes(tool, "cache_control")
			tool, _ = sjson.DeleteBytes(tool, "defer_loading")
			tool, _ = sjson.SetBytes(tool, "strict", false)
			template, _ = sjson.SetRawBytes(template, "tools.-1", tool)
		}
	}

	// Default to parallel tool calls unless tool_choice explicitly disables them.
	parallelToolCalls := true
	if disableParallelToolUse := rootResult.Get("tool_choice.disable_parallel_tool_use"); disableParallelToolUse.Exists() {
		parallelToolCalls = !disableParallelToolUse.Bool()
	}

	// Add additional configuration parameters for the Codex API.
	template, _ = sjson.SetBytes(template, "parallel_tool_calls", parallelToolCalls)

	// Convert thinking.budget_tokens to reasoning.effort.
	reasoningEffort := "medium"
	if thinkingConfig := rootResult.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		switch thinkingConfig.Get("type").String() {
		case "enabled":
			if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
				budget := int(budgetTokens.Int())
				if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
					reasoningEffort = effort
				}
			}
		case "adaptive", "auto":
			// Adaptive thinking can carry an explicit effort in output_config.effort (Claude 4.6).
			// Pass through directly; ApplyThinking handles clamping to target model's levels.
			effort := ""
			if v := rootResult.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
				effort = normalizeReasoningEffort(v.String())
			}
			if effort != "" {
				reasoningEffort = effort
			} else {
				reasoningEffort = string(thinking.LevelXHigh)
			}
		case "disabled":
			if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
				reasoningEffort = effort
			}
		}
	}
	template, _ = sjson.SetBytes(template, "reasoning.effort", reasoningEffort)
	template, _ = sjson.SetBytes(template, "reasoning.summary", "auto")
	template, _ = sjson.SetBytes(template, "stream", true)
	template, _ = sjson.SetBytes(template, "store", false)
	template, _ = sjson.SetBytes(template, "include", []string{"reasoning.encrypted_content"})

	return template
}

func normalizeReasoningEffort(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	switch {
	case strings.EqualFold(value, "none"):
		return "none"
	case strings.EqualFold(value, "minimal"):
		return "minimal"
	case strings.EqualFold(value, "low"):
		return "low"
	case strings.EqualFold(value, "medium"):
		return "medium"
	case strings.EqualFold(value, "high"):
		return "high"
	case strings.EqualFold(value, "xhigh"):
		return "xhigh"
	case strings.EqualFold(value, "max"):
		return "max"
	case strings.EqualFold(value, "auto"):
		return "auto"
	case strings.EqualFold(value, "adaptive"):
		return "adaptive"
	default:
		return strings.ToLower(value)
	}
}

type codexClaudeToolCallKind struct {
	ItemType string
	Name     string
}

func codexClaudeToolCallKindForUse(name string, input gjson.Result) codexClaudeToolCallKind {
	switch name {
	case "local_shell":
		return codexClaudeToolCallKind{ItemType: "local_shell_call", Name: name}
	case "tool_search":
		return codexClaudeToolCallKind{ItemType: "tool_search_call", Name: name}
	case "apply_patch":
		return codexClaudeToolCallKind{ItemType: "custom_tool_call", Name: name}
	}
	if input.IsObject() {
		fields := input.Map()
		if len(fields) == 1 && input.Get("input").Type == gjson.String {
			return codexClaudeToolCallKind{ItemType: "custom_tool_call", Name: name}
		}
	}
	return codexClaudeToolCallKind{ItemType: "function_call", Name: name}
}

func codexClaudeToolUseToInputItem(toolUse gjson.Result, callID string, name string, kind codexClaudeToolCallKind, toolNameMap map[string]string) []byte {
	input := toolUse.Get("input")
	switch kind.ItemType {
	case "custom_tool_call":
		item := []byte(`{"type":"custom_tool_call"}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		item, _ = sjson.SetBytes(item, "name", name)
		if payload := input.Get("input"); payload.Exists() {
			item, _ = sjson.SetBytes(item, "input", payload.String())
		} else {
			item, _ = sjson.SetBytes(item, "input", input.Raw)
		}
		return item
	case "local_shell_call":
		item := []byte(`{"type":"local_shell_call","status":"completed","action":{}}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		if input.IsObject() {
			item, _ = sjson.SetRawBytes(item, "action", []byte(input.Raw))
		}
		return item
	case "tool_search_call":
		item := []byte(`{"type":"tool_search_call","status":"completed","execution":"client","arguments":{}}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		if input.IsObject() {
			item, _ = sjson.SetRawBytes(item, "arguments", []byte(input.Raw))
		}
		return item
	default:
		item := []byte(`{"type":"function_call"}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		if short, ok := toolNameMap[name]; ok {
			name = short
		} else {
			name = shortenNameIfNeeded(name)
		}
		item, _ = sjson.SetBytes(item, "name", name)
		item, _ = sjson.SetBytes(item, "arguments", input.Raw)
		return item
	}
}

func codexClaudeToolResultToInputItem(toolResult gjson.Result, callID string, kind codexClaudeToolCallKind) []byte {
	switch kind.ItemType {
	case "custom_tool_call":
		item := []byte(`{"type":"custom_tool_call_output"}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		if kind.Name != "" {
			item, _ = sjson.SetBytes(item, "name", kind.Name)
		}
		return codexClaudeSetToolResultOutput(item, toolResult)
	case "tool_search_call":
		item := []byte(`{"type":"tool_search_output","status":"completed","execution":"client","tools":[]}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		item, _ = sjson.SetRawBytes(item, "tools", codexClaudeToolSearchOutputTools(toolResult.Get("content")))
		return item
	default:
		item := []byte(`{"type":"function_call_output"}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		return codexClaudeSetToolResultOutput(item, toolResult)
	}
}

func codexClaudeSetToolResultOutput(item []byte, toolResult gjson.Result) []byte {
	if outputRaw, ok := codexClaudeToolResultContentItems(toolResult.Get("content")); ok {
		item, _ = sjson.SetRawBytes(item, "output", outputRaw)
		return item
	}
	item, _ = sjson.SetBytes(item, "output", toolResult.Get("content").String())
	return item
}

func codexClaudeToolResultContentItems(contentResult gjson.Result) ([]byte, bool) {
	if !contentResult.IsArray() {
		return nil, false
	}
	toolResultContentIndex := 0
	toolResultContent := []byte(`[]`)
	contentResults := contentResult.Array()
	for k := 0; k < len(contentResults); k++ {
		toolResultContentType := contentResults[k].Get("type").String()
		if toolResultContentType == "image" {
			sourceResult := contentResults[k].Get("source")
			if sourceResult.Exists() {
				data := sourceResult.Get("data").String()
				if data == "" {
					data = sourceResult.Get("base64").String()
				}
				if data != "" {
					mediaType := sourceResult.Get("media_type").String()
					if mediaType == "" {
						mediaType = sourceResult.Get("mime_type").String()
					}
					if mediaType == "" {
						mediaType = "application/octet-stream"
					}
					dataURL := codexDataURL(mediaType, data)
					toolResultContent, _ = sjson.SetBytes(toolResultContent, codexIndexedPath("", toolResultContentIndex, "type"), "input_image")
					toolResultContent, _ = sjson.SetBytes(toolResultContent, codexIndexedPath("", toolResultContentIndex, "image_url"), dataURL)
					toolResultContentIndex++
				}
			}
		} else if toolResultContentType == "text" {
			toolResultContent, _ = sjson.SetBytes(toolResultContent, codexIndexedPath("", toolResultContentIndex, "type"), "input_text")
			toolResultContent, _ = sjson.SetBytes(toolResultContent, codexIndexedPath("", toolResultContentIndex, "text"), contentResults[k].Get("text").String())
			toolResultContentIndex++
		}
	}
	if toolResultContentIndex == 0 {
		return nil, false
	}
	return toolResultContent, true
}

func codexClaudeToolSearchOutputTools(content gjson.Result) []byte {
	if !content.Exists() || content.Type == gjson.Null {
		return []byte(`[]`)
	}
	if content.IsArray() {
		return []byte(content.Raw)
	}
	if content.IsObject() {
		if tools := content.Get("tools"); tools.IsArray() {
			return []byte(tools.Raw)
		}
		wrapped, _ := json.Marshal([]any{json.RawMessage(content.Raw)})
		return wrapped
	}
	text := strings.TrimSpace(content.String())
	if text == "" {
		return []byte(`[]`)
	}
	parsed := gjson.Parse(text)
	if parsed.IsArray() {
		return []byte(parsed.Raw)
	}
	if parsed.IsObject() {
		if tools := parsed.Get("tools"); tools.IsArray() {
			return []byte(tools.Raw)
		}
		wrapped, _ := json.Marshal([]any{json.RawMessage(parsed.Raw)})
		return wrapped
	}
	wrapped, _ := json.Marshal([]string{text})
	return wrapped
}

func collectClaudeSystemInstructions(systemsResult gjson.Result) string {
	if !systemsResult.Exists() {
		return ""
	}

	var parts []string
	appendSystemText := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "x-anthropic-billing-header: ") {
			return
		}
		parts = append(parts, text)
	}

	if systemsResult.Type == gjson.String {
		appendSystemText(systemsResult.String())
	} else if systemsResult.IsArray() {
		systemResults := systemsResult.Array()
		for i := 0; i < len(systemResults); i++ {
			systemResult := systemResults[i]
			if systemResult.Get("type").String() == "text" {
				appendSystemText(systemResult.Get("text").String())
			}
		}
	}

	return strings.Join(parts, "\n\n")
}

// isFernetLikeReasoningSignature checks only the encrypted_content envelope shape
// observed in OpenAI reasoning signatures. It does not authenticate source or payload type.
func isFernetLikeReasoningSignature(signature string) bool {
	const (
		fernetVersionLen = 1
		fernetTimestamp  = 8
		fernetIV         = 16
		fernetHMAC       = 32
		aesBlockSize     = 16
	)

	signature = strings.TrimSpace(signature)
	if !strings.HasPrefix(signature, "gAAAA") {
		return false
	}

	decoded, err := base64.URLEncoding.DecodeString(signature)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(signature)
		if err != nil {
			return false
		}
	}

	minLen := fernetVersionLen + fernetTimestamp + fernetIV + aesBlockSize + fernetHMAC
	if len(decoded) < minLen || decoded[0] != 0x80 {
		return false
	}

	ciphertextLen := len(decoded) - fernetVersionLen - fernetTimestamp - fernetIV - fernetHMAC
	return ciphertextLen > 0 && ciphertextLen%aesBlockSize == 0
}

// shortenCodexCallIDIfNeeded keeps Claude tool IDs within the OpenAI Responses
// API call_id limit while preserving a stable, low-collision mapping.
func shortenCodexCallIDIfNeeded(id string) string {
	const limit = 64
	if len(id) <= limit {
		return id
	}

	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLen := limit - len(suffix)
	if prefixLen <= 0 {
		return suffix[len(suffix)-limit:]
	}
	return id[:prefixLen] + suffix
}

func isClaudeWebSearchToolType(toolType string) bool {
	return toolType == "web_search_20250305" || toolType == "web_search_20260209"
}

func buildClaudeWebSearchToolNameSet(tools gjson.Result) map[string]struct{} {
	names := map[string]struct{}{}
	if !tools.IsArray() {
		return names
	}

	tools.ForEach(func(_, tool gjson.Result) bool {
		toolType := tool.Get("type").String()
		if !isClaudeWebSearchToolType(toolType) {
			return true
		}

		if name := tool.Get("name").String(); name != "" {
			names[name] = struct{}{}
		}
		return true
	})

	return names
}

func convertClaudeToolChoiceToCodex(toolChoice gjson.Result, toolNameMap map[string]string, webSearchToolNames map[string]struct{}) []byte {
	if !toolChoice.Exists() || toolChoice.Type == gjson.Null {
		return []byte(`"auto"`)
	}

	choiceType := toolChoice.Get("type").String()
	if choiceType == "" && toolChoice.Type == gjson.String {
		choiceType = toolChoice.String()
	}

	switch choiceType {
	case "auto", "":
		return []byte(`"auto"`)
	case "any":
		return []byte(`"required"`)
	case "none":
		return []byte(`"none"`)
	case "tool":
		name := toolChoice.Get("name").String()
		if _, ok := webSearchToolNames[name]; ok {
			return []byte(`{"type":"web_search"}`)
		}
		if short, ok := toolNameMap[name]; ok {
			name = short
		} else {
			name = shortenNameIfNeeded(name)
		}
		if name == "" {
			return []byte(`"auto"`)
		}

		choice := []byte(`{"type":"function","name":""}`)
		choice, _ = sjson.SetBytes(choice, "name", name)
		return choice
	default:
		return []byte(`"auto"`)
	}
}

func convertClaudeWebSearchToolToCodex(tool gjson.Result) []byte {
	out := []byte(`{"type":"web_search"}`)
	if allowedDomains := tool.Get("allowed_domains"); allowedDomains.Exists() && allowedDomains.IsArray() {
		out, _ = sjson.SetRawBytes(out, "filters.allowed_domains", []byte(allowedDomains.Raw))
	}
	if userLocation := tool.Get("user_location"); userLocation.Exists() && userLocation.IsObject() {
		out, _ = sjson.SetRawBytes(out, "user_location", []byte(userLocation.Raw))
	}
	return out
}

// codexIndexedPath composes an sjson path of the form "<prefix>.<idx>.<suffix>"
// without the allocation overhead of fmt.Sprintf. Hot translator paths invoke
// sjson.SetBytes repeatedly per content part; building the path with
// strconv.AppendInt/append keeps the allocations predictable.
func codexIndexedPath(prefix string, idx int, suffix string) string {
	// Capacity: prefix + '.' + up to 20 digits + '.' + suffix.
	buf := make([]byte, 0, len(prefix)+len(suffix)+22)
	if prefix != "" {
		buf = append(buf, prefix...)
		buf = append(buf, '.')
	}
	buf = strconv.AppendInt(buf, int64(idx), 10)
	if suffix != "" {
		buf = append(buf, '.')
		buf = append(buf, suffix...)
	}
	return string(buf)
}

// codexDataURL builds a "data:<mime>;base64,<data>" URL without fmt.Sprintf.
func codexDataURL(mediaType, data string) string {
	buf := make([]byte, 0, 13+len(mediaType)+len(data))
	buf = append(buf, "data:"...)
	buf = append(buf, mediaType...)
	buf = append(buf, ";base64,"...)
	buf = append(buf, data...)
	return string(buf)
}

// shortenNameIfNeeded applies a simple shortening rule for a single name.
// Delegates to the shared codex translator helper so all four translators
// stay in sync.
func shortenNameIfNeeded(name string) string {
	return codexcommon.ShortenNameIfNeeded(name)
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	return codexcommon.BuildShortNameMap(names)
}

// buildReverseMapFromClaudeOriginalToShort builds original->short map, used to map tool_use names to short.
func buildReverseMapFromClaudeOriginalToShort(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	m := map[string]string{}
	if !tools.IsArray() {
		return m
	}
	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		n := arr[i].Get("name").String()
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		m = buildShortNameMap(names)
	}
	return m
}

// normalizeToolParameters ensures object schemas contain at least an empty properties map.
func normalizeToolParameters(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !gjson.Valid(raw) {
		return `{"type":"object","properties":{}}`
	}
	result := gjson.Parse(raw)
	schema := []byte(raw)
	schemaType := result.Get("type").String()
	if schemaType == "" {
		schema, _ = sjson.SetBytes(schema, "type", "object")
		schemaType = "object"
	}
	if schemaType == "object" && !result.Get("properties").Exists() {
		schema, _ = sjson.SetRawBytes(schema, "properties", []byte(`{}`))
	}
	return string(schema)
}
