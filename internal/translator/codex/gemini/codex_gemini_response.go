// Package gemini provides response translation functionality for Codex to Gemini API compatibility.
// This package handles the conversion of Codex API responses into Gemini-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by Gemini API clients.
package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"time"

	codexcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/common"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// ConvertCodexResponseToGeminiParams holds parameters for response conversion.
type ConvertCodexResponseToGeminiParams struct {
	Model              string
	CreatedAt          int64
	ResponseID         string
	LastStorageOutputs [][]byte
	HasOutputTextDelta bool
	HasReasoningDelta  bool
	LastImageHashByID  map[string][32]byte
	// reverseNameMap caches the short→original tool name mapping derived from
	// the original Gemini request. See the Claude translator for the same
	// rationale; the tools array is otherwise parsed on every tool-call event.
	reverseNameMap    map[string]string
	reverseNameMapSet bool
}

// reverseToolNameMap returns the short→original tool name mapping for the
// current stream, building it lazily from originalRequestRawJSON on first use.
func (p *ConvertCodexResponseToGeminiParams) reverseToolNameMap(originalRequestRawJSON []byte) map[string]string {
	if p == nil {
		return nil
	}
	if p.reverseNameMapSet {
		return p.reverseNameMap
	}
	p.reverseNameMap = buildReverseMapFromGeminiOriginal(originalRequestRawJSON)
	p.reverseNameMapSet = true
	return p.reverseNameMap
}

