// Package openai provides streaming SSE event building for OpenAI format.
// This package handles the construction of OpenAI-compatible Server-Sent Events (SSE)
// for streaming responses from Kiro API.
package openai

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// sseChunkBufPool reuses scratch buffers for the per-chunk OpenAI SSE
// builders. Same trick as the Kiro Claude streaming path: a fresh
// map[string]interface{} + reflection-based json.Marshal per chunk costs
// ~10–20 allocations on the dominant text_delta path; the optimized form
// drops to one alloc (the returned string).
var sseChunkBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256)
		return &b
	},
}

func acquireSSEChunkBuf() *[]byte {
	bp := sseChunkBufPool.Get().(*[]byte)
	*bp = (*bp)[:0]
	return bp
}

func releaseSSEChunkBuf(bp *[]byte) {
	if cap(*bp) > 64*1024 {
		return
	}
	sseChunkBufPool.Put(bp)
}

// appendOpenAIJSONString writes s to dst as a JSON-quoted string. Defers to
// encoding/json for correctness on edge cases (control chars, non-UTF-8) —
// strconv.AppendQuote uses Go syntax which is not valid JSON.
func appendOpenAIJSONString(dst []byte, s string) []byte {
	encoded, _ := json.Marshal(s)
	return append(dst, encoded...)
}

// appendChunkEnvelope writes the chunk header common to every per-chunk
// builder up to (but not including) the choices array opener. Caller appends
// `"choices":[...]` and the closing `}`.
func appendChunkEnvelope(dst []byte, state *OpenAIStreamState) []byte {
	dst = append(dst, "{\"id\":"...)
	dst = appendOpenAIJSONString(dst, state.ResponseID)
	dst = append(dst, ",\"object\":\"chat.completion.chunk\",\"created\":"...)
	dst = strconv.AppendInt(dst, state.Created, 10)
	dst = append(dst, ",\"model\":"...)
	dst = appendOpenAIJSONString(dst, state.Model)
	return dst
}

// OpenAIStreamState tracks the state of streaming response conversion
type OpenAIStreamState struct {
	ChunkIndex        int
	ToolCallIndex     int
	HasSentFirstChunk bool
	Model             string
	ResponseID        string
	Created           int64
}

// NewOpenAIStreamState creates a new stream state for tracking
func NewOpenAIStreamState(model string) *OpenAIStreamState {
	return &OpenAIStreamState{
		ChunkIndex:        0,
		ToolCallIndex:     0,
		HasSentFirstChunk: false,
		Model:             model,
		ResponseID:        "chatcmpl-" + uuid.New().String()[:24],
		Created:           time.Now().Unix(),
	}
}

// FormatSSEEvent formats a JSON payload for SSE streaming.
// Note: This returns raw JSON data without "data:" prefix.
// The SSE "data:" prefix is added by the Handler layer (e.g., openai_handlers.go)
// to maintain architectural consistency and avoid double-prefix issues.
func FormatSSEEvent(data []byte) string {
	return string(data)
}

// BuildOpenAISSETextDelta creates an SSE event for text content delta.
// Hot path — called once per text chunk during streaming. Hand-built JSON +
// pooled buffer; same optimization as the Kiro Claude streaming path.
func BuildOpenAISSETextDelta(state *OpenAIStreamState, textDelta string) string {
	bp := acquireSSEChunkBuf()
	defer releaseSSEChunkBuf(bp)
	buf := *bp

	buf = appendChunkEnvelope(buf, state)
	buf = append(buf, ",\"choices\":[{\"index\":0,\"delta\":{"...)
	if !state.HasSentFirstChunk {
		buf = append(buf, "\"role\":\"assistant\",\"content\":"...)
		buf = appendOpenAIJSONString(buf, textDelta)
		state.HasSentFirstChunk = true
	} else {
		buf = append(buf, "\"content\":"...)
		buf = appendOpenAIJSONString(buf, textDelta)
	}
	buf = append(buf, "},\"finish_reason\":null}]}"...)

	// One alloc here (the returned string copies the pooled buffer's content).
	out := string(buf)
	*bp = buf
	state.ChunkIndex++
	return out
}

// BuildOpenAISSEToolCallStart creates an SSE event for tool call start
func BuildOpenAISSEToolCallStart(state *OpenAIStreamState, toolUseID, toolName string) string {
	toolCall := map[string]interface{}{
		"index": state.ToolCallIndex,
		"id":    toolUseID,
		"type":  "function",
		"function": map[string]interface{}{
			"name":      toolName,
			"arguments": "",
		},
	}

	delta := map[string]interface{}{
		"tool_calls": []map[string]interface{}{toolCall},
	}

	// Include role in first chunk if not sent yet
	if !state.HasSentFirstChunk {
		delta["role"] = "assistant"
		state.HasSentFirstChunk = true
	}

	chunk := buildBaseChunk(state, delta, nil)
	result, _ := json.Marshal(chunk)
	state.ChunkIndex++
	return FormatSSEEvent(result)
}

// BuildOpenAISSEToolCallArgumentsDelta creates an SSE event for tool call arguments delta
func BuildOpenAISSEToolCallArgumentsDelta(state *OpenAIStreamState, argumentsDelta string, toolIndex int) string {
	toolCall := map[string]interface{}{
		"index": toolIndex,
		"function": map[string]interface{}{
			"arguments": argumentsDelta,
		},
	}

	delta := map[string]interface{}{
		"tool_calls": []map[string]interface{}{toolCall},
	}

	chunk := buildBaseChunk(state, delta, nil)
	result, _ := json.Marshal(chunk)
	state.ChunkIndex++
	return FormatSSEEvent(result)
}

