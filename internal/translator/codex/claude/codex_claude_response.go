// Package claude provides response translation functionality for Codex to Claude Code API compatibility.
// This package handles the conversion of Codex API responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
)

var (
	dataTag = []byte("data:")
)

// ConvertCodexResponseToClaudeParams holds parameters for response conversion.
type ConvertCodexResponseToClaudeParams struct {
	HasToolCall               bool
	BlockIndex                int
	HasReceivedArgumentsDelta bool
	HasTextDelta              bool
	TextBlockOpen             bool
	ThinkingBlockOpen         bool
	ThinkingStopPending       bool
	ThinkingSignature         string
	ThinkingSummarySeen       bool
	// reverseNameMap caches the short→original tool name mapping derived from
	// the original Claude request. It is lazily built once per stream and
	// reused across every tool-call event so the tools array is only parsed
	// once per conversation instead of once per event.
	reverseNameMap    map[string]string
	reverseNameMapSet bool
}

// reverseToolNameMap returns the short→original tool name mapping for the
// current stream, building it lazily from originalRequestRawJSON on first use.
func (p *ConvertCodexResponseToClaudeParams) reverseToolNameMap(originalRequestRawJSON []byte) map[string]string {
	if p == nil {
		return nil
	}
	if p.reverseNameMapSet {
		return p.reverseNameMap
	}
	p.reverseNameMap = buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)
	p.reverseNameMapSet = true
	return p.reverseNameMap
}

// ConvertCodexResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates Codex API responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of Claude Code-compatible JSON responses
func ConvertCodexResponseToClaude(_ context.Context, _ string, originalRequestRawJSON, _ []byte, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &ConvertCodexResponseToClaudeParams{
			HasToolCall: false,
			BlockIndex:  0,
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	output := make([]byte, 0, 512)
	rootResult := gjson.ParseBytes(rawJSON)
	params := (*param).(*ConvertCodexResponseToClaudeParams)
	if params.ThinkingBlockOpen && params.ThinkingStopPending {
		switch rootResult.Get("type").String() {
		case "response.content_part.added", "response.completed", "response.incomplete":
			output = append(output, finalizeCodexThinkingBlock(params)...)
		}
	}

	typeResult := rootResult.Get("type")
	typeStr := typeResult.String()
	var template []byte

	if typeStr == "response.created" {
		template = buildClaudeMessageStart(
			rootResult.Get("response.id").String(),
			rootResult.Get("response.model").String(),
		)

		output = translatorcommon.AppendSSEEventBytes(output, "message_start", template, 2)
	} else if typeStr == "response.reasoning_summary_part.added" {
		if params.ThinkingBlockOpen && params.ThinkingStopPending {
			output = append(output, finalizeCodexThinkingBlock(params)...)
		}
		params.ThinkingSummarySeen = true
		output = append(output, startCodexThinkingBlock(params)...)
	} else if typeStr == "response.reasoning_summary_text.delta" {
		template = buildClaudeThinkingDelta(params.BlockIndex, rootResult.Get("delta").String())

		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
	} else if typeStr == "response.reasoning_summary_part.done" {
		params.ThinkingStopPending = true
	} else if typeStr == "response.content_part.added" {
		template = buildClaudeTextBlockStart(params.BlockIndex)
		params.TextBlockOpen = true

		output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)
	} else if typeStr == "response.output_text.delta" {
		params.HasTextDelta = true
		template = buildClaudeTextDelta(params.BlockIndex, rootResult.Get("delta").String())

		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
	} else if typeStr == "response.content_part.done" {
		template = buildClaudeContentBlockStop(params.BlockIndex)
		params.TextBlockOpen = false
		params.BlockIndex++

		output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", template, 2)
	} else if typeStr == "response.completed" || typeStr == "response.incomplete" {
		responseData := rootResult.Get("response")
		stopReason := mapCodexStopReasonToClaude(codexStopReason(responseData), params.HasToolCall)
		stopSeqRaw := codexStopSequenceRaw(responseData)
		inputTokens, outputTokens, cachedTokens := extractResponsesUsage(responseData.Get("usage"))
		template = buildClaudeMessageDelta(stopReason, stopSeqRaw, inputTokens, outputTokens, cachedTokens)

		output = translatorcommon.AppendSSEEventBytes(output, "message_delta", template, 2)
		output = translatorcommon.AppendSSEEventBytes(output, "message_stop", []byte(`{"type":"message_stop"}`), 2)
	} else if typeStr == "response.output_item.added" {
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		if itemType == "function_call" {
			output = append(output, finalizeCodexThinkingBlock(params)...)
			params.HasToolCall = true
			params.HasReceivedArgumentsDelta = false
			name := itemResult.Get("name").String()
			{
				rev := params.reverseToolNameMap(originalRequestRawJSON)
				if orig, ok := rev[name]; ok {
					name = orig
				}
			}
			template = buildClaudeToolUseStart(
				params.BlockIndex,
				shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(itemResult.Get("call_id").String())),
				name,
			)

			output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)

			template = buildClaudeInputJSONDelta(params.BlockIndex, "")

			output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
		} else if itemType == "reasoning" {
			params.ThinkingSummarySeen = false
			params.ThinkingSignature = itemResult.Get("encrypted_content").String()
		}
	} else if typeStr == "response.output_item.done" {
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		if itemType == "message" {
			if params.HasTextDelta {
				return [][]byte{output}
			}
			contentResult := itemResult.Get("content")
			if !contentResult.Exists() || !contentResult.IsArray() {
				return [][]byte{output}
			}
			var textBuilder strings.Builder
			contentResult.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() != "output_text" {
					return true
				}
				if txt := part.Get("text").String(); txt != "" {
					textBuilder.WriteString(txt)
				}
				return true
			})
			text := textBuilder.String()
			if text == "" {
				return [][]byte{output}
			}

			output = append(output, finalizeCodexThinkingBlock(params)...)
			if !params.TextBlockOpen {
				template = buildClaudeTextBlockStart(params.BlockIndex)
				params.TextBlockOpen = true
				output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)
			}

			template = buildClaudeTextDelta(params.BlockIndex, text)
			output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)

			template = buildClaudeContentBlockStop(params.BlockIndex)
			params.TextBlockOpen = false
			params.BlockIndex++
			params.HasTextDelta = true
			output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", template, 2)
		} else if itemType == "function_call" {
			template = buildClaudeContentBlockStop(params.BlockIndex)
			params.BlockIndex++

			output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", template, 2)
		} else if itemType == "reasoning" {
			if signature := itemResult.Get("encrypted_content").String(); signature != "" {
				params.ThinkingSignature = signature
			}
			if params.ThinkingSummarySeen {
				output = append(output, finalizeCodexThinkingBlock(params)...)
			} else {
				output = append(output, finalizeCodexSignatureOnlyThinkingBlock(params)...)
			}
			params.ThinkingSignature = ""
			params.ThinkingSummarySeen = false
		}
	} else if typeStr == "response.function_call_arguments.delta" {
		params.HasReceivedArgumentsDelta = true
		template = buildClaudeInputJSONDelta(params.BlockIndex, rootResult.Get("delta").String())

		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
	} else if typeStr == "response.function_call_arguments.done" {
		if !params.HasReceivedArgumentsDelta {
			if args := rootResult.Get("arguments").String(); args != "" {
				template = buildClaudeInputJSONDelta(params.BlockIndex, args)

				output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
			}
		}
	}

	return [][]byte{output}
}

