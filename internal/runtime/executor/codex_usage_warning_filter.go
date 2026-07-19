package executor

import (
	"bytes"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexUsageWarningMinPayloadBytes = len(`{"type":"response.output_text.delta","delta":"heads up less than limit left /status"}`)

	codexEventContentPartAdded = "response.content_part.added"
	codexEventContentPartDone  = "response.content_part.done"
)

var (
	codexUsageWarningMarkerLessThan      = "less than"
	codexUsageWarningMarkerLimitLeft     = "limit left"
	codexUsageWarningMarkerStatus        = "/status"
	codexUsageWarningMarkerEscapedStatus = `\/status`
)

type codexUsageWarningStreamEvent struct {
	eventType string
	payload   []byte
}

type codexUsageWarningStreamFilter struct {
	pending []codexUsageWarningStreamEvent
	text    string
	key     string
	// single backs the common one-event result. Filter callers consume the
	// returned slice synchronously before invoking Filter again.
	single [1]codexUsageWarningStreamEvent
}

func newCodexUsageWarningStreamFilter() *codexUsageWarningStreamFilter {
	return &codexUsageWarningStreamFilter{}
}

func (f *codexUsageWarningStreamFilter) Filter(eventType string, payload []byte) []codexUsageWarningStreamEvent {
	event := codexUsageWarningStreamEvent{eventType: strings.TrimSpace(eventType), payload: payload}
	if f == nil {
		return []codexUsageWarningStreamEvent{event}
	}
	if len(payload) == 0 {
		return f.singleEvent(event)
	}

	if len(f.pending) == 0 {
		if f.shouldHoldDelta(event.eventType, payload, "") {
			f.hold(event)
			return nil
		}
		if codexShouldSuppressUsageWarningEvent(event.eventType, payload) {
			return nil
		}
		return f.singleEvent(event)
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
		case codexEventOutputItemAdded, codexEventOutputItemDone:
			if codexOutputItemIsUsageLimitWarning(gjson.GetBytes(payload, "item")) {
				f.clear()
				return nil
			}
		case codexEventContentPartAdded, codexEventContentPartDone:
			if codexContentPartIsUsageLimitWarning(gjson.GetBytes(payload, "part")) {
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

func (f *codexUsageWarningStreamFilter) singleEvent(event codexUsageWarningStreamEvent) []codexUsageWarningStreamEvent {
	f.single[0] = event
	return f.single[:]
}

func (f *codexUsageWarningStreamFilter) shouldHoldDelta(eventType string, payload []byte, prefix string) bool {
	if eventType != codexEventOutputTextDelta {
		return false
	}
	if prefix == "" {
		if raw, escaped, ok := codexTopLevelJSONStringRaw(payload, codexJSONKeyDelta); ok && !escaped {
			if codexTextLooksLikeUsageLimitWarningBytes(raw) {
				return false
			}
			return codexTextMayBeUsageLimitWarningPrefixBytes(raw)
		}
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
	case codexEventOutputTextDelta, codexEventOutputTextDone, codexEventContentPartAdded, codexEventContentPartDone:
		if itemID := strings.TrimSpace(gjson.GetBytes(payload, "item_id").String()); itemID != "" {
			return "item:" + itemID
		}
	case codexEventOutputItemAdded, codexEventOutputItemDone:
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
	switch strings.TrimSpace(eventType) {
	case codexEventOutputTextDelta:
		if !codexPayloadMayContainUsageLimitWarning(payload) {
			return false
		}
		return codexTextLooksLikeUsageLimitWarning(gjson.GetBytes(payload, "delta").String())
	case codexEventOutputTextDone:
		if !codexPayloadMayContainUsageLimitWarning(payload) {
			return false
		}
		return codexTextLooksLikeUsageLimitWarning(gjson.GetBytes(payload, "text").String())
	case codexEventOutputItemAdded, codexEventOutputItemDone:
		if !codexPayloadMayContainUsageLimitWarning(payload) {
			return false
		}
		return codexOutputItemIsUsageLimitWarning(gjson.GetBytes(payload, "item"))
	case codexEventContentPartAdded, codexEventContentPartDone:
		if !codexPayloadMayContainUsageLimitWarning(payload) {
			return false
		}
		return codexContentPartIsUsageLimitWarning(gjson.GetBytes(payload, "part"))
	default:
		return false
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
	if !asciifold.ContainsBytes(payload, codexUsageWarningMarkerLimitLeft) {
		return false
	}
	if !asciifold.ContainsBytes(payload, codexUsageWarningMarkerLessThan) {
		return false
	}
	return asciifold.ContainsBytes(payload, codexUsageWarningMarkerStatus) ||
		asciifold.ContainsBytes(payload, codexUsageWarningMarkerEscapedStatus)
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

func codexContentPartIsUsageLimitWarning(part gjson.Result) bool {
	if !part.Exists() {
		return false
	}
	if part.Type == gjson.String {
		return codexTextLooksLikeUsageLimitWarning(part.String())
	}
	if !part.IsObject() {
		return false
	}
	return codexTextLooksLikeUsageLimitWarning(part.Get("text").String())
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
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	return asciifold.Contains(text, "heads up") &&
		asciifold.Contains(text, "less than") &&
		asciifold.Contains(text, "limit left") &&
		asciifold.Contains(text, "/status")
}

func codexTextLooksLikeUsageLimitWarningBytes(text []byte) bool {
	return asciifold.ContainsBytes(text, "heads up") &&
		asciifold.ContainsBytes(text, codexUsageWarningMarkerLessThan) &&
		asciifold.ContainsBytes(text, codexUsageWarningMarkerLimitLeft) &&
		(asciifold.ContainsBytes(text, codexUsageWarningMarkerStatus) ||
			asciifold.ContainsBytes(text, codexUsageWarningMarkerEscapedStatus))
}

func codexTextMayBeUsageLimitWarningPrefix(text string) bool {
	return codexTextMayBeUsageLimitWarningPrefixValue(text)
}

func codexTextMayBeUsageLimitWarningPrefixBytes(text []byte) bool {
	return codexTextMayBeUsageLimitWarningPrefixValue(text)
}

func codexTextMayBeUsageLimitWarningPrefixValue[T ~string | ~[]byte](text T) bool {
	const marker = "heads up you have less than"
	if len(text) == 0 {
		return false
	}

	normalizedLen := 0
	lastSpace := false
	for i := 0; i < len(text); i++ {
		r := text[i]
		c := byte(0)
		switch {
		case r >= 'A' && r <= 'Z':
			c = r + ('a' - 'A')
			lastSpace = false
		case r >= 'a' && r <= 'z':
			c = r
			lastSpace = false
		case r >= '0' && r <= '9':
			c = r
			lastSpace = false
		case r == '/' || r == '%':
			c = r
			lastSpace = false
		case r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == ',' || r == '.':
			if lastSpace || normalizedLen == 0 {
				continue
			}
			c = ' '
			lastSpace = true
		default:
			continue
		}

		if normalizedLen >= len(marker) {
			return true
		}
		if marker[normalizedLen] != c {
			return false
		}
		normalizedLen++
		if normalizedLen == len(marker) {
			return true
		}
	}
	return normalizedLen > 0
}

func codexUsageWarningPrefixText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimLeft(text, "⚠!,.:- \t\r\n")
	if text == "" {
		return ""
	}
	var stack [128]byte
	out := stack[:0]
	lastSpace := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
			lastSpace = false
		case c >= 'a' && c <= 'z':
			out = append(out, c)
			lastSpace = false
		case c >= '0' && c <= '9':
			out = append(out, c)
			lastSpace = false
		case c == '/' || c == '%':
			out = append(out, c)
			lastSpace = false
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' || c == '.':
			if !lastSpace && len(out) > 0 {
				out = append(out, ' ')
				lastSpace = true
			}
		}
	}
	if lastSpace && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return string(out)
}
