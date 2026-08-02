package common

import (
	"bytes"
	"context"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var sseDataPrefix = []byte("data:")
var sseDonePayload = []byte("[DONE]")

func ClaudeInputTokensJSON(count int64) []byte {
	out := make([]byte, 0, 32)
	out = append(out, `{"input_tokens":`...)
	out = strconv.AppendInt(out, count, 10)
	out = append(out, '}')
	return out
}

// NewRawArrayItems creates a raw item slice sized for the expected input.
func NewRawArrayItems(capacity int64) [][]byte {
	if capacity <= 0 {
		return nil
	}
	return make([][]byte, 0, int(capacity))
}

// JoinRawArray joins validated raw JSON items without re-encoding them.
func JoinRawArray(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	size := len(items) + 1
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for i, item := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}

// SetRawArrayItems replaces an empty JSON array at path with raw items. The
// single-item fast path avoids an intermediate joined array allocation.
func SetRawArrayItems(data []byte, path string, items [][]byte) []byte {
	if len(items) == 0 {
		return data
	}
	if len(items) == 1 {
		array := gjson.GetBytes(data, path)
		if array.Raw == "[]" && array.Index >= 0 && array.Index+len(array.Raw) <= len(data) {
			out := make([]byte, 0, len(data)+len(items[0]))
			out = append(out, data[:array.Index]...)
			out = append(out, '[')
			out = append(out, items[0]...)
			out = append(out, ']')
			return append(out, data[array.Index+len(array.Raw):]...)
		}
	}
	updated, err := sjson.SetRawBytes(data, path, JoinRawArray(items))
	if err != nil {
		return data
	}
	return updated
}

func PassthroughStreamPayload(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) [][]byte {
	rawJSON = TrimSSEDataPrefix(rawJSON)
	if bytes.Equal(rawJSON, sseDonePayload) {
		return [][]byte{}
	}
	return [][]byte{rawJSON}
}

func PassthroughNonStreamPayload(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) []byte {
	return rawJSON
}

func TrimSSEDataPrefix(rawJSON []byte) []byte {
	if bytes.HasPrefix(rawJSON, sseDataPrefix) {
		return bytes.TrimSpace(rawJSON[len(sseDataPrefix):])
	}
	return rawJSON
}

func SSEEventData(event string, payload []byte) []byte {
	out := make([]byte, 0, len(event)+len(payload)+14)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	return out
}

func AppendSSEEventString(out []byte, event, payload string, trailingNewlines int) []byte {
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	for i := 0; i < trailingNewlines; i++ {
		out = append(out, '\n')
	}
	return out
}

func AppendSSEEventBytes(out []byte, event string, payload []byte, trailingNewlines int) []byte {
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	for i := 0; i < trailingNewlines; i++ {
		out = append(out, '\n')
	}
	return out
}

// ForEachSSEDataLine walks an SSE byte buffer without Scanner's token buffer
// allocation. The callback receives a trimmed view into raw and must not retain it.
func ForEachSSEDataLine(raw []byte, fn func(data []byte) bool) {
	if len(raw) == 0 || fn == nil {
		return
	}
	for len(raw) > 0 {
		line := raw
		if idx := bytes.IndexByte(raw, '\n'); idx >= 0 {
			line = raw[:idx]
			raw = raw[idx+1:]
		} else {
			raw = nil
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if !bytes.HasPrefix(line, sseDataPrefix) {
			continue
		}
		data := bytes.TrimSpace(line[len(sseDataPrefix):])
		if len(data) == 0 {
			continue
		}
		if !fn(data) {
			return
		}
	}
}
