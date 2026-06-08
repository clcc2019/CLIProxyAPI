package openai

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type responsesNoticeFilter struct {
	suppressedItemIDs map[string]struct{}
	pending           []responsesNoticePendingPayload
	pendingText       string
	pendingItemID     string
}

type responsesNoticeFilteredLine struct {
	line     []byte
	payload  []byte
	payloads [][]byte
	keep     bool
	rewrite  bool
}

type responsesNoticePendingPayload struct {
	payload []byte
	itemID  string
}

const responsesNoticeFilterMarker = "heads up"

func newResponsesNoticeFilter() *responsesNoticeFilter {
	return &responsesNoticeFilter{
		suppressedItemIDs: make(map[string]struct{}),
	}
}

func (f *responsesNoticeFilter) FilterPayload(payload []byte) []byte {
	payloads := f.FilterPayloads(payload)
	if len(payloads) == 0 {
		return nil
	}
	return payloads[len(payloads)-1]
}

func (f *responsesNoticeFilter) FilterPayloads(payload []byte) [][]byte {
	return f.FilterPayloadsInto(payload, nil)
}

func (f *responsesNoticeFilter) FilterPayloadsInto(payload []byte, out [][]byte) [][]byte {
	if len(payload) == 0 {
		return append(out, payload)
	}
	if f == nil {
		return append(out, payload)
	}
	if len(f.pending) == 0 && len(f.suppressedItemIDs) == 0 && !responsesNoticeMayNeedFiltering(payload) {
		return append(out, payload)
	}
	if !json.Valid(payload) {
		return append(f.flushPendingInto(out), payload)
	}

	itemID := strings.TrimSpace(gjson.GetBytes(payload, "item_id").String())
	if itemID != "" {
		if _, ok := f.suppressedItemIDs[itemID]; ok {
			return nil
		}
	}

	itemResult := gjson.GetBytes(payload, "item")
	if itemResult.Exists() && itemResult.Type == gjson.JSON {
		payloadItemID := strings.TrimSpace(itemResult.Get("id").String())
		if payloadItemID != "" {
			if _, ok := f.suppressedItemIDs[payloadItemID]; ok {
				return nil
			}
		}
	}

	switch strings.TrimSpace(gjson.GetBytes(payload, "type").String()) {
	case "response.output_text.delta":
		text := gjson.GetBytes(payload, "delta").String()
		if f.pendingMatches(itemID) {
			combined := f.pendingText + text
			if responsesUsageWarningText(combined) {
				f.markSuppressedItem(itemID)
				f.clearPending()
				return nil
			}
			if responsesUsageWarningTextPrefix(combined) {
				f.holdPending(payload, itemID, text)
				return nil
			}
			return append(f.flushPendingInto(out), payload)
		}
		if responsesUsageWarningText(text) {
			f.markSuppressedItem(itemID)
			return nil
		}
		if responsesUsageWarningTextPrefix(text) {
			f.holdPending(payload, itemID, text)
			return nil
		}
	case "response.output_text.done":
		if responsesUsageWarningText(gjson.GetBytes(payload, "text").String()) {
			f.markSuppressedItem(itemID)
			return nil
		}
	case "response.content_part.added", "response.content_part.done":
		if responsesUsageWarningPart(gjson.GetBytes(payload, "part")) {
			f.markSuppressedItem(itemID)
			return nil
		}
	case "response.output_item.added", "response.output_item.done":
		if itemResult.Exists() && responsesUsageWarningItem(itemResult) {
			f.markSuppressedItem(strings.TrimSpace(itemResult.Get("id").String()))
			return nil
		}
	case "response.completed":
		filtered := f.filterOutputPayload(payload, "response.output")
		return append(f.flushPendingInto(out), filtered)
	}

	return append(f.flushPendingInto(out), payload)
}

func (f *responsesNoticeFilter) FilterResponseObject(payload []byte) []byte {
	if len(payload) == 0 || !json.Valid(payload) {
		return payload
	}
	if f == nil {
		return payload
	}
	return f.filterOutputPayload(payload, "output")
}