// ConvertCodexResponseToClaudeNonStream converts a non-streaming Codex response to a non-streaming Claude Code response.
// This function processes the complete Codex response and transforms it into a single Claude Code-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Claude Code API format.
func ConvertCodexResponseToClaudeNonStream(_ context.Context, _ string, originalRequestRawJSON, _ []byte, rawJSON []byte, _ *any) []byte {
	revNames := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)

	rootResult := gjson.ParseBytes(rawJSON)
	typeStr := rootResult.Get("type").String()
	if typeStr != "response.completed" && typeStr != "response.incomplete" {
		return []byte{}
	}

	responseData := rootResult.Get("response")
	if !responseData.Exists() {
		return []byte{}
	}

	id := responseData.Get("id").String()
	model := responseData.Get("model").String()
	inputTokens, outputTokens, cachedTokens := extractResponsesUsage(responseData.Get("usage"))

	// Pre-sized buffer. Most message bodies fit comfortably inside 1 KiB once tool input
	// arguments and reasoning text are excluded; let append grow as needed.
	out := make([]byte, 0, 512)
	out = append(out, `{"id":`...)
	out = appendJSONString(out, id)
	out = append(out, `,"type":"message","role":"assistant","model":`...)
	out = appendJSONString(out, model)
	out = append(out, `,"content":[`...)

	hasToolCall := false
	contentCount := 0

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "reasoning":
				thinkingBuilder := strings.Builder{}
				signature := item.Get("encrypted_content").String()
				if summary := item.Get("summary"); summary.Exists() {
					if summary.IsArray() {
						summary.ForEach(func(_, part gjson.Result) bool {
							if txt := part.Get("text"); txt.Exists() {
								thinkingBuilder.WriteString(txt.String())
							} else {
								thinkingBuilder.WriteString(part.String())
							}
							return true
						})
					} else {
						thinkingBuilder.WriteString(summary.String())
					}
				}
				if thinkingBuilder.Len() == 0 {
					if content := item.Get("content"); content.Exists() {
						if content.IsArray() {
							content.ForEach(func(_, part gjson.Result) bool {
								if txt := part.Get("text"); txt.Exists() {
									thinkingBuilder.WriteString(txt.String())
								} else {
									thinkingBuilder.WriteString(part.String())
								}
								return true
							})
						} else {
							thinkingBuilder.WriteString(content.String())
						}
					}
				}
				if thinkingBuilder.Len() > 0 || signature != "" {
					if contentCount > 0 {
						out = append(out, ',')
					}
					out = appendClaudeThinkingBlock(out, thinkingBuilder.String(), signature)
					contentCount++
				}
			case "message":
				if content := item.Get("content"); content.Exists() {
					if content.IsArray() {
						content.ForEach(func(_, part gjson.Result) bool {
							if part.Get("type").String() == "output_text" {
								text := part.Get("text").String()
								if text != "" {
									if contentCount > 0 {
										out = append(out, ',')
									}
									out = appendClaudeTextBlock(out, text)
									contentCount++
								}
							}
							return true
						})
					} else {
						text := content.String()
						if text != "" {
							if contentCount > 0 {
								out = append(out, ',')
							}
							out = appendClaudeTextBlock(out, text)
							contentCount++
						}
					}
				}
			case "function_call":
				hasToolCall = true
				name := item.Get("name").String()
				if original, ok := revNames[name]; ok {
					name = original
				}

				inputRaw := "{}"
				if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
					argsJSON := gjson.Parse(argsStr)
					if argsJSON.IsObject() {
						inputRaw = argsJSON.Raw
					}
				}
				if contentCount > 0 {
					out = append(out, ',')
				}
				out = appendClaudeToolUseBlock(out, shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(item.Get("call_id").String())), name, inputRaw)
				contentCount++
			}
			return true
		})
	}

	out = append(out, `],"stop_reason":`...)
	stopReason := mapCodexStopReasonToClaude(codexStopReason(responseData), hasToolCall)
	if stopReason == "" {
		out = append(out, "null"...)
	} else {
		out = appendJSONString(out, stopReason)
	}
	out = append(out, `,"stop_sequence":`...)
	if raw := codexStopSequenceRaw(responseData); len(raw) > 0 {
		out = append(out, raw...)
	} else {
		out = append(out, "null"...)
	}
	out = append(out, `,"usage":{"input_tokens":`...)
	out = strconv.AppendInt(out, inputTokens, 10)
	out = append(out, `,"output_tokens":`...)
	out = strconv.AppendInt(out, outputTokens, 10)
	if cachedTokens > 0 {
		out = append(out, `,"cache_read_input_tokens":`...)
		out = strconv.AppendInt(out, cachedTokens, 10)
	}
	out = append(out, "}}"...)

	return out
}

