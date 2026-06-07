package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func writeResponsesSSEChunk(w io.Writer, chunk []byte) bool {
	return handlers.WriteRawSSEChunk(w, chunk)
}

type responsesSSEFramer struct {
	pending      []byte
	noticeFilter *responsesNoticeFilter
	trustedData  bool
	repairState  responsesCompletedOutputRepairState
}

func (f *responsesSSEFramer) WriteChunk(w io.Writer, chunk []byte) bool {
	if len(chunk) == 0 {
		return false
	}
	if len(f.pending) == 0 && responsesSSECanWriteDirect(chunk, f.trustedData, f.noticeFilter) {
		return writeResponsesSSEChunk(w, f.processFrame(chunk))
	}
	if responsesSSENeedsLineBreak(f.pending, chunk) {
		f.pending = append(f.pending, '\n')
	}
	f.pending = append(f.pending, chunk...)
	wrote := false
	consumed := 0
	for {
		frameLen := responsesSSEFrameLen(f.pending[consumed:])
		if frameLen == 0 {
			break
		}
		frame := f.pending[consumed : consumed+frameLen]
		frame = f.processFrame(frame)
		wrote = writeResponsesSSEChunk(w, frame) || wrote
		consumed += frameLen
	}
	if consumed > 0 {
		copy(f.pending, f.pending[consumed:])
		f.pending = f.pending[:len(f.pending)-consumed]
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		return wrote
	}
	if len(f.pending) == 0 || !responsesSSECanEmitWithoutDelimiterWithNoticeFilter(f.pending, f.trustedData, f.noticeFilter) {
		return wrote
	}
	frame := f.pending
	frame = f.processFrame(frame)
	wrote = writeResponsesSSEChunk(w, frame) || wrote
	f.pending = f.pending[:0]
	return wrote
}

func (f *responsesSSEFramer) Flush(w io.Writer) bool {
	if len(f.pending) == 0 {
		return false
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		return false
	}
	if !responsesSSECanEmitWithoutDelimiterWithNoticeFilter(f.pending, f.trustedData, f.noticeFilter) {
		f.pending = f.pending[:0]
		return false
	}
	frame := f.pending
	frame = f.processFrame(frame)
	wrote := writeResponsesSSEChunk(w, frame)
	f.pending = f.pending[:0]
	return wrote
}

func (f *responsesSSEFramer) processFrame(frame []byte) []byte {
	if len(frame) == 0 {
		return frame
	}
	if f.noticeFilter != nil {
		frame = f.noticeFilter.FilterSSEFrame(frame)
	}
	if len(frame) == 0 {
		return frame
	}
	return f.repairState.PatchFrame(frame)
}

type responsesCompletedOutputRepairState struct {
	outputItemsByIndex  map[int64]json.RawMessage
	outputItemsFallback []json.RawMessage
}

func (s *responsesCompletedOutputRepairState) PatchFrame(frame []byte) []byte {
	if !responsesSSEFrameMayNeedOutputRepair(frame) {
		return frame
	}
	payload, ok := responsesSSEDataPayload(frame)
	if !ok || !json.Valid(payload) {
		return frame
	}

	switch gjson.GetBytes(payload, "type").String() {
	case "response.output_item.done":
		s.recordOutputItem(payload)
		return frame
	case "response.completed":
		patched := s.patchCompletedPayload(payload)
		if bytes.Equal(patched, payload) {
			return frame
		}
		return responsesSSEFrameWithDataPayload(frame, patched)
	default:
		return frame
	}
}

func responsesSSEFrameMayNeedOutputRepair(frame []byte) bool {
	return bytes.Contains(frame, []byte("response.output_item.done")) ||
		bytes.Contains(frame, []byte("response.completed"))
}

func (s *responsesCompletedOutputRepairState) recordOutputItem(payload []byte) {
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() {
		return
	}
	itemJSON := json.RawMessage(item.Raw)
	outputIndex := gjson.GetBytes(payload, "output_index")
	if outputIndex.Exists() {
		if s.outputItemsByIndex == nil {
			s.outputItemsByIndex = make(map[int64]json.RawMessage)
		}
		s.outputItemsByIndex[outputIndex.Int()] = itemJSON
		return
	}
	s.outputItemsFallback = append(s.outputItemsFallback, itemJSON)
}

func (s *responsesCompletedOutputRepairState) patchCompletedPayload(payload []byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if !output.Exists() || !output.IsArray() || len(output.Array()) != 0 {
		return payload
	}

	items := s.outputItems()
	if len(items) == 0 {
		return payload
	}

	rawItems, err := json.Marshal(items)
	if err != nil {
		return payload
	}
	patched, err := sjson.SetRawBytes(payload, "response.output", rawItems)
	if err != nil {
		return payload
	}
	return patched
}

func (s *responsesCompletedOutputRepairState) outputItems() []json.RawMessage {
	total := len(s.outputItemsFallback)
	if s.outputItemsByIndex != nil {
		total += len(s.outputItemsByIndex)
	}
	if total == 0 {
		return nil
	}

	items := make([]json.RawMessage, 0, total)
	if len(s.outputItemsByIndex) > 0 {
		indexes := make([]int64, 0, len(s.outputItemsByIndex))
		for index := range s.outputItemsByIndex {
			indexes = append(indexes, index)
		}
		sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
		for _, index := range indexes {
			items = append(items, s.outputItemsByIndex[index])
		}
	}
	items = append(items, s.outputItemsFallback...)
	return items
}