// ConvertCodexResponseToGemini converts Codex streaming response format to Gemini format.
// This function processes various Codex event types and transforms them into Gemini-compatible JSON responses.
// It handles text content, tool calls, and usage metadata, outputting responses that match the Gemini API format.
// The function maintains state across multiple calls to ensure proper response sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of Gemini-compatible JSON responses
func ConvertCodexResponseToGemini(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &ConvertCodexResponseToGeminiParams{
			Model:              modelName,
			CreatedAt:          0,
			ResponseID:         "",
			LastStorageOutputs: nil,
			HasOutputTextDelta: false,
			HasReasoningDelta:  false,
			LastImageHashByID:  make(map[string][32]byte),
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	rootResult := gjson.ParseBytes(rawJSON)
	typeResult := rootResult.Get("type")
	typeStr := typeResult.String()

	params := (*param).(*ConvertCodexResponseToGeminiParams)

	// Base Gemini response template
	template := []byte(`{"candidates":[{"content":{"role":"model","parts":[]}}],"usageMetadata":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"gemini-2.5-pro","createTime":"2025-08-15T02:52:03.884209Z","responseId":"06CeaPH7NaCU48APvNXDyA4"}`)
	{
		template, _ = sjson.SetBytes(template, "modelVersion", params.Model)
		createdAtResult := rootResult.Get("response.created_at")
		if createdAtResult.Exists() {
			params.CreatedAt = createdAtResult.Int()
			template, _ = sjson.SetBytes(template, "createTime", time.Unix(params.CreatedAt, 0).Format(time.RFC3339Nano))
		}
		template, _ = sjson.SetBytes(template, "responseId", params.ResponseID)
	}

	if typeStr == "response.image_generation_call.partial_image" {
		itemID := rootResult.Get("item_id").String()
		b64 := rootResult.Get("partial_image_b64").String()
		if b64 == "" {
			return [][]byte{}
		}
		if itemID != "" {
			if params.LastImageHashByID == nil {
				params.LastImageHashByID = make(map[string][32]byte)
			}
			hash := sha256.Sum256([]byte(b64))
			if last, ok := params.LastImageHashByID[itemID]; ok && last == hash {
				return [][]byte{}
			}
			params.LastImageHashByID[itemID] = hash
		}

		outputFormat := rootResult.Get("output_format").String()
		mimeType := mimeTypeFromCodexOutputFormat(outputFormat)

		part := []byte(`{"inlineData":{"data":"","mimeType":""}}`)
		part, _ = sjson.SetBytes(part, "inlineData.data", b64)
		part, _ = sjson.SetBytes(part, "inlineData.mimeType", mimeType)
		template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
		return [][]byte{template}
	}

	// Handle function call completion
	if typeStr == "response.output_item.done" {
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		if itemType == "image_generation_call" {
			itemID := itemResult.Get("id").String()
			b64 := itemResult.Get("result").String()
			if b64 == "" {
				return [][]byte{}
			}
			if itemID != "" {
				if params.LastImageHashByID == nil {
					params.LastImageHashByID = make(map[string][32]byte)
				}
				hash := sha256.Sum256([]byte(b64))
				if last, ok := params.LastImageHashByID[itemID]; ok && last == hash {
					return [][]byte{}
				}
				params.LastImageHashByID[itemID] = hash
			}

			outputFormat := itemResult.Get("output_format").String()
			mimeType := mimeTypeFromCodexOutputFormat(outputFormat)

			part := []byte(`{"inlineData":{"data":"","mimeType":""}}`)
			part, _ = sjson.SetBytes(part, "inlineData.data", b64)
			part, _ = sjson.SetBytes(part, "inlineData.mimeType", mimeType)
			template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
			return [][]byte{template}
		}
		if functionCall, ok := codexGeminiFunctionCallPart(itemResult, params.reverseToolNameMap(originalRequestRawJSON)); ok {
			template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", functionCall)
			template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")

			params.LastStorageOutputs = append(params.LastStorageOutputs, append([]byte(nil), template...))

			// Use this return to storage message
			return [][]byte{}
		}
	}

	if typeStr == "response.created" { // Handle response creation - set model and response ID
		template, _ = sjson.SetBytes(template, "modelVersion", rootResult.Get("response.model").String())
		template, _ = sjson.SetBytes(template, "responseId", rootResult.Get("response.id").String())
		params.ResponseID = rootResult.Get("response.id").String()
	} else if typeStr == "response.reasoning_summary_text.delta" { // Handle reasoning/thinking content delta
		params.HasReasoningDelta = true
		part := []byte(`{"thought":true,"text":""}`)
		part, _ = sjson.SetBytes(part, "text", rootResult.Get("delta").String())
		template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
	} else if typeStr == "response.output_item.added" && rootResult.Get("item.type").String() == "reasoning" {
		params.HasReasoningDelta = false
		return [][]byte{}
	} else if typeStr == "response.output_text.delta" { // Handle regular text content delta
		params.HasOutputTextDelta = true
		part := []byte(`{"text":""}`)
		part, _ = sjson.SetBytes(part, "text", rootResult.Get("delta").String())
		template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
	} else if typeStr == "response.output_item.done" { // Fallback: emit final message text when no delta chunks were received
		itemResult := rootResult.Get("item")
		if itemResult.Get("type").String() == "reasoning" {
			if params.HasReasoningDelta {
				params.HasReasoningDelta = false
				return [][]byte{}
			}
			if reasoningText := codexGeminiReasoningText(itemResult); reasoningText != "" {
				part := []byte(`{"thought":true,"text":""}`)
				part, _ = sjson.SetBytes(part, "text", reasoningText)
				template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
				params.HasReasoningDelta = false
				return [][]byte{template}
			}
			params.HasReasoningDelta = false
			return [][]byte{}
		}
		if itemResult.Get("type").String() != "message" || params.HasOutputTextDelta {
			return [][]byte{}
		}
		contentResult := itemResult.Get("content")
		if !contentResult.Exists() || !contentResult.IsArray() {
			return [][]byte{}
		}
		wroteText := false
		contentResult.ForEach(func(_, partResult gjson.Result) bool {
			if partResult.Get("type").String() != "output_text" {
				return true
			}
			text := partResult.Get("text").String()
			if text == "" {
				return true
			}
			part := []byte(`{"text":""}`)
			part, _ = sjson.SetBytes(part, "text", text)
			template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
			wroteText = true
			return true
		})
		if wroteText {
			params.HasOutputTextDelta = true
			return [][]byte{template}
		}
		return [][]byte{}
	} else if typeStr == "response.completed" { // Handle response completion with usage metadata
		template = setCodexGeminiUsageMetadata(template, rootResult.Get("response.usage"))
	} else {
		return [][]byte{}
	}

	if len(params.LastStorageOutputs) > 0 {
		outputs := make([][]byte, 0, len(params.LastStorageOutputs)+1)
		for _, stored := range params.LastStorageOutputs {
			outputs = append(outputs, append([]byte(nil), stored...))
		}
		params.LastStorageOutputs = nil
		outputs = append(outputs, template)
		return outputs
	}
	return [][]byte{template}
}

// ConvertCodexResponseToGeminiNonStream converts a non-streaming Codex response to a non-streaming Gemini response.
// This function processes the complete Codex response and transforms it into a single Gemini-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Gemini API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: A Gemini-compatible JSON response containing all message content and metadata
func ConvertCodexResponseToGeminiNonStream(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	rootResult := gjson.ParseBytes(rawJSON)

	// Verify this is a response.completed event
	if rootResult.Get("type").String() != "response.completed" {
		return []byte{}
	}

	// Base Gemini response template for non-streaming
	template := []byte(`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"","createTime":"","responseId":""}`)

	// Set model version
	template, _ = sjson.SetBytes(template, "modelVersion", modelName)

	// Set response metadata from the completed response
	responseData := rootResult.Get("response")
	if responseData.Exists() {
		// Set response ID
		if responseId := responseData.Get("id"); responseId.Exists() {
			template, _ = sjson.SetBytes(template, "responseId", responseId.String())
		}

		// Set creation time
		if createdAt := responseData.Get("created_at"); createdAt.Exists() {
			template, _ = sjson.SetBytes(template, "createTime", time.Unix(createdAt.Int(), 0).Format(time.RFC3339Nano))
		}

		// Set usage metadata
		if usage := responseData.Get("usage"); usage.Exists() {
			template = setCodexGeminiUsageMetadata(template, usage)
		}

		// Process output content to build parts array
		hasToolCall := false
		var pendingFunctionCalls [][]byte

		flushPendingFunctionCalls := func() {
			if len(pendingFunctionCalls) == 0 {
				return
			}
			// Add all pending function calls as individual parts
			// This maintains the original Gemini API format while ensuring consecutive calls are grouped together
			for _, fc := range pendingFunctionCalls {
				template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", fc)
			}
			pendingFunctionCalls = nil
		}

		if output := responseData.Get("output"); output.Exists() && output.IsArray() {
			output.ForEach(func(key, value gjson.Result) bool {
				itemType := value.Get("type").String()

				switch itemType {
				case "reasoning":
					// Flush any pending function calls before adding non-function content
					flushPendingFunctionCalls()

					// Add thinking content
					if reasoningText := codexGeminiReasoningText(value); reasoningText != "" {
						part := []byte(`{"text":"","thought":true}`)
						part, _ = sjson.SetBytes(part, "text", reasoningText)
						template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
					}

				case "message":
					// Flush any pending function calls before adding non-function content
					flushPendingFunctionCalls()

					// Add regular text content
					if content := value.Get("content"); content.Exists() && content.IsArray() {
						content.ForEach(func(_, contentItem gjson.Result) bool {
							if contentItem.Get("type").String() == "output_text" {
								if text := contentItem.Get("text"); text.Exists() {
									part := []byte(`{"text":""}`)
									part, _ = sjson.SetBytes(part, "text", text.String())
									template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
								}
							}
							return true
						})
					}

				case "image_generation_call":
					flushPendingFunctionCalls()
					b64 := value.Get("result").String()
					if b64 == "" {
						break
					}
					outputFormat := value.Get("output_format").String()
					mimeType := mimeTypeFromCodexOutputFormat(outputFormat)

					part := []byte(`{"inlineData":{"data":"","mimeType":""}}`)
					part, _ = sjson.SetBytes(part, "inlineData.data", b64)
					part, _ = sjson.SetBytes(part, "inlineData.mimeType", mimeType)
					template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)

				case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call":
					// Collect function call for potential merging with consecutive ones
					hasToolCall = true
					functionCall, ok := codexGeminiFunctionCallPart(value, buildReverseMapFromGeminiOriginal(originalRequestRawJSON))
					if !ok {
						return true
					}

					pendingFunctionCalls = append(pendingFunctionCalls, functionCall)
				}
				return true
			})

			// Handle any remaining pending function calls at the end
			flushPendingFunctionCalls()
		}

		// Set finish reason based on whether there were tool calls
		if hasToolCall {
			template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
		} else {
			template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
		}
	}
	return template
}