func (f *responsesNoticeFilter) FilterSSEFrame(frame []byte) []byte {
	if len(frame) == 0 {
		return frame
	}
	if f == nil {
		return frame
	}

	trimmed := bytes.TrimRight(frame, "\r\n")
	if len(trimmed) == 0 {
		return nil
	}

	canonical := len(frame) == len(trimmed)+2 &&
		frame[len(trimmed)] == '\n' &&
		frame[len(trimmed)+1] == '\n'
	var lineBuffer [8]responsesNoticeFilteredLine
	lines := lineBuffer[:0]
	dataLines := 0
	for offset := 0; ; {
		lineEnd := len(trimmed)
		if newline := bytes.IndexByte(trimmed[offset:], '\n'); newline >= 0 {
			lineEnd = offset + newline
		}
		rawLine := trimmed[offset:lineEnd]
		line := bytes.TrimRight(rawLine, "\r")
		entry := responsesNoticeFilteredLine{line: line, keep: true}
		if len(line) != len(rawLine) {
			canonical = false
		}

		trimmedLine := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmedLine, []byte("data:")) {
			lines = append(lines, entry)
		} else {
			data := bytes.TrimSpace(trimmedLine[len("data:"):])
			if len(data) == 0 || bytes.Equal(data, []byte(wsDoneMarker)) {
				dataLines++
			} else if len(f.pending) == 0 && len(f.suppressedItemIDs) == 0 && !responsesNoticeMayNeedFiltering(data) {
				dataLines++
				if !responsesNoticeDataLineMatches(line, data) {
					entry.payload = data
					entry.rewrite = true
					canonical = false
				}
			} else if !json.Valid(data) {
				dataLines++
			} else {
				var payloadBuffer [4][]byte
				filteredPayloads := f.FilterPayloadsInto(data, payloadBuffer[:0])
				if len(filteredPayloads) == 0 {
					entry.keep = false
					canonical = false
				} else {
					dataLines += len(filteredPayloads)
					if len(filteredPayloads) != 1 || !responsesNoticeDataLineMatches(line, filteredPayloads[0]) {
						if len(filteredPayloads) == 1 {
							entry.payload = filteredPayloads[0]
						} else {
							entry.payloads = filteredPayloads
						}
						entry.rewrite = true
						canonical = false
					}
				}
			}
			lines = append(lines, entry)
		}

		if lineEnd == len(trimmed) {
			break
		}
		offset = lineEnd + 1
	}
	if dataLines == 0 {
		return nil
	}
	if canonical {
		return frame
	}

	outputLen := 2
	keptLines := 0
	for i := range lines {
		if !lines[i].keep {
			continue
		}
		if keptLines > 0 {
			outputLen++
		}
		if lines[i].rewrite {
			if len(lines[i].payloads) == 0 {
				outputLen += len("data: ") + len(lines[i].payload)
			} else {
				for j := range lines[i].payloads {
					if j > 0 {
						outputLen++
					}
					outputLen += len("data: ") + len(lines[i].payloads[j])
				}
			}
		} else {
			outputLen += len(lines[i].line)
		}
		keptLines++
	}

	out := make([]byte, 0, outputLen)
	writtenLines := 0
	for i := range lines {
		if !lines[i].keep {
			continue
		}
		if writtenLines > 0 {
			out = append(out, '\n')
		}
		if lines[i].rewrite {
			if len(lines[i].payloads) == 0 {
				out = append(out, "data: "...)
				out = append(out, lines[i].payload...)
			} else {
				for j := range lines[i].payloads {
					if j > 0 {
						out = append(out, '\n')
					}
					out = append(out, "data: "...)
					out = append(out, lines[i].payloads[j]...)
				}
			}
		} else {
			out = append(out, lines[i].line...)
		}
		writtenLines++
	}
	return append(out, '\n', '\n')
}

func responsesNoticeDataLineMatches(line, payload []byte) bool {
	const prefix = "data: "
	return len(line) == len(prefix)+len(payload) &&
		bytes.Equal(line[:len(prefix)], []byte(prefix)) &&
		bytes.Equal(line[len(prefix):], payload)
}

func (f *responsesNoticeFilter) CanBypassSSEChunk(chunk []byte) bool {
	if f == nil {
		return true
	}
	if len(f.pending) != 0 {
		return false
	}
	if len(f.suppressedItemIDs) != 0 {
		return false
	}
	return !responsesNoticeMayNeedFiltering(chunk)
}

func (f *responsesNoticeFilter) markSuppressedItem(itemID string) {
	if f == nil {
		return
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return
	}
	f.suppressedItemIDs[itemID] = struct{}{}
}

func (f *responsesNoticeFilter) holdPending(payload []byte, itemID, text string) {
	if f == nil {
		return
	}
	f.pending = append(f.pending, responsesNoticePendingPayload{
		payload: append([]byte(nil), payload...),
		itemID:  strings.TrimSpace(itemID),
	})
	f.pendingText += text
	if f.pendingItemID == "" {
		f.pendingItemID = strings.TrimSpace(itemID)
	}
}

func (f *responsesNoticeFilter) pendingMatches(itemID string) bool {
	if f == nil || len(f.pending) == 0 {
		return false
	}
	itemID = strings.TrimSpace(itemID)
	if f.pendingItemID == "" || itemID == "" {
		return true
	}
	return f.pendingItemID == itemID
}

