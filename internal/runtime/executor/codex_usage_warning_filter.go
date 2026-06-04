package executor

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexUsageWarningMinPayloadBytes = len(`{"type":"response.output_text.delta","delta":"heads up less than limit left /status"}`)

var (
	codexUsageWarningMarkerLessThan      = []byte("less than")
	codexUsageWarningMarkerLimitLeft     = []byte("limit left")
	codexUsageWarningMarkerStatus        = []byte("/status")
	codexUsageWarningMarkerEscapedStatus = []byte(`\/status`)
)

type codexUsageWarningStreamEvent struct {
	eventType string
	payload   []byte
}

type codexUsageWarningStreamFilter struct {
	pending []codexUsageWarningStreamEvent
	text    string
	key     string
}

func newCodexUsageWarningStreamFilter() *codexUsageWarningStreamFilter {
	return &codexUsageWarningStreamFilter{}
}

func (f *codexUsageWarningStreamFilter) Filter(eventType string, payload []byte) []codexUsageWarningStreamEvent {
	event := codexUsageWarningStreamEvent{eventType: strings.TrimSpace(eventType), payload: payload}
	if f == nil || len(payload) == 0 {
		return []codexUsageWarningStreamEvent{event}
	}

	if len(f.pending) == 0 {
		if f.shouldHoldDelta(event.eventType, payload, "") {
			f.hold(event)
			return nil
		}
		if codexShouldSuppressUsageWarningEvent(event.eventType, payload) {
			return nil
		}
		return []codexUsageWarningStreamEvent{event}
	}

	if f.pendingMatches(event.eventType, payload) {
		switch event.eventType {
		case codexEventOutputTextDelta:
			combined := f.text + gjson.GetBytes(payload, "delta").String()
			if codexTextLooksLikeUsageLimitWarning(combined) {
				f.clear()
				return nil
			}
			if codexTextMayBeUsageLimitWarningPrefix(combined) {
				f.hold(event)
				return nil
			}
		case codexEventOutputTextDone:
			text := gjson.GetBytes(payload, "text").String()
			if codexTextLooksLikeUsageLimitWarning(text) || codexTextLooksLikeUsageLimitWarning(f.text+text) {
				f.clear()
				return nil
			}
		case codexEventOutputItemDone:
			if codexOutputItemIsUsageLimitWarning(gjson.GetBytes(payload, "item")) {
				f.clear()
				return nil
			}
		case codexEventCompleted:
			if codexCompletedContainsUsageLimitWarning(payload) {
				f.clear()
				return []codexUsageWarningStreamEvent{event}
			}
		}
	}

	flushed := f.flush()
	return append(flushed, event)
}

func (f *codexUsageWarningStreamFilter) shouldHoldDelta(eventType string, payload []byte, prefix string) bool {
	if eventType != codexEventOutputTextDelta {
		return false
	}
	text := prefix + gjson.GetBytes(payload, "delta").String()
	if codexTextLooksLikeUsageLimitWarning(text) {
		return false
	}
	return codexTextMayBeUsageLimitWarningPrefix(text)
}

func (f *codexUsageWarningStreamFilter) hold(event codexUsageWarningStreamEvent) {
	if f == nil {
		return
	}
	f.pending = append(f.pending, codexUsageWarningStreamEvent{
		eventType: event.eventType,
		payload:   bytes.Clone(event.payload),
	})
	f.text += gjson.GetBytes(event.payload, "delta").String()
	if f.key == "" {
		f.key = codexUsageWarningEventKey(event.eventType, event.payload)
	}
}

func (f *codexUsageWarningStreamFilter) flush() []codexUsageWarningStreamEvent {
	if f == nil || len(f.pending) == 0 {
		return nil
	}
	flushed := f.pending
	f.clear()
	return flushed
}

func (f *codexUsageWarningStreamFilter) clear() {
	if f == nil {
		return
	}
	f.pending = nil
	f.text = ""
	f.key = ""
}

func (f *codexUsageWarningStreamFilter) pendingMatches(eventType string, payload []byte) bool {
	if f == nil || len(f.pending) == 0 {
		return false
	}
	key := codexUsageWarningEventKey(eventType, payload)
	if f.key == "" || key == "" {
		return true
	}
	return f.key == key
}

func codexUsageWarningEventKey(eventType string, payload []byte) string {
	switch strings.TrimSpace(eventType) {
	case codexEventOutputTextDelta, codexEventOutputTextDone:
		if itemID := strings.TrimSpace(gjson.GetBytes(payload, "item_id").String()); itemID != "" {
			return "item:" + itemID
		}
	case codexEventOutputItemDone:
		if itemID := strings.TrimSpace(gjson.GetBytes(payload, "item.id").String()); itemID != "" {
			return "item:" + itemID
		}
	}
	if outputIndex := gjson.GetBytes(payload, "output_index"); outputIndex.Exists() {
		return "idx:" + outputIndex.Raw
	}
	return ""
}