func codexGeminiFunctionCallPart(item gjson.Result, revNames map[string]string) ([]byte, bool) {
	itemType := item.Get("type").String()
	name := ""
	argsRaw := "{}"
	switch itemType {
	case "function_call":
		name = item.Get("name").String()
		if orig, ok := revNames[name]; ok {
			name = orig
		}
		if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
			argsResult := gjson.Parse(argsStr)
			if argsResult.IsObject() {
				argsRaw = argsResult.Raw
			}
		}
	case "custom_tool_call":
		name = item.Get("name").String()
		argsRaw = codexGeminiStringInputObject(item.Get("input").String())
	case "local_shell_call":
		name = "local_shell"
		if action := item.Get("action"); action.Exists() && action.IsObject() {
			argsRaw = action.Raw
		}
	case "tool_search_call":
		name = "tool_search"
		if item.Get("execution").String() == "server" && item.Get("call_id").String() == "" {
			return nil, false
		}
		if args := item.Get("arguments"); args.Exists() && args.IsObject() {
			argsRaw = args.Raw
		}
	default:
		return nil, false
	}
	if name == "" {
		return nil, false
	}
	functionCall := []byte(`{"functionCall":{"args":{},"name":""}}`)
	functionCall, _ = sjson.SetBytes(functionCall, "functionCall.name", name)
	functionCall, _ = sjson.SetRawBytes(functionCall, "functionCall.args", []byte(argsRaw))
	return functionCall, true
}

