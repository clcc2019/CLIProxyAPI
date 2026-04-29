package common

import (
	"bytes"
	"strconv"

	"github.com/tidwall/sjson"
)

var sseDataPrefix = []byte("data:")

func WrapGeminiCLIResponse(response []byte) []byte {
	out, err := sjson.SetRawBytes([]byte(`{"response":{}}`), "response", response)
	if err != nil {
		return response
	}
	return out
}

func GeminiTokenCountJSON(count int64) []byte {
	out := make([]byte, 0, 96)
	out = append(out, `{"totalTokens":`...)
	out = strconv.AppendInt(out, count, 10)
	out = append(out, `,"promptTokensDetails":[{"modality":"TEXT","tokenCount":`...)
	out = strconv.AppendInt(out, count, 10)
	out = append(out, `}]}`...)
	return out
}

func ClaudeInputTokensJSON(count int64) []byte {
	out := make([]byte, 0, 32)
	out = append(out, `{"input_tokens":`...)
	out = strconv.AppendInt(out, count, 10)
	out = append(out, '}')
	return out
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