func codexShouldSuppressUsageWarningEvent(eventType string, payload []byte) bool {
	switch eventType {
	case codexEventOutputTextDelta:
		if !codexPayloadMayContainUsageLimitWarning(payload) {
			return false
		}
		return codexTextLooksLikeUsageLimitWarning(gjson.GetBytes(payload, "delta").String())
	case codexEventOutputItemDone:
		if !codexPayloadMayContainUsageLimitWarning(payload) {
			return false
		}
		return codexOutputItemIsUsageLimitWarning(gjson.GetBytes(payload, "item"))
	default:
		switch strings.TrimSpace(eventType) {
		case codexEventOutputTextDelta:
			if !codexPayloadMayContainUsageLimitWarning(payload) {
				return false
			}
			return codexTextLooksLikeUsageLimitWarning(gjson.GetBytes(payload, "delta").String())
		case codexEventOutputItemDone:
			if !codexPayloadMayContainUsageLimitWarning(payload) {
				return false
			}
			return codexOutputItemIsUsageLimitWarning(gjson.GetBytes(payload, "item"))
		default:
			return false
		}
	}
}

func scrubCodexCompletedUsageWarnings(payload []byte) ([]byte, int) {
	if !codexPayloadMayContainUsageLimitWarning(payload) {
		return payload, 0
	}
	output := gjson.GetBytes(payload, "response.output")
	if !output.Exists() || !output.IsArray() {
		return payload, 0
	}

	removed := 0
	kept := make([]string, 0, len(output.Array()))
	output.ForEach(func(_, item gjson.Result) bool {
		if codexOutputItemIsUsageLimitWarning(item) {
			removed++
			return true
		}
		kept = append(kept, item.Raw)
		return true
	})
	if removed == 0 {
		return payload, 0
	}

	raw := "[]"
	if len(kept) > 0 {
		raw = "[" + strings.Join(kept, ",") + "]"
	}
	updated, err := sjson.SetRawBytes(payload, "response.output", []byte(raw))
	if err != nil || len(updated) == 0 {
		return payload, 0
	}
	return updated, removed
}

func codexPayloadMayContainUsageLimitWarning(payload []byte) bool {
	if len(payload) < codexUsageWarningMinPayloadBytes {
		return false
	}
	if !bytes.Contains(payload, codexUsageWarningMarkerLimitLeft) {
		return false
	}
	if !bytes.Contains(payload, codexUsageWarningMarkerLessThan) {
		return false
	}
	return bytes.Contains(payload, codexUsageWarningMarkerStatus) ||
		bytes.Contains(payload, codexUsageWarningMarkerEscapedStatus)
}

func codexOutputItemIsUsageLimitWarning(item gjson.Result) bool {
	if !item.Exists() || !item.IsObject() {
		return false
	}
	if itemType := strings.TrimSpace(item.Get("type").String()); itemType != "" && itemType != "message" {
		return false
	}
	if role := strings.TrimSpace(item.Get("role").String()); role != "" && role != "assistant" {
		return false
	}

	content := item.Get("content")
	if content.IsArray() {
		matched := false
		content.ForEach(func(_, part gjson.Result) bool {
			text := part.Get("text").String()
			if text == "" && part.Type == gjson.String {
				text = part.String()
			}
			if codexTextLooksLikeUsageLimitWarning(text) {
				matched = true
				return false
			}
			return true
		})
		return matched
	}
	if content.Type == gjson.String {
		return codexTextLooksLikeUsageLimitWarning(content.String())
	}
	return false
}

func codexCompletedContainsUsageLimitWarning(payload []byte) bool {
	if !codexPayloadMayContainUsageLimitWarning(payload) {
		return false
	}
	output := gjson.GetBytes(payload, "response.output")
	if !output.Exists() || !output.IsArray() {
		return false
	}
	matched := false
	output.ForEach(func(_, item gjson.Result) bool {
		if codexOutputItemIsUsageLimitWarning(item) {
			matched = true
			return false
		}
		return true
	})
	return matched
}

func codexTextLooksLikeUsageLimitWarning(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, "heads up") &&
		strings.Contains(normalized, "less than") &&
		strings.Contains(normalized, "limit left") &&
		strings.Contains(normalized, "/status")
}

func codexTextMayBeUsageLimitWarningPrefix(text string) bool {
	normalized := codexUsageWarningPrefixText(text)
	if normalized == "" {
		return false
	}
	const marker = "heads up you have less than"
	return strings.HasPrefix(marker, normalized) || strings.HasPrefix(normalized, marker)
}

func codexUsageWarningPrefixText(text string) string {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.TrimLeft(normalized, "⚠!,.:- \t\r\n")
	if normalized == "" {
		return ""
	}
	var b strings.Builder
	lastSpace := false
	for _, r := range normalized {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastSpace = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSpace = false
		case r == '/' || r == '%':
			b.WriteRune(r)
			lastSpace = false
		case r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == ',' || r == '.':
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