func codexStopReason(responseData gjson.Result) string {
	if stopReason := responseData.Get("stop_reason"); stopReason.Exists() && stopReason.String() != "" {
		if stopReason.String() == "stop" && codexStopSequence(responseData).String() != "" {
			return "stop_sequence"
		}
		return stopReason.String()
	}
	if reason := responseData.Get("incomplete_details.reason"); reason.Exists() && reason.String() != "" {
		return reason.String()
	}
	if codexStopSequence(responseData).String() != "" {
		return "stop_sequence"
	}
	return ""
}

func mapCodexStopReasonToClaude(stopReason string, hasToolCall bool) string {
	if hasToolCall {
		return "tool_use"
	}

	switch stopReason {
	case "", "stop", "completed":
		return "end_turn"
	case "max_tokens", "max_output_tokens":
		return "max_tokens"
	case "tool_use", "tool_calls", "function_call":
		return "tool_use"
	case "end_turn", "stop_sequence", "pause_turn", "refusal", "model_context_window_exceeded":
		return stopReason
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func codexStopSequence(responseData gjson.Result) gjson.Result {
	return responseData.Get("stop_sequence")
}

func extractResponsesUsage(usage gjson.Result) (int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0
	}

	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()

	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}

	return inputTokens, outputTokens, cachedTokens
}

// buildReverseMapFromClaudeOriginalShortToOriginal builds a map[short]original from original Claude request tools.
func buildReverseMapFromClaudeOriginalShortToOriginal(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
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
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

func ClaudeTokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}

func startCodexThinkingBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if params.ThinkingBlockOpen {
		return nil
	}

	template := buildClaudeThinkingBlockStart(params.BlockIndex)
	params.ThinkingBlockOpen = true
	params.ThinkingStopPending = false

	return translatorcommon.AppendSSEEventBytes(nil, "content_block_start", template, 2)
}

func finalizeCodexSignatureOnlyThinkingBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if params.ThinkingSignature == "" {
		return nil
	}

	output := startCodexThinkingBlock(params)
	output = append(output, finalizeCodexThinkingBlock(params)...)
	return output
}

func finalizeCodexThinkingBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if !params.ThinkingBlockOpen {
		return nil
	}

	output := make([]byte, 0, 256)
	if params.ThinkingSignature != "" {
		signatureDelta := buildClaudeSignatureDelta(params.BlockIndex, params.ThinkingSignature)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", signatureDelta, 2)
	}

	contentBlockStop := buildClaudeContentBlockStop(params.BlockIndex)
	output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", contentBlockStop, 2)

	params.BlockIndex++
	params.ThinkingBlockOpen = false
	params.ThinkingStopPending = false

	return output
}
