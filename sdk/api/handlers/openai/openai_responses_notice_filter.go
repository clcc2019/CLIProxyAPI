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
}

type responsesNoticeFilteredLine struct {
	line    []byte
	payload []byte
	keep    bool
	rewrite bool
}

const responsesNoticeFilterMarker = "heads up"

func newResponsesNoticeFilter() *responsesNoticeFilter {
	return &responsesNoticeFilter{
		suppressedItemIDs: make(map[string]struct{}),
	}
}

func (f *responsesNoticeFilter) FilterPayload(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	if f == nil {
		return payload
	}
	if len(f.suppressedItemIDs) == 0 && !responsesNoticeMayNeedFiltering(payload) {
		return payload
	}
	if !json.Valid(payload) {
		return payload
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
		if responsesUsageWarningText(gjson.GetBytes(payload, "delta").String()) {
			f.markSuppressedItem(itemID)
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
		return f.filterOutputPayload(payload, "response.output")
	}

	return payload
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
			if len(data) == 0 || bytes.Equal(data, []byte(wsDoneMarker)) || !json.Valid(data) {
				dataLines++
			} else {
				filtered := f.FilterPayload(data)
				if len(filtered) == 0 {
					entry.keep = false
					canonical = false
				} else {
					dataLines++
					if !responsesNoticeDataLineMatches(line, filtered) {
						entry.payload = filtered
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
			outputLen += len("data: ") + len(lines[i].payload)
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
			out = append(out, "data: "...)
			out = append(out, lines[i].payload...)
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