// BuildOpenAISSEFinish creates an SSE event with finish_reason.
// Called once per stream end. Optimized to share the same pooled-buffer path
// as the per-chunk delta builders.
func BuildOpenAISSEFinish(state *OpenAIStreamState, finishReason string) string {
	bp := acquireSSEChunkBuf()
	defer releaseSSEChunkBuf(bp)
	buf := *bp

	buf = appendChunkEnvelope(buf, state)
	buf = append(buf, ",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":"...)
	if finishReason != "" {
		buf = appendOpenAIJSONString(buf, finishReason)
	} else {
		buf = append(buf, "null"...)
	}
	buf = append(buf, "}]}"...)

	out := string(buf)
	*bp = buf
	state.ChunkIndex++
	return out
}

// BuildOpenAISSEUsage creates an SSE event with usage information.
//
// Emits the OpenAI nested details shape (prompt_tokens_details.cached_tokens,
// completion_tokens_details.reasoning_tokens) when upstream populated those
// buckets. Cache creation is folded back into prompt_tokens because the
// OpenAI surface has no equivalent field — downstream cost calculators that
// care about Kiro cache writes read CacheCreationTokens off usage.Detail
// directly via the usage reporter, not the client-visible SSE frame.
func BuildOpenAISSEUsage(state *OpenAIStreamState, usageInfo usage.Detail) string {
	total := usageInfo.TotalTokens
	if total == 0 {
		total = usageInfo.InputTokens + usageInfo.OutputTokens + usageInfo.ReasoningTokens
	}
	usagePayload := map[string]interface{}{
		"prompt_tokens":     usageInfo.InputTokens,
		"completion_tokens": usageInfo.OutputTokens,
		"total_tokens":      total,
	}
	if usageInfo.CachedTokens > 0 {
		usagePayload["prompt_tokens_details"] = map[string]interface{}{
			"cached_tokens": usageInfo.CachedTokens,
		}
	}
	if usageInfo.ReasoningTokens > 0 {
		usagePayload["completion_tokens_details"] = map[string]interface{}{
			"reasoning_tokens": usageInfo.ReasoningTokens,
		}
	}
	chunk := map[string]interface{}{
		"id":      state.ResponseID,
		"object":  "chat.completion.chunk",
		"created": state.Created,
		"model":   state.Model,
		"choices": []map[string]interface{}{},
		"usage":   usagePayload,
	}
	result, _ := json.Marshal(chunk)
	return FormatSSEEvent(result)
}

// BuildOpenAISSEDone creates the final [DONE] SSE event.
// Note: This returns raw "[DONE]" without "data:" prefix.
// The SSE "data:" prefix is added by the Handler layer (e.g., openai_handlers.go)
// to maintain architectural consistency and avoid double-prefix issues.
func BuildOpenAISSEDone() string {
	return "[DONE]"
}

// buildBaseChunk creates a base chunk structure for streaming
func buildBaseChunk(state *OpenAIStreamState, delta map[string]interface{}, finishReason *string) map[string]interface{} {
	choice := map[string]interface{}{
		"index": 0,
		"delta": delta,
	}

	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	} else {
		choice["finish_reason"] = nil
	}

	return map[string]interface{}{
		"id":      state.ResponseID,
		"object":  "chat.completion.chunk",
		"created": state.Created,
		"model":   state.Model,
		"choices": []map[string]interface{}{choice},
	}
}

// BuildOpenAISSEReasoningDelta creates an SSE event for reasoning content delta
// This is used for o1/o3 style models that expose reasoning tokens.
// Hot path on reasoning streams — same pooled-buffer optimization.
func BuildOpenAISSEReasoningDelta(state *OpenAIStreamState, reasoningDelta string) string {
	bp := acquireSSEChunkBuf()
	defer releaseSSEChunkBuf(bp)
	buf := *bp

	buf = appendChunkEnvelope(buf, state)
	buf = append(buf, ",\"choices\":[{\"index\":0,\"delta\":{"...)
	if !state.HasSentFirstChunk {
		buf = append(buf, "\"role\":\"assistant\",\"reasoning_content\":"...)
		buf = appendOpenAIJSONString(buf, reasoningDelta)
		state.HasSentFirstChunk = true
	} else {
		buf = append(buf, "\"reasoning_content\":"...)
		buf = appendOpenAIJSONString(buf, reasoningDelta)
	}
	buf = append(buf, "},\"finish_reason\":null}]}"...)

	out := string(buf)
	*bp = buf
	state.ChunkIndex++
	return out
}

// BuildOpenAISSEFirstChunk creates the first chunk with role only.
// Called once per stream; optimized for consistency with the rest of the
// pooled-buffer path.
func BuildOpenAISSEFirstChunk(state *OpenAIStreamState) string {
	bp := acquireSSEChunkBuf()
	defer releaseSSEChunkBuf(bp)
	buf := *bp

	buf = appendChunkEnvelope(buf, state)
	buf = append(buf, ",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}"...)

	state.HasSentFirstChunk = true
	out := string(buf)
	*bp = buf
	state.ChunkIndex++
	return out
}

// ThinkingTagState tracks state for thinking tag detection in streaming
type ThinkingTagState struct {
	InThinkingBlock   bool
	PendingStartChars int
	PendingEndChars   int
}

// NewThinkingTagState creates a new thinking tag state
func NewThinkingTagState() *ThinkingTagState {
	return &ThinkingTagState{
		InThinkingBlock:   false,
		PendingStartChars: 0,
		PendingEndChars:   0,
	}
}