func (f *responsesNoticeFilter) flushPending() [][]byte {
	return f.flushPendingInto(nil)
}

func (f *responsesNoticeFilter) flushPendingInto(out [][]byte) [][]byte {
	if f == nil || len(f.pending) == 0 {
		return out
	}
	for i := range f.pending {
		out = append(out, f.pending[i].payload)
	}
	f.clearPending()
	return out
}

func (f *responsesNoticeFilter) clearPending() {
	if f == nil {
		return
	}
	f.pending = nil
	f.pendingText = ""
	f.pendingItemID = ""
}

func (f *responsesNoticeFilter) filterOutputPayload(payload []byte, path string) []byte {
	output := gjson.GetBytes(payload, path)
	if !output.Exists() || !output.IsArray() {
		return payload
	}

	filteredItems := make([]json.RawMessage, 0)
	output.ForEach(func(_, item gjson.Result) bool {
		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID != "" {
			if _, ok := f.suppressedItemIDs[itemID]; ok {
				return true
			}
		}
		if responsesUsageWarningItem(item) {
			f.markSuppressedItem(itemID)
			return true
		}
		filteredItems = append(filteredItems, json.RawMessage(item.Raw))
		return true
	})

	filteredJSON, err := json.Marshal(filteredItems)
	if err != nil {
		return payload
	}
	updated, err := sjson.SetRawBytes(payload, path, filteredJSON)
	if err != nil {
		return payload
	}
	return updated
}

func responsesUsageWarningItem(item gjson.Result) bool {
	if !item.Exists() || item.Type != gjson.JSON {
		return false
	}
	if responsesUsageWarningText(item.Get("text").String()) {
		return true
	}
	content := item.Get("content")
	if !content.Exists() || !content.IsArray() {
		return false
	}
	warning := false
	content.ForEach(func(_, part gjson.Result) bool {
		if responsesUsageWarningPart(part) {
			warning = true
			return false
		}
		return true
	})
	return warning
}

func responsesUsageWarningPart(part gjson.Result) bool {
	if !part.Exists() || part.Type != gjson.JSON {
		return false
	}
	return responsesUsageWarningText(part.Get("text").String())
}

func responsesUsageWarningText(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if !responsesContainsASCIIFold(text, "heads up, you have less than") {
		return false
	}
	if !responsesContainsASCIIFold(text, " limit left") {
		return false
	}
	if !responsesContainsASCIIFold(text, "run /status for a breakdown") {
		return false
	}
	return true
}

func responsesUsageWarningTextPrefix(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || responsesUsageWarningText(text) {
		return false
	}
	text = strings.ToLower(text)
	const headsUp = "heads up, you have"
	idx := strings.Index(text, headsUp)
	if idx < 0 {
		return false
	}
	tail := strings.TrimSpace(text[idx+len(headsUp):])
	if tail == "" {
		return true
	}
	if !strings.HasPrefix(tail, "less than") {
		return strings.HasPrefix("less than", tail)
	}
	if !strings.Contains(tail, " limit left") {
		return true
	}
	return !strings.Contains(tail, "run /status for a breakdown")
}

func responsesNoticeMayNeedFiltering(chunk []byte) bool {
	if len(chunk) == 0 {
		return false
	}
	return responsesContainsASCIIBytesFold(chunk, responsesNoticeFilterMarker)
}

func responsesContainsASCIIFold(s, substr string) bool {
	if substr == "" {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	first := responsesASCIILower(substr[0])
	limit := len(s) - len(substr)
	for i := 0; i <= limit; i++ {
		if responsesASCIILower(s[i]) != first {
			continue
		}
		if responsesASCIIEqualFoldAt(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func responsesContainsASCIIBytesFold(s []byte, substr string) bool {
	if substr == "" {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	first := responsesASCIILower(substr[0])
	limit := len(s) - len(substr)
	for i := 0; i <= limit; i++ {
		if responsesASCIILower(s[i]) != first {
			continue
		}
		if responsesASCIIBytesEqualFoldAt(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func responsesASCIIEqualFoldAt(s, substr string) bool {
	for i := 0; i < len(substr); i++ {
		if responsesASCIILower(s[i]) != responsesASCIILower(substr[i]) {
			return false
		}
	}
	return true
}

func responsesASCIIBytesEqualFoldAt(s []byte, substr string) bool {
	for i := 0; i < len(substr); i++ {
		if responsesASCIILower(s[i]) != responsesASCIILower(substr[i]) {
			return false
		}
	}
	return true
}

func responsesASCIILower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
