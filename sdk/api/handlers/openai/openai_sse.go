package openai

import (
	"bytes"
	"encoding/json"
)

type sseFrameAccumulator struct {
	pending []byte
}

func (a *sseFrameAccumulator) AddChunk(chunk []byte) [][]byte {
	var frames [][]byte
	a.ForEachChunkFrame(chunk, func(frame []byte) bool {
		frames = append(frames, append([]byte(nil), frame...))
		return true
	})
	return frames
}

func (a *sseFrameAccumulator) Flush() [][]byte {
	var frames [][]byte
	a.FlushFrames(func(frame []byte) bool {
		frames = append(frames, append([]byte(nil), frame...))
		return true
	})
	return frames
}

func (a *sseFrameAccumulator) ForEachChunkFrame(chunk []byte, fn func([]byte) bool) bool {
	if len(chunk) == 0 {
		return true
	}
	if fn != nil && len(a.pending) == 0 {
		return a.forEachDirectChunkFrame(chunk, fn)
	}
	if responsesSSENeedsLineBreak(a.pending, chunk) {
		a.pending = append(a.pending, '\n')
	}
	a.pending = append(a.pending, chunk...)
	return a.drainFrames(fn, false)
}

func (a *sseFrameAccumulator) forEachDirectChunkFrame(chunk []byte, fn func([]byte) bool) bool {
	consumed := 0
	for consumed < len(chunk) {
		frameLen := responsesSSEFrameLen(chunk[consumed:])
		if frameLen == 0 {
			break
		}
		next := consumed + frameLen
		if !fn(chunk[consumed:next]) {
			a.pending = append(a.pending, chunk[next:]...)
			return false
		}
		consumed = next
	}
	if consumed >= len(chunk) {
		return true
	}
	rest := chunk[consumed:]
	if len(bytes.TrimSpace(rest)) == 0 {
		return true
	}
	if responsesSSECanEmitWithoutDelimiter(rest, false) {
		return fn(rest)
	}
	a.pending = append(a.pending, rest...)
	return true
}

func (a *sseFrameAccumulator) FlushFrames(fn func([]byte) bool) bool {
	if len(a.pending) == 0 {
		return true
	}
	return a.drainFrames(fn, true)
}

func (a *sseFrameAccumulator) drainFrames(fn func([]byte) bool, flush bool) bool {
	if fn == nil {
		if flush {
			a.pending = nil
		}
		return true
	}

	consumed := 0
	for {
		frameLen := responsesSSEFrameLen(a.pending[consumed:])
		if frameLen == 0 {
			break
		}
		frame := a.pending[consumed : consumed+frameLen]
		consumed += frameLen
		if !fn(frame) {
			a.compactConsumed(consumed, flush)
			return false
		}
	}
	if consumed > 0 {
		a.compactConsumed(consumed, false)
	}

	if len(bytes.TrimSpace(a.pending)) == 0 {
		if flush {
			a.pending = nil
		} else {
			a.pending = a.pending[:0]
		}
		return true
	}
	if responsesSSECanEmitWithoutDelimiter(a.pending, false) {
		frame := a.pending
		if !fn(frame) {
			a.pending = nil
			return false
		}
		a.pending = nil
		return true
	}
	if flush {
		a.pending = nil
	}
	return true
}

func (a *sseFrameAccumulator) compactConsumed(consumed int, clear bool) {
	if consumed <= 0 {
		if clear {
			a.pending = nil
		}
		return
	}
	if consumed >= len(a.pending) {
		if clear {
			a.pending = nil
		} else {
			a.pending = a.pending[:0]
		}
		return
	}
	copy(a.pending, a.pending[consumed:])
	a.pending = a.pending[:len(a.pending)-consumed]
	if clear {
		a.pending = nil
	}
}

func responsesSSEFrameWithDataPayload(frame, payload []byte) []byte {
	payloadLines := bytes.Count(payload, []byte{'\n'}) + 1
	out := make([]byte, 0, len(frame)+len(payload)+payloadLines*len("data: ")+2)
	trimmedFrame := bytes.TrimRight(frame, "\r\n")
	dataWritten := false
	wroteLine := false
	for offset := 0; ; {
		lineEnd := len(trimmedFrame)
		if newline := bytes.IndexByte(trimmedFrame[offset:], '\n'); newline >= 0 {
			lineEnd = offset + newline
		}
		line := bytes.TrimRight(trimmedFrame[offset:lineEnd], "\r")
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			if !dataWritten {
				out, wroteLine = appendResponsesSSEDataLines(out, payload, wroteLine)
				dataWritten = true
			}
		} else if len(line) > 0 {
			if wroteLine {
				out = append(out, '\n')
			}
			out = append(out, line...)
			wroteLine = true
		}

		if lineEnd == len(trimmedFrame) {
			break
		}
		offset = lineEnd + 1
	}
	if !dataWritten {
		out, _ = appendResponsesSSEDataLines(out, payload, wroteLine)
	}
	return append(out, '\n', '\n')
}

