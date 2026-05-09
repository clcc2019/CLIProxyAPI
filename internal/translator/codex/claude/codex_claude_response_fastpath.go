package claude

import (
	"strconv"

	"github.com/tidwall/gjson"
)

// appendJSONString appends s to dst as a JSON string literal (including surrounding quotes).
// It escapes the minimum set of characters required by RFC 8259: quote, backslash, and
// control characters. It deliberately does not escape HTML characters (<, >, &) in order
// to match the behaviour of tidwall/sjson used previously in this file.
func appendJSONString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		if i > start {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	dst = append(dst, '"')
	return dst
}

// appendIndex writes ,"index":N for the common Claude event payloads.
func appendIndexField(dst []byte, index int) []byte {
	dst = append(dst, `,"index":`...)
	return strconv.AppendInt(dst, int64(index), 10)
}

// buildClaudeMessageStart emits the message_start payload with id and model filled in.
func buildClaudeMessageStart(id, model string) []byte {
	out := make([]byte, 0, 192+len(id)+len(model))
	out = append(out, `{"type":"message_start","message":{"id":`...)
	out = appendJSONString(out, id)
	out = append(out, `,"type":"message","role":"assistant","model":`...)
	out = appendJSONString(out, model)
	out = append(out, `,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}`...)
	return out
}

// buildClaudeThinkingDelta emits a content_block_delta with thinking_delta payload.
func buildClaudeThinkingDelta(index int, text string) []byte {
	out := make([]byte, 0, 96+len(text))
	out = append(out, `{"type":"content_block_delta"`...)
	out = appendIndexField(out, index)
	out = append(out, `,"delta":{"type":"thinking_delta","thinking":`...)
	out = appendJSONString(out, text)
	out = append(out, "}}"...)
	return out
}

// buildClaudeTextBlockStart emits a content_block_start for an empty text block.
func buildClaudeTextBlockStart(index int) []byte {
	out := make([]byte, 0, 96)
	out = append(out, `{"type":"content_block_start"`...)
	out = appendIndexField(out, index)
	out = append(out, `,"content_block":{"type":"text","text":""}}`...)
	return out
}

// buildClaudeTextDelta emits a content_block_delta with text_delta payload.
func buildClaudeTextDelta(index int, text string) []byte {
	out := make([]byte, 0, 96+len(text))
	out = append(out, `{"type":"content_block_delta"`...)
	out = appendIndexField(out, index)
	out = append(out, `,"delta":{"type":"text_delta","text":`...)
	out = appendJSONString(out, text)
	out = append(out, "}}"...)
	return out
}

// buildClaudeContentBlockStop emits a content_block_stop event.
func buildClaudeContentBlockStop(index int) []byte {
	out := make([]byte, 0, 48)
	out = append(out, `{"type":"content_block_stop"`...)
	out = appendIndexField(out, index)
	out = append(out, '}')
	return out
}

// buildClaudeMessageDelta emits a message_delta event. stopSequenceRaw must either be empty
// (encoded as JSON null) or a full raw JSON value (already quoted for strings).
func buildClaudeMessageDelta(stopReason string, stopSequenceRaw []byte, inputTokens, outputTokens, cachedTokens int64) []byte {
	out := make([]byte, 0, 160+len(stopReason)+len(stopSequenceRaw))
	out = append(out, `{"type":"message_delta","delta":{"stop_reason":`...)
	if stopReason == "" {
		out = append(out, "null"...)
	} else {
		out = appendJSONString(out, stopReason)
	}
	out = append(out, `,"stop_sequence":`...)
	if len(stopSequenceRaw) == 0 {
		out = append(out, "null"...)
	} else {
		out = append(out, stopSequenceRaw...)
	}
	out = append(out, `},"usage":{"input_tokens":`...)
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

// buildClaudeToolUseStart emits a content_block_start for a tool_use block.
func buildClaudeToolUseStart(index int, id, name string) []byte {
	out := make([]byte, 0, 128+len(id)+len(name))
	out = append(out, `{"type":"content_block_start"`...)
	out = appendIndexField(out, index)
	out = append(out, `,"content_block":{"type":"tool_use","id":`...)
	out = appendJSONString(out, id)
	out = append(out, `,"name":`...)
	out = appendJSONString(out, name)
	out = append(out, `,"input":{}}}`...)
	return out
}

// buildClaudeInputJSONDelta emits a content_block_delta with input_json_delta payload.
func buildClaudeInputJSONDelta(index int, partialJSON string) []byte {
	out := make([]byte, 0, 96+len(partialJSON))
	out = append(out, `{"type":"content_block_delta"`...)
	out = appendIndexField(out, index)
	out = append(out, `,"delta":{"type":"input_json_delta","partial_json":`...)
	out = appendJSONString(out, partialJSON)
	out = append(out, "}}"...)
	return out
}

// buildClaudeThinkingBlockStart emits a content_block_start for a thinking block.
func buildClaudeThinkingBlockStart(index int) []byte {
	out := make([]byte, 0, 96)
	out = append(out, `{"type":"content_block_start"`...)
	out = appendIndexField(out, index)
	out = append(out, `,"content_block":{"type":"thinking","thinking":""}}`...)
	return out
}

// buildClaudeSignatureDelta emits a content_block_delta with signature_delta payload.
func buildClaudeSignatureDelta(index int, signature string) []byte {
	out := make([]byte, 0, 96+len(signature))
	out = append(out, `{"type":"content_block_delta"`...)
	out = appendIndexField(out, index)
	out = append(out, `,"delta":{"type":"signature_delta","signature":`...)
	out = appendJSONString(out, signature)
	out = append(out, "}}"...)
	return out
}

// appendClaudeThinkingBlock writes a thinking content block ({"type":"thinking","thinking":"...","signature":"..."})
// into dst. signature is included only when non-empty.
func appendClaudeThinkingBlock(dst []byte, thinking, signature string) []byte {
	dst = append(dst, `{"type":"thinking","thinking":`...)
	dst = appendJSONString(dst, thinking)
	if signature != "" {
		dst = append(dst, `,"signature":`...)
		dst = appendJSONString(dst, signature)
	}
	dst = append(dst, '}')
	return dst
}

// appendClaudeTextBlock writes a text content block into dst.
func appendClaudeTextBlock(dst []byte, text string) []byte {
	dst = append(dst, `{"type":"text","text":`...)
	dst = appendJSONString(dst, text)
	dst = append(dst, '}')
	return dst
}

// appendClaudeToolUseBlock writes a tool_use content block into dst. inputRawJSON must be a
// syntactically valid JSON object literal (e.g. "{}" or `{"a":1}`).
func appendClaudeToolUseBlock(dst []byte, id, name, inputRawJSON string) []byte {
	dst = append(dst, `{"type":"tool_use","id":`...)
	dst = appendJSONString(dst, id)
	dst = append(dst, `,"name":`...)
	dst = appendJSONString(dst, name)
	dst = append(dst, `,"input":`...)
	if inputRawJSON == "" {
		dst = append(dst, "{}"...)
	} else {
		dst = append(dst, inputRawJSON...)
	}
	dst = append(dst, '}')
	return dst
}

// codexStopSequenceRaw returns the raw JSON bytes of the stop_sequence field when present
// and non-empty, and nil otherwise. The raw bytes already include surrounding quotes for
// string values.
func codexStopSequenceRaw(responseData gjson.Result) []byte {
	if stopSequence := codexStopSequence(responseData); stopSequence.Exists() && stopSequence.String() != "" {
		return []byte(stopSequence.Raw)
	}
	return nil
}