func codexGeminiStringInputObject(input string) string {
	if input != "" && gjson.Valid(input) {
		parsed := gjson.Parse(input)
		if parsed.IsObject() {
			return parsed.Raw
		}
	}
	out := []byte(`{"input":""}`)
	out, _ = sjson.SetBytes(out, "input", input)
	return string(out)
}

func codexGeminiReasoningText(item gjson.Result) string {
	var builder strings.Builder
	if summary := item.Get("summary"); summary.IsArray() {
		summary.ForEach(func(_, part gjson.Result) bool {
			if text := part.Get("text").String(); text != "" {
				builder.WriteString(text)
			}
			return true
		})
	}
	if builder.Len() > 0 {
		return builder.String()
	}
	if content := item.Get("content"); content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			if text := part.Get("text").String(); text != "" {
				builder.WriteString(text)
			}
			return true
		})
	} else if content.Exists() && content.Type == gjson.String {
		builder.WriteString(content.String())
	}
	return builder.String()
}

func setCodexGeminiUsageMetadata(template []byte, usage gjson.Result) []byte {
	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	reasoningTokens := codexcommon.ReasoningOutputTokens(usage).Int()
	cachedTokens := codexcommon.CachedInputTokens(usage).Int()
	totalTokens := usage.Get("total_tokens").Int()
	if totalTokens == 0 {
		totalTokens = inputTokens + outputTokens
	}

	template, _ = sjson.SetBytes(template, "usageMetadata.promptTokenCount", inputTokens)
	template, _ = sjson.SetBytes(template, "usageMetadata.candidatesTokenCount", outputTokens)
	template, _ = sjson.SetBytes(template, "usageMetadata.totalTokenCount", totalTokens)
	if reasoningTokens > 0 {
		template, _ = sjson.SetBytes(template, "usageMetadata.thoughtsTokenCount", reasoningTokens)
	}
	if cachedTokens > 0 {
		template, _ = sjson.SetBytes(template, "usageMetadata.cachedContentTokenCount", cachedTokens)
	}
	return template
}

// buildReverseMapFromGeminiOriginal builds a map[short]original from original Gemini request tools.
func buildReverseMapFromGeminiOriginal(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
	}
	var names []string
	tarr := tools.Array()
	for i := 0; i < len(tarr); i++ {
		fns := tarr[i].Get("functionDeclarations")
		if !fns.IsArray() {
			continue
		}
		for _, fn := range fns.Array() {
			if v := fn.Get("name"); v.Exists() {
				names = append(names, v.String())
			}
		}
	}
	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

func GeminiTokenCount(ctx context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}

func mimeTypeFromCodexOutputFormat(outputFormat string) string {
	if outputFormat == "" {
		return "image/png"
	}
	if strings.Contains(outputFormat, "/") {
		return outputFormat
	}
	switch strings.ToLower(outputFormat) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	default:
		return "image/png"
	}
}
