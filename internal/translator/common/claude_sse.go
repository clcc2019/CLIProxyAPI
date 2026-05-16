package common

import (
	"encoding/json"
	"strconv"
	"sync"
)

// claudeSSEBufPool reuses scratch buffers for Claude-format SSE event builders.
// Translators that emit Claude-style SSE frames (text_delta, thinking_delta,
// input_json_delta, content_block_start/stop) previously did per-chunk
// fmt.Sprintf + sjson.SetBytes + []byte() round-trips, allocating 8–15 times
// per emitted event. The pooled-buffer + hand-built JSON path collapses that
// to one alloc per event (the returned []byte).
var claudeSSEBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256)
		return &b
	},
}

func acquireClaudeSSEBuf() *[]byte {
	bp := claudeSSEBufPool.Get().(*[]byte)
	*bp = (*bp)[:0]
	return bp
}

func releaseClaudeSSEBuf(bp *[]byte) {
	if cap(*bp) > 64*1024 {
		return
	}
	claudeSSEBufPool.Put(bp)
}

// appendClaudeJSONString writes s to dst as a JSON-quoted string. Uses
// encoding/json for correctness on edge cases (control chars, non-UTF-8) —
// strconv.AppendQuote uses Go syntax which is not valid JSON.
func appendClaudeJSONString(dst []byte, s string) []byte {
	encoded, _ := json.Marshal(s)
	return append(dst, encoded...)
}

// ClaudeTextDeltaJSON returns the JSON payload for a Claude text_delta
// content_block_delta event. Hot path — called once per text token across
// gemini-claude, gemini-cli-claude, and antigravity-claude translators.
//
// The returned slice is freshly allocated (one alloc); the scratch buffer
// is pooled.
func ClaudeTextDeltaJSON(index int, text string) []byte {
	bp := acquireClaudeSSEBuf()
	defer releaseClaudeSSEBuf(bp)
	buf := *bp

	buf = append(buf, `{"type":"content_block_delta","index":`...)
	buf = strconv.AppendInt(buf, int64(index), 10)
	buf = append(buf, `,"delta":{"type":"text_delta","text":`...)
	buf = appendClaudeJSONString(buf, text)
	buf = append(buf, `}}`...)

	out := make([]byte, len(buf))
	copy(out, buf)
	*bp = buf
	return out
}

// ClaudeThinkingDeltaJSON returns the JSON payload for a Claude thinking_delta
// content_block_delta event. Hot path on reasoning-model streams.
func ClaudeThinkingDeltaJSON(index int, thinking string) []byte {
	bp := acquireClaudeSSEBuf()
	defer releaseClaudeSSEBuf(bp)
	buf := *bp

	buf = append(buf, `{"type":"content_block_delta","index":`...)
	buf = strconv.AppendInt(buf, int64(index), 10)
	buf = append(buf, `,"delta":{"type":"thinking_delta","thinking":`...)
	buf = appendClaudeJSONString(buf, thinking)
	buf = append(buf, `}}`...)

	out := make([]byte, len(buf))
	copy(out, buf)
	*bp = buf
	return out
}

// ClaudeInputJSONDeltaJSON returns the JSON payload for a Claude
// input_json_delta event used for streaming tool-call arguments.
func ClaudeInputJSONDeltaJSON(index int, partialJSON string) []byte {
	bp := acquireClaudeSSEBuf()
	defer releaseClaudeSSEBuf(bp)
	buf := *bp

	buf = append(buf, `{"type":"content_block_delta","index":`...)
	buf = strconv.AppendInt(buf, int64(index), 10)
	buf = append(buf, `,"delta":{"type":"input_json_delta","partial_json":`...)
	buf = appendClaudeJSONString(buf, partialJSON)
	buf = append(buf, `}}`...)

	out := make([]byte, len(buf))
	copy(out, buf)
	*bp = buf
	return out
}

// ClaudeContentBlockStopJSON returns the JSON payload for a Claude
// content_block_stop event.
func ClaudeContentBlockStopJSON(index int) []byte {
	bp := acquireClaudeSSEBuf()
	defer releaseClaudeSSEBuf(bp)
	buf := *bp

	buf = append(buf, `{"type":"content_block_stop","index":`...)
	buf = strconv.AppendInt(buf, int64(index), 10)
	buf = append(buf, '}')

	out := make([]byte, len(buf))
	copy(out, buf)
	*bp = buf
	return out
}

// ClaudeContentBlockStartTextJSON returns the JSON payload for a Claude
// content_block_start event with type=text. The "text" field is always empty
// at start; subsequent text_delta events fill it.
func ClaudeContentBlockStartTextJSON(index int) []byte {
	bp := acquireClaudeSSEBuf()
	defer releaseClaudeSSEBuf(bp)
	buf := *bp

	buf = append(buf, `{"type":"content_block_start","index":`...)
	buf = strconv.AppendInt(buf, int64(index), 10)
	buf = append(buf, `,"content_block":{"type":"text","text":""}}`...)

	out := make([]byte, len(buf))
	copy(out, buf)
	*bp = buf
	return out
}

// ClaudeContentBlockStartThinkingJSON returns the JSON payload for a Claude
// content_block_start event with type=thinking.
func ClaudeContentBlockStartThinkingJSON(index int) []byte {
	bp := acquireClaudeSSEBuf()
	defer releaseClaudeSSEBuf(bp)
	buf := *bp

	buf = append(buf, `{"type":"content_block_start","index":`...)
	buf = strconv.AppendInt(buf, int64(index), 10)
	buf = append(buf, `,"content_block":{"type":"thinking","thinking":""}}`...)

	out := make([]byte, len(buf))
	copy(out, buf)
	*bp = buf
	return out
}