func appendResponsesSSEDataLines(out, payload []byte, wroteLine bool) ([]byte, bool) {
	for offset := 0; ; {
		lineEnd := len(payload)
		if newline := bytes.IndexByte(payload[offset:], '\n'); newline >= 0 {
			lineEnd = offset + newline
		}
		if wroteLine {
			out = append(out, '\n')
		}
		out = append(out, "data: "...)
		out = append(out, payload[offset:lineEnd]...)
		wroteLine = true

		if lineEnd == len(payload) {
			break
		}
		offset = lineEnd + 1
	}
	return out, wroteLine
}

func responsesSSEFrameLen(chunk []byte) int {
	for offset := 0; offset < len(chunk); {
		idx := bytes.IndexByte(chunk[offset:], '\n')
		if idx < 0 {
			return 0
		}
		i := offset + idx
		if i+1 < len(chunk) && chunk[i+1] == '\n' {
			return i + 2
		}
		if i > 0 && chunk[i-1] == '\r' && i+2 < len(chunk) && chunk[i+1] == '\r' && chunk[i+2] == '\n' {
			return i + 3
		}
		offset = i + 1
	}
	return 0
}

func responsesSSEDataPayload(frame []byte) ([]byte, bool) {
	if len(frame) == 0 {
		return nil, false
	}

	var (
		firstPayload []byte
		payload      []byte
		dataLines    int
	)
	for remaining := frame; len(remaining) > 0; {
		line := remaining
		if index := bytes.IndexByte(remaining, '\n'); index >= 0 {
			line = remaining[:index]
			remaining = remaining[index+1:]
		} else {
			remaining = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := line[len("data: "):]
		if dataLines == 0 {
			firstPayload = data
		} else {
			if dataLines == 1 {
				payload = make([]byte, 0, len(firstPayload)+len(data))
				payload = append(payload, firstPayload...)
			}
			payload = append(payload, data...)
		}
		dataLines++
	}
	switch dataLines {
	case 0:
		return nil, false
	case 1:
		return firstPayload, true
	default:
		return payload, true
	}
}

func responsesSSENeedsMoreData(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 {
		return false
	}
	return responsesSSEHasField(trimmed, []byte("event:")) && !responsesSSEHasField(trimmed, []byte("data:"))
}

func responsesSSEHasField(chunk []byte, prefix []byte) bool {
	s := chunk
	for len(s) > 0 {
		line := s
		if i := bytes.IndexByte(s, '\n'); i >= 0 {
			line = s[:i]
			s = s[i+1:]
		} else {
			s = nil
		}
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func responsesSSECanWriteDirect(chunk []byte, trustedData bool, noticeFilter *responsesNoticeFilter) bool {
	if len(chunk) == 0 {
		return false
	}
	if noticeFilter != nil && !noticeFilter.CanBypassSSEChunk(chunk) {
		return false
	}
	frameLen := responsesSSEFrameLen(chunk)
	if frameLen == len(chunk) {
		if trustedData {
			return true
		}
		return responsesSSEDataLinesValid(bytes.TrimSpace(chunk))
	}
	return responsesSSECanEmitWithoutDelimiterWithNoticeFilter(chunk, trustedData, noticeFilter)
}

func responsesSSECanEmitWithoutDelimiter(chunk []byte, trustedData bool) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 || responsesSSENeedsMoreData(trimmed) || !responsesSSEHasField(trimmed, []byte("data:")) {
		return false
	}
	if trustedData {
		return true
	}
	return responsesSSEDataLinesValid(trimmed)
}

func responsesSSECanEmitWithoutDelimiterWithNoticeFilter(chunk []byte, trustedData bool, noticeFilter *responsesNoticeFilter) bool {
	if !responsesSSECanEmitWithoutDelimiter(chunk, trustedData) {
		return false
	}
	if noticeFilter == nil {
		return true
	}
	return responsesSSEDataLinesComplete(bytes.TrimSpace(chunk))
}

func responsesSSEDataLinesValid(chunk []byte) bool {
	return responsesSSEDataLinesValidWithMode(chunk, true)
}

func responsesSSEDataLinesComplete(chunk []byte) bool {
	return responsesSSEDataLinesValidWithMode(chunk, false)
}

func responsesSSEDataLinesValidWithMode(chunk []byte, allowEmpty bool) bool {
	s := chunk
	foundData := false
	for len(s) > 0 {
		line := s
		if i := bytes.IndexByte(s, '\n'); i >= 0 {
			line = s[:i]
			s = s[i+1:]
		} else {
			s = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		foundData = true
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 {
			if allowEmpty {
				continue
			}
			return false
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if !json.Valid(data) {
			return false
		}
	}
	return allowEmpty || foundData
}

func responsesSSENeedsLineBreak(pending, chunk []byte) bool {
	if len(pending) == 0 || len(chunk) == 0 {
		return false
	}
	if bytes.HasSuffix(pending, []byte("\n")) || bytes.HasSuffix(pending, []byte("\r")) {
		return false
	}
	if chunk[0] == '\n' || chunk[0] == '\r' {
		return false
	}
	trimmed := bytes.TrimLeft(chunk, " \t")
	if len(trimmed) == 0 {
		return false
	}
	for _, prefix := range [][]byte{[]byte("data:"), []byte("event:"), []byte("id:"), []byte("retry:"), []byte(":")} {
		if bytes.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}
