package executor

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var dataTag = []byte("data:")

var (
	codexJSONTypeFirstFieldPrefix = []byte(`"type":"`)

	codexJSONKeyType        = []byte("type")
	codexJSONKeyInput       = []byte("input")
	codexJSONKeyGenerate    = []byte("generate")
	codexJSONKeyMetadata    = []byte("client_metadata")
	codexJSONKeyItemID      = []byte("item_id")
	codexJSONKeyCallID      = []byte("call_id")
	codexJSONKeyDelta       = []byte("delta")
	codexJSONKeyOutputIndex = []byte("output_index")
	codexJSONKeyPreviousID  = []byte("previous_response_id")

	codexJSONKeyMetadataTurn      = []byte(codexClientMetadataTurnMetadata)
	codexJSONKeyMetadataTrace     = []byte(codexWSClientMetadataTraceparent)
	codexJSONKeyMetadataTraceStat = []byte(codexWSClientMetadataTracestate)
	codexJSONKeyMetadataStartMS   = []byte(codexClientMetadataWSStreamRequestStartMS)

	codexJSONFieldItemID      = []byte(`"item_id":`)
	codexJSONFieldCallID      = []byte(`"call_id":`)
	codexJSONFieldDelta       = []byte(`"delta":`)
	codexJSONFieldOutputIndex = []byte(`"output_index":`)
	codexJSONFieldSequence    = []byte(`"sequence_number":`)

	codexEventOutputItemDoneValue             = []byte("response.output_item.done")
	codexEventOutputItemAddedValue            = []byte("response.output_item.added")
	codexEventFunctionCallArgumentsDeltaValue = []byte("response.function_call_arguments.delta")
	codexEventFunctionCallArgumentsDoneValue  = []byte("response.function_call_arguments.done")
	codexEventCustomToolCallInputDeltaValue   = []byte("response.custom_tool_call_input.delta")
	codexEventOutputTextDeltaValue            = []byte("response.output_text.delta")
	codexEventOutputTextDoneValue             = []byte("response.output_text.done")
	codexEventReasoningSummaryTextDeltaValue  = []byte("response.reasoning_summary_text.delta")
	codexEventReasoningSummaryTextDoneValue   = []byte("response.reasoning_summary_text.done")
	codexEventReasoningTextDeltaValue         = []byte("response.reasoning_text.delta")
	codexEventCompletedValue                  = []byte("response.completed")

	codexJSONFieldFunctionCallArgumentsDeltaType = []byte(`"type":"response.function_call_arguments.delta"`)
	codexJSONFieldCustomToolCallInputDeltaType   = []byte(`"type":"response.custom_tool_call_input.delta"`)

	codexJSONCompactFunctionCallDeltaType         = []byte(`{"type":"response.function_call_arguments.delta"`)
	codexJSONCompactCustomToolCallInputDeltaType  = []byte(`{"type":"response.custom_tool_call_input.delta"`)
	codexJSONCompactOutputTextDeltaType           = []byte(`{"type":"response.output_text.delta"`)
	codexJSONCompactReasoningSummaryTextDeltaType = []byte(`{"type":"response.reasoning_summary_text.delta"`)
	codexJSONCompactReasoningTextDeltaType        = []byte(`{"type":"response.reasoning_text.delta"`)
	codexJSONCompactOutputItemAddedType           = []byte(`{"type":"response.output_item.added"`)
	codexJSONCompactOutputItemDoneType            = []byte(`{"type":"response.output_item.done"`)
	codexJSONCompactFunctionCallDeltaItemIDPrefix = []byte(`{"type":"response.function_call_arguments.delta","item_id":`)
	codexJSONCompactFunctionCallDeltaSequencePref = []byte(`{"type":"response.function_call_arguments.delta","sequence_number":`)
	codexJSONCompactItemIDField                   = []byte(`,"item_id":`)
	codexJSONCompactOutputIndexField              = []byte(`,"output_index":`)
	codexJSONCompactDeltaField                    = []byte(`,"delta":`)
	codexJSONCompactSequenceField                 = []byte(`,"sequence_number":`)
)

const (
	codexEventOutputItemDone             = "response.output_item.done"
	codexEventOutputItemAdded            = "response.output_item.added"
	codexEventFunctionCallArgumentsDelta = "response.function_call_arguments.delta"
	codexEventFunctionCallArgumentsDone  = "response.function_call_arguments.done"
	codexEventCustomToolCallInputDelta   = "response.custom_tool_call_input.delta"
	codexEventOutputTextDelta            = "response.output_text.delta"
	codexEventOutputTextDone             = "response.output_text.done"
	codexEventReasoningSummaryTextDelta  = "response.reasoning_summary_text.delta"
	codexEventReasoningSummaryTextDone   = "response.reasoning_summary_text.done"
	codexEventReasoningTextDelta         = "response.reasoning_text.delta"
	codexEventCompleted                  = "response.completed"

	codexStreamArgumentBuilderInitialCapacity = 128

	codexCompactResponseTypeKindOffset = len(`{"type":"response.`)
)

const (
	codexStreamArgumentFieldUnknown uint8 = iota
	codexStreamArgumentFieldAlreadyParsed
	codexStreamArgumentFieldOutputIndex
	codexStreamArgumentFieldItemID
	codexStreamArgumentFieldCallID
	codexStreamArgumentFieldDelta
	codexStreamArgumentFieldSequence
)

type codexStreamFunctionCallState struct {
	ItemID           string
	CallID           string
	Name             string
	ItemType         string
	Arguments        string
	ActionRaw        string
	Execution        string
	Status           string
	argumentsBuilder strings.Builder
	OutputIndex      int64
}

type codexStreamCompletionState struct {
	outputItemsByIndex  map[int64][]byte
	outputItemsFallback [][]byte
	functionCallsByItem map[string]*codexStreamFunctionCallState
	// functionCallsByIndex indexes the same states by OutputIndex so that
	// events missing item_id can be resolved in O(1) instead of scanning
	// the entire map. Only populated when OutputIndex >= 0.
	functionCallsByIndex map[int64]*codexStreamFunctionCallState
}

type codexCompletedStreamEvent struct {
	data           []byte
	recoveredCount int
}

type codexRecoveredStreamItem struct {
	outputIndex int64
	raw         []byte
}

type codexStreamArgumentDeltaFields struct {
	itemID             []byte
	callID             []byte
	delta              []byte
	itemIDEscaped      bool
	callIDEscaped      bool
	deltaEscaped       bool
	outputIndex        int64
	hasOutputIndex     bool
	hasDelta           bool
	hasLookupCandidate bool
}

func newCodexStreamCompletionState() *codexStreamCompletionState {
	return &codexStreamCompletionState{}
}

func (s *codexStreamFunctionCallState) appendArgumentsDelta(delta string) {
	if s == nil || delta == "" {
		return
	}
	s.ensureArgumentsBuilderCapacity(len(delta))
	if s.Arguments != "" && s.argumentsBuilder.Len() == 0 {
		s.argumentsBuilder.WriteString(s.Arguments)
		s.Arguments = ""
	}
	s.argumentsBuilder.WriteString(delta)
}

func (s *codexStreamFunctionCallState) appendArgumentsDeltaBytes(delta []byte) {
	if s == nil || len(delta) == 0 {
		return
	}
	s.ensureArgumentsBuilderCapacity(len(delta))
	if s.Arguments != "" && s.argumentsBuilder.Len() == 0 {
		s.argumentsBuilder.WriteString(s.Arguments)
		s.Arguments = ""
	}
	_, _ = s.argumentsBuilder.Write(delta)
}

func (s *codexStreamFunctionCallState) ensureArgumentsBuilderCapacity(additional int) {
	if s == nil || s.argumentsBuilder.Len() > 0 {
		return
	}
	capacity := len(s.Arguments) + additional
	if capacity < codexStreamArgumentBuilderInitialCapacity {
		capacity = codexStreamArgumentBuilderInitialCapacity
	}
	s.argumentsBuilder.Grow(capacity)
}

func (s *codexStreamFunctionCallState) appendArgumentsDeltaRaw(delta []byte, escaped bool) bool {
	if len(delta) == 0 {
		return true
	}
	if !escaped {
		s.appendArgumentsDeltaBytes(delta)
		return true
	}
	unquoted, ok := codexUnquoteJSONStringValue(delta)
	if !ok {
		return false
	}
	s.appendArgumentsDelta(unquoted)
	return true
}

func (s *codexStreamFunctionCallState) setArguments(arguments string) {
	if s == nil {
		return
	}
	s.Arguments = arguments
	if s.argumentsBuilder.Len() > 0 {
		s.argumentsBuilder.Reset()
	}
}

func (s *codexStreamFunctionCallState) arguments() string {
	if s == nil {
		return ""
	}
	if s.Arguments != "" {
		return s.Arguments
	}
	if s.argumentsBuilder.Len() > 0 {
		return s.argumentsBuilder.String()
	}
	return ""
}

func (s *codexStreamCompletionState) functionCallByItem(itemID string, outputIndex int64) *codexStreamFunctionCallState {
	if s == nil {
		return nil
	}
	if itemID != "" {
		if state, ok := s.functionCallsByItem[itemID]; ok && state != nil {
			return state
		}
	}
	if outputIndex < 0 {
		return nil
	}
	if s.functionCallsByIndex != nil {
		if state, ok := s.functionCallsByIndex[outputIndex]; ok && state != nil {
			return state
		}
	}
	// Defensive fallback: the index map should be authoritative, but if it
	// was somehow not populated (e.g. older instances before this field
	// existed) fall back to a linear scan so correctness is preserved.
	for _, state := range s.functionCallsByItem {
		if state != nil && state.OutputIndex == outputIndex {
			return state
		}
	}
	return nil
}

func (s *codexStreamCompletionState) functionCallForEvent(eventData []byte) *codexStreamFunctionCallState {
	itemID := strings.TrimSpace(gjson.GetBytes(eventData, "item_id").String())
	outputIndex := codexStreamEventOutputIndex(eventData)
	if state := s.functionCallByItem(itemID, outputIndex); state != nil {
		return state
	}
	callID := strings.TrimSpace(gjson.GetBytes(eventData, "call_id").String())
	if key := codexStreamToolCallStateKey(itemID, callID); key != "" {
		return s.functionCallsByItem[key]
	}
	return nil
}

func codexEventData(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, dataTag) {
		return nil, false
	}
	return bytes.TrimSpace(line[len(dataTag):]), true
}

func codexSSEDataLine(data []byte) []byte {
	line := make([]byte, 0, len(dataTag)+1+len(data))
	line = append(line, dataTag...)
	line = append(line, ' ')
	line = append(line, data...)
	return line
}

func codexEventType(eventData []byte) string {
	if len(eventData) == 0 {
		return ""
	}
	if eventType, ok := codexCompactKnownEventType(eventData); ok {
		return eventType
	}
	if raw, ok := codexFirstFieldEventTypeRaw(eventData); ok {
		if eventType, ok := codexKnownEventTypeRaw(raw); ok {
			return eventType
		}
		return string(raw)
	}
	if raw, escaped, ok := codexTopLevelJSONStringRaw(eventData, codexJSONKeyType); ok {
		if !escaped {
			if eventType, ok := codexKnownEventTypeRaw(raw); ok {
				return eventType
			}
			return string(raw)
		}
		if eventType, ok := codexUnquoteJSONStringValue(raw); ok {
			return eventType
		}
	}
	return gjson.GetBytes(eventData, "type").String()
}

func codexCompactKnownEventType(data []byte) (string, bool) {
	if len(data) <= codexCompactResponseTypeKindOffset || data[0] != '{' {
		return "", false
	}
	switch data[codexCompactResponseTypeKindOffset] {
	case 'c':
		if codexHasCompactJSONFieldPrefix(data, codexJSONCompactCustomToolCallInputDeltaType) {
			return codexEventCustomToolCallInputDelta, true
		}
	case 'f':
		if codexHasCompactJSONFieldPrefix(data, codexJSONCompactFunctionCallDeltaType) {
			return codexEventFunctionCallArgumentsDelta, true
		}
	case 'o':
		switch {
		case codexHasCompactJSONFieldPrefix(data, codexJSONCompactOutputTextDeltaType):
			return codexEventOutputTextDelta, true
		case codexHasCompactJSONFieldPrefix(data, codexJSONCompactOutputItemAddedType):
			return codexEventOutputItemAdded, true
		case codexHasCompactJSONFieldPrefix(data, codexJSONCompactOutputItemDoneType):
			return codexEventOutputItemDone, true
		}
	case 'r':
		switch {
		case codexHasCompactJSONFieldPrefix(data, codexJSONCompactReasoningSummaryTextDeltaType):
			return codexEventReasoningSummaryTextDelta, true
		case codexHasCompactJSONFieldPrefix(data, codexJSONCompactReasoningTextDeltaType):
			return codexEventReasoningTextDelta, true
		}
	}
	return "", false
}

func codexHasCompactJSONFieldPrefix(data []byte, prefix []byte) bool {
	if !bytes.HasPrefix(data, prefix) || len(data) == len(prefix) {
		return false
	}
	switch data[len(prefix)] {
	case ',', '}', ' ', '\n', '\r', '\t':
		return true
	default:
		return false
	}
}

func codexFirstFieldEventTypeRaw(data []byte) ([]byte, bool) {
	i := codexSkipJSONSpaces(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, false
	}
	i = codexSkipJSONSpaces(data, i+1)
	if len(data)-i < len(codexJSONTypeFirstFieldPrefix) || !bytes.Equal(data[i:i+len(codexJSONTypeFirstFieldPrefix)], codexJSONTypeFirstFieldPrefix) {
		return nil, false
	}
	start := i + len(codexJSONTypeFirstFieldPrefix)
	for j := start; j < len(data); j++ {
		switch data[j] {
		case '\\':
			return nil, false
		case '"':
			return data[start:j], true
		default:
			if data[j] < 0x20 {
				return nil, false
			}
		}
	}
	return nil, false
}

func (s *codexStreamCompletionState) recordEvent(eventData []byte) {
	s.recordEventWithType(codexEventType(eventData), eventData)
}

func (s *codexStreamCompletionState) recordEventWithType(eventType string, eventData []byte) {
	if s == nil || len(eventData) == 0 {
		return
	}
	if codexShouldSuppressUsageWarningEvent(eventType, eventData) {
		return
	}

	switch eventType {
	case codexEventOutputItemDone:
		itemResult := gjson.GetBytes(eventData, "item")
		if !itemResult.Exists() || itemResult.Type != gjson.JSON {
			return
		}
		itemBytes := []byte(itemResult.Raw)
		outputIndexResult := gjson.GetBytes(eventData, "output_index")
		if outputIndexResult.Exists() {
			if s.outputItemsByIndex == nil {
				s.outputItemsByIndex = make(map[int64][]byte)
			}
			s.outputItemsByIndex[outputIndexResult.Int()] = itemBytes
			return
		}
		s.outputItemsFallback = append(s.outputItemsFallback, itemBytes)
	case codexEventOutputItemAdded:
		item := gjson.GetBytes(eventData, "item")
		itemType := strings.TrimSpace(item.Get("type").String())
		if !item.Exists() || (itemType != "function_call" && itemType != "custom_tool_call" && itemType != "local_shell_call" && itemType != "tool_search_call") {
			return
		}
		outputIndex := codexStreamEventOutputIndex(eventData)
		itemID := strings.TrimSpace(item.Get("id").String())
		callID := strings.TrimSpace(item.Get("call_id").String())
		stateKey := codexStreamToolCallStateKey(itemID, callID)
		state := s.functionCallByItem(itemID, outputIndex)
		if state == nil && stateKey != "" {
			state = s.functionCallsByItem[stateKey]
		}
		if state == nil {
			state = &codexStreamFunctionCallState{
				ItemID:      itemID,
				OutputIndex: outputIndex,
			}
			if stateKey == "" {
				if outputIndex < 0 {
					return
				}
				stateKey = fmt.Sprintf("idx:%d", outputIndex)
			}
			if s.functionCallsByItem == nil {
				s.functionCallsByItem = make(map[string]*codexStreamFunctionCallState)
			}
			s.functionCallsByItem[stateKey] = state
			if outputIndex >= 0 {
				if s.functionCallsByIndex == nil {
					s.functionCallsByIndex = make(map[int64]*codexStreamFunctionCallState)
				}
				s.functionCallsByIndex[outputIndex] = state
			}
		}
		if itemID != "" {
			state.ItemID = itemID
		}
		if itemType != "" {
			state.ItemType = itemType
		}
		if callID != "" {
			state.CallID = callID
		}
		if name := strings.TrimSpace(item.Get("name").String()); name != "" {
			state.Name = name
		}
		if status := strings.TrimSpace(item.Get("status").String()); status != "" {
			state.Status = status
		}
		if execution := strings.TrimSpace(item.Get("execution").String()); execution != "" {
			state.Execution = execution
		}
		if itemType == "custom_tool_call" {
			if input := item.Get("input"); input.Exists() && input.Type == gjson.String && input.String() != "" {
				state.setArguments(input.String())
			}
		} else if itemType == "local_shell_call" {
			if action := item.Get("action"); action.Exists() && action.Type == gjson.JSON {
				state.ActionRaw = action.Raw
			}
		} else if itemType == "tool_search_call" {
			if arguments := item.Get("arguments"); arguments.Exists() && arguments.Type == gjson.JSON {
				state.setArguments(arguments.Raw)
			}
		}
	case codexEventFunctionCallArgumentsDelta, codexEventCustomToolCallInputDelta:
		if s.recordArgumentsDeltaFast(eventType, eventData) {
			return
		}
		state := s.functionCallForEvent(eventData)
		if state == nil {
			return
		}
		if eventType == codexEventCustomToolCallInputDelta {
			state.ItemType = "custom_tool_call"
		}
		state.appendArgumentsDelta(gjson.GetBytes(eventData, "delta").String())
	case codexEventFunctionCallArgumentsDone:
		state := s.functionCallForEvent(eventData)
		if state == nil {
			return
		}
		if arguments := gjson.GetBytes(eventData, "arguments").String(); arguments != "" {
			state.setArguments(arguments)
		}
	}
}

func (s *codexStreamCompletionState) recordArgumentsDeltaFast(eventType string, eventData []byte) bool {
	if s.recordFunctionCallArgumentsDeltaByIndexFast(eventType, eventData) {
		return true
	}

	fields, ok := parseCodexStreamArgumentDeltaFields(eventData)
	if !ok {
		return false
	}

	outputIndex := int64(-1)
	if fields.hasOutputIndex {
		outputIndex = fields.outputIndex
	}

	var state *codexStreamFunctionCallState
	if fields.hasOutputIndex && s.functionCallsByIndex != nil {
		state = s.functionCallsByIndex[fields.outputIndex]
	}
	if state == nil && len(fields.itemID) > 0 {
		itemID, ok := codexJSONStringValue(fields.itemID, fields.itemIDEscaped)
		if !ok {
			return false
		}
		itemID = strings.TrimSpace(itemID)
		if itemID != "" {
			state = s.functionCallByItem(itemID, outputIndex)
		}
	}
	if state == nil && len(fields.callID) > 0 {
		callID, ok := codexJSONStringValue(fields.callID, fields.callIDEscaped)
		if !ok {
			return false
		}
		if key := codexStreamToolCallStateKey("", callID); key != "" {
			state = s.functionCallsByItem[key]
		}
	}
	if state == nil {
		return true
	}

	if eventType == codexEventCustomToolCallInputDelta {
		state.ItemType = "custom_tool_call"
	}
	if !fields.hasDelta {
		return true
	}
	return state.appendArgumentsDeltaRaw(fields.delta, fields.deltaEscaped)
}

func (s *codexStreamCompletionState) recordFunctionCallArgumentsDeltaByIndexFast(eventType string, eventData []byte) bool {
	if s == nil || eventType != codexEventFunctionCallArgumentsDelta || s.functionCallsByIndex == nil {
		return false
	}

	i := 0
	switch {
	case bytes.HasPrefix(eventData, codexJSONCompactFunctionCallDeltaItemIDPrefix):
		i = len(codexJSONCompactFunctionCallDeltaItemIDPrefix)
	case bytes.HasPrefix(eventData, codexJSONCompactFunctionCallDeltaSequencePref):
		i = len(codexJSONCompactFunctionCallDeltaSequencePref)
		_, next, ok := codexParseJSONInt(eventData, i)
		if !ok {
			return false
		}
		i = next
		if !bytes.HasPrefix(eventData[i:], codexJSONCompactItemIDField) {
			return false
		}
		i += len(codexJSONCompactItemIDField)
	default:
		return false
	}

	_, _, _, next, ok := codexParseJSONStringRaw(eventData, i)
	if !ok {
		return false
	}
	i = next
	if !bytes.HasPrefix(eventData[i:], codexJSONCompactOutputIndexField) {
		return false
	}
	i += len(codexJSONCompactOutputIndexField)
	outputIndex, next, ok := codexParseJSONInt(eventData, i)
	if !ok {
		return false
	}
	state := s.functionCallsByIndex[outputIndex]
	if state == nil {
		return false
	}

	i = next
	if !bytes.HasPrefix(eventData[i:], codexJSONCompactDeltaField) {
		return false
	}
	i += len(codexJSONCompactDeltaField)
	delta, escaped, next, isNull, ok := codexParseOptionalJSONStringRaw(eventData, i)
	if !ok {
		return false
	}

	i = next
	if bytes.HasPrefix(eventData[i:], codexJSONCompactSequenceField) {
		i += len(codexJSONCompactSequenceField)
		_, next, ok = codexParseJSONInt(eventData, i)
		if !ok {
			return false
		}
		i = next
	}
	i = codexSkipJSONSpaces(eventData, i)
	if i >= len(eventData) || eventData[i] != '}' {
		return false
	}
	i = codexSkipJSONSpaces(eventData, i+1)
	if i != len(eventData) {
		return false
	}
	if isNull {
		return true
	}
	return state.appendArgumentsDeltaRaw(delta, escaped)
}

func codexStreamToolCallStateKey(itemID, callID string) string {
	itemID = strings.TrimSpace(itemID)
	if itemID != "" {
		return itemID
	}
	return strings.TrimSpace(callID)
}

func parseCodexStreamArgumentDeltaFields(data []byte) (codexStreamArgumentDeltaFields, bool) {
	fields := codexStreamArgumentDeltaFields{outputIndex: -1}
	i := codexSkipJSONSpaces(data, 0)
	if i >= len(data) || data[i] != '{' {
		return fields, false
	}
	i++

	for {
		i = codexSkipJSONSpaces(data, i)
		if i >= len(data) {
			return fields, false
		}
		if data[i] == '}' {
			return fields, fields.hasLookupCandidate || fields.hasDelta
		}

		field := codexStreamArgumentFieldUnknown
		matched := false
		if i+1 < len(data) && data[i] == '"' {
			switch data[i+1] {
			case 'c':
				if bytes.HasPrefix(data[i:], codexJSONFieldCallID) {
					field = codexStreamArgumentFieldCallID
					i += len(codexJSONFieldCallID)
					matched = true
				}
			case 'd':
				if bytes.HasPrefix(data[i:], codexJSONFieldDelta) {
					field = codexStreamArgumentFieldDelta
					i += len(codexJSONFieldDelta)
					matched = true
				}
			case 'i':
				if bytes.HasPrefix(data[i:], codexJSONFieldItemID) {
					field = codexStreamArgumentFieldItemID
					i += len(codexJSONFieldItemID)
					matched = true
				}
			case 'o':
				if bytes.HasPrefix(data[i:], codexJSONFieldOutputIndex) {
					field = codexStreamArgumentFieldOutputIndex
					i += len(codexJSONFieldOutputIndex)
					matched = true
				}
			case 's':
				if bytes.HasPrefix(data[i:], codexJSONFieldSequence) {
					field = codexStreamArgumentFieldSequence
					i += len(codexJSONFieldSequence)
					matched = true
				}
			case 't':
				switch {
				case bytes.HasPrefix(data[i:], codexJSONFieldFunctionCallArgumentsDeltaType):
					field = codexStreamArgumentFieldAlreadyParsed
					i += len(codexJSONFieldFunctionCallArgumentsDeltaType)
					matched = true
				case bytes.HasPrefix(data[i:], codexJSONFieldCustomToolCallInputDeltaType):
					field = codexStreamArgumentFieldAlreadyParsed
					i += len(codexJSONFieldCustomToolCallInputDeltaType)
					matched = true
				}
			}
		}
		if !matched {
			keyStart, keyEnd, keyEscaped, next, ok := codexParseJSONStringRaw(data, i)
			if !ok || keyEscaped {
				return fields, false
			}
			i = codexSkipJSONSpaces(data, next)
			if i >= len(data) || data[i] != ':' {
				return fields, false
			}
			i++
			switch key := data[keyStart:keyEnd]; {
			case bytes.Equal(key, codexJSONKeyOutputIndex):
				field = codexStreamArgumentFieldOutputIndex
			case bytes.Equal(key, codexJSONKeyItemID):
				field = codexStreamArgumentFieldItemID
			case bytes.Equal(key, codexJSONKeyCallID):
				field = codexStreamArgumentFieldCallID
			case bytes.Equal(key, codexJSONKeyDelta):
				field = codexStreamArgumentFieldDelta
			}
		}

		if field != codexStreamArgumentFieldAlreadyParsed {
			i = codexSkipJSONSpaces(data, i)
			if i >= len(data) {
				return fields, false
			}
		}

		switch field {
		case codexStreamArgumentFieldAlreadyParsed:
		case codexStreamArgumentFieldOutputIndex:
			value, valueNext, ok := codexParseJSONInt(data, i)
			if !ok {
				return fields, false
			}
			fields.outputIndex = value
			fields.hasOutputIndex = true
			fields.hasLookupCandidate = true
			i = valueNext
		case codexStreamArgumentFieldItemID:
			raw, escaped, valueNext, isNull, ok := codexParseOptionalJSONStringRaw(data, i)
			if !ok {
				return fields, false
			}
			if !isNull {
				fields.itemID = raw
				fields.itemIDEscaped = escaped
				fields.hasLookupCandidate = true
			}
			i = valueNext
		case codexStreamArgumentFieldCallID:
			raw, escaped, valueNext, isNull, ok := codexParseOptionalJSONStringRaw(data, i)
			if !ok {
				return fields, false
			}
			if !isNull {
				fields.callID = raw
				fields.callIDEscaped = escaped
				fields.hasLookupCandidate = true
			}
			i = valueNext
		case codexStreamArgumentFieldDelta:
			raw, escaped, valueNext, isNull, ok := codexParseOptionalJSONStringRaw(data, i)
			if !ok {
				return fields, false
			}
			if !isNull {
				fields.delta = raw
				fields.deltaEscaped = escaped
				fields.hasDelta = true
			}
			i = valueNext
		case codexStreamArgumentFieldSequence:
			_, valueNext, ok := codexParseJSONInt(data, i)
			if !ok {
				return fields, false
			}
			i = valueNext
		default:
			valueNext, ok := codexSkipJSONValue(data, i)
			if !ok {
				return fields, false
			}
			i = valueNext
		}

		i = codexSkipJSONSpaces(data, i)
		if i >= len(data) {
			return fields, false
		}
		switch data[i] {
		case ',':
			i++
		case '}':
			return fields, fields.hasLookupCandidate || fields.hasDelta
		default:
			return fields, false
		}
	}
}

func codexTopLevelJSONStringRaw(data []byte, targetKey []byte) (raw []byte, escaped bool, ok bool) {
	i := codexSkipJSONSpaces(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, false, false
	}
	i++

	for {
		i = codexSkipJSONSpaces(data, i)
		if i >= len(data) || data[i] == '}' {
			return nil, false, false
		}

		keyStart, keyEnd, keyEscaped, next, ok := codexParseJSONStringRaw(data, i)
		if !ok || keyEscaped {
			return nil, false, false
		}
		i = codexSkipJSONSpaces(data, next)
		if i >= len(data) || data[i] != ':' {
			return nil, false, false
		}
		i = codexSkipJSONSpaces(data, i+1)
		if i >= len(data) {
			return nil, false, false
		}

		if bytes.Equal(data[keyStart:keyEnd], targetKey) {
			raw, escaped, _, isNull, ok := codexParseOptionalJSONStringRaw(data, i)
			if !ok || isNull {
				return nil, false, false
			}
			return raw, escaped, true
		}

		valueNext, ok := codexSkipJSONValue(data, i)
		if !ok {
			return nil, false, false
		}
		i = codexSkipJSONSpaces(data, valueNext)
		if i >= len(data) {
			return nil, false, false
		}
		switch data[i] {
		case ',':
			i++
		case '}':
			return nil, false, false
		default:
			return nil, false, false
		}
	}
}

func codexKnownEventTypeRaw(raw []byte) (string, bool) {
	switch {
	case bytes.Equal(raw, codexEventFunctionCallArgumentsDeltaValue):
		return codexEventFunctionCallArgumentsDelta, true
	case bytes.Equal(raw, codexEventCustomToolCallInputDeltaValue):
		return codexEventCustomToolCallInputDelta, true
	case bytes.Equal(raw, codexEventOutputTextDeltaValue):
		return codexEventOutputTextDelta, true
	case bytes.Equal(raw, codexEventOutputTextDoneValue):
		return codexEventOutputTextDone, true
	case bytes.Equal(raw, codexEventReasoningSummaryTextDeltaValue):
		return codexEventReasoningSummaryTextDelta, true
	case bytes.Equal(raw, codexEventReasoningSummaryTextDoneValue):
		return codexEventReasoningSummaryTextDone, true
	case bytes.Equal(raw, codexEventReasoningTextDeltaValue):
		return codexEventReasoningTextDelta, true
	case bytes.Equal(raw, codexEventCompletedValue):
		return codexEventCompleted, true
	case bytes.Equal(raw, codexEventOutputItemDoneValue):
		return codexEventOutputItemDone, true
	case bytes.Equal(raw, codexEventOutputItemAddedValue):
		return codexEventOutputItemAdded, true
	case bytes.Equal(raw, codexEventFunctionCallArgumentsDoneValue):
		return codexEventFunctionCallArgumentsDone, true
	default:
		return "", false
	}
}

func codexJSONStringValue(raw []byte, escaped bool) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	if !escaped {
		return string(raw), true
	}
	return codexUnquoteJSONStringValue(raw)
}

func codexUnquoteJSONStringValue(raw []byte) (string, bool) {
	quoted := make([]byte, 0, len(raw)+2)
	quoted = append(quoted, '"')
	quoted = append(quoted, raw...)
	quoted = append(quoted, '"')
	value, err := strconv.Unquote(string(quoted))
	if err != nil {
		return "", false
	}
	return value, true
}

func codexParseOptionalJSONStringRaw(data []byte, i int) (raw []byte, escaped bool, next int, isNull bool, ok bool) {
	i = codexSkipJSONSpaces(data, i)
	if codexHasJSONLiteral(data, i, "null") {
		return nil, false, i + 4, true, true
	}
	start, end, escaped, next, ok := codexParseJSONStringRaw(data, i)
	if !ok {
		return nil, false, 0, false, false
	}
	return data[start:end], escaped, next, false, true
}

func codexParseJSONStringRaw(data []byte, i int) (start int, end int, escaped bool, next int, ok bool) {
	if i >= len(data) || data[i] != '"' {
		return 0, 0, false, 0, false
	}
	start = i + 1
	for j := start; j < len(data); j++ {
		switch data[j] {
		case '\\':
			escaped = true
			j++
			if j >= len(data) {
				return 0, 0, false, 0, false
			}
		case '"':
			return start, j, escaped, j + 1, true
		default:
			if data[j] < 0x20 {
				return 0, 0, false, 0, false
			}
		}
	}
	return 0, 0, false, 0, false
}

func codexParseJSONInt(data []byte, i int) (int64, int, bool) {
	i = codexSkipJSONSpaces(data, i)
	if i >= len(data) || data[i] < '0' || data[i] > '9' {
		return 0, 0, false
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	var value int64
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		digit := int64(data[i] - '0')
		if value > (maxInt64-digit)/10 {
			return 0, 0, false
		}
		value = value*10 + digit
		i++
	}
	return value, i, true
}

func codexSkipJSONSpaces(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func codexSkipJSONValue(data []byte, i int) (int, bool) {
	i = codexSkipJSONSpaces(data, i)
	if i >= len(data) {
		return 0, false
	}
	switch data[i] {
	case '"':
		_, _, _, next, ok := codexParseJSONStringRaw(data, i)
		return next, ok
	case '{', '[':
		return codexSkipJSONComposite(data, i)
	case 't':
		if codexHasJSONLiteral(data, i, "true") {
			return i + 4, true
		}
	case 'f':
		if codexHasJSONLiteral(data, i, "false") {
			return i + 5, true
		}
	case 'n':
		if codexHasJSONLiteral(data, i, "null") {
			return i + 4, true
		}
	default:
		return codexSkipJSONNumber(data, i)
	}
	return 0, false
}

func codexHasJSONLiteral(data []byte, i int, literal string) bool {
	if i < 0 || i+len(literal) > len(data) {
		return false
	}
	for j := 0; j < len(literal); j++ {
		if data[i+j] != literal[j] {
			return false
		}
	}
	return true
}

func codexSkipJSONComposite(data []byte, i int) (int, bool) {
	var stack [32]byte
	depth := 0
	for i < len(data) {
		switch data[i] {
		case '"':
			_, _, _, next, ok := codexParseJSONStringRaw(data, i)
			if !ok {
				return 0, false
			}
			i = next
			continue
		case '{':
			if depth == len(stack) {
				return 0, false
			}
			stack[depth] = '}'
			depth++
		case '[':
			if depth == len(stack) {
				return 0, false
			}
			stack[depth] = ']'
			depth++
		case '}', ']':
			if depth == 0 || data[i] != stack[depth-1] {
				return 0, false
			}
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
		i++
	}
	return 0, false
}

func codexSkipJSONNumber(data []byte, i int) (int, bool) {
	start := i
	if i < len(data) && data[i] == '-' {
		i++
	}
	hasDigit := false
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		i++
		hasDigit = true
	}
	if i < len(data) && data[i] == '.' {
		i++
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
			hasDigit = true
		}
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		expDigits := false
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
			expDigits = true
		}
		if !expDigits {
			return 0, false
		}
	}
	return i, hasDigit && i > start
}

func codexStreamEventOutputIndex(eventData []byte) int64 {
	outputIndex := gjson.GetBytes(eventData, "output_index")
	if !outputIndex.Exists() {
		return -1
	}
	return outputIndex.Int()
}

func (s *codexStreamCompletionState) processEventData(eventData []byte, patchCompleted bool) (codexCompletedStreamEvent, bool) {
	return s.processEventDataWithType(codexEventType(eventData), eventData, patchCompleted)
}

func (s *codexStreamCompletionState) processEventDataWithType(eventType string, eventData []byte, patchCompleted bool) (codexCompletedStreamEvent, bool) {
	if s == nil || len(eventData) == 0 {
		return codexCompletedStreamEvent{}, false
	}

	if eventType == codexEventCompleted {
		if scrubbed, removed := scrubCodexCompletedUsageWarnings(eventData); removed > 0 {
			eventData = scrubbed
		}
	}
	s.recordEventWithType(eventType, eventData)
	if eventType != codexEventCompleted {
		return codexCompletedStreamEvent{}, false
	}

	completed := codexCompletedStreamEvent{data: eventData}
	if patchCompleted {
		if patched, recoveredCount := s.patchCompletedOutputIfEmpty(eventData); recoveredCount > 0 {
			completed.data = patched
			completed.recoveredCount = recoveredCount
		}
	}
	return completed, true
}

func (s *codexStreamCompletionState) patchCompletedOutputIfEmpty(completedData []byte) ([]byte, int) {
	if s == nil || len(completedData) == 0 {
		return completedData, 0
	}

	outputResult := gjson.GetBytes(completedData, "response.output")
	if outputResult.Exists() && outputResult.IsArray() && outputResult.Get("#").Int() > 0 {
		return completedData, 0
	}

	if len(s.functionCallsByItem) == 0 {
		return s.patchCompletedOutputFromRecordedItemsOnly(completedData, outputResult)
	}

	recovered := make([]codexRecoveredStreamItem, 0, len(s.outputItemsByIndex)+len(s.outputItemsFallback)+len(s.functionCallsByItem))
	seenCallIDs := make(map[string]struct{}, len(s.functionCallsByItem))
	seenItemIDs := make(map[string]struct{}, len(s.functionCallsByItem))

	indexes := make([]int64, 0, len(s.outputItemsByIndex))
	for idx := range s.outputItemsByIndex {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	for _, idx := range indexes {
		raw := s.outputItemsByIndex[idx]
		recovered = append(recovered, codexRecoveredStreamItem{outputIndex: idx, raw: raw})
		if callID := strings.TrimSpace(gjson.GetBytes(raw, "call_id").String()); callID != "" {
			seenCallIDs[callID] = struct{}{}
		}
		if itemID := strings.TrimSpace(gjson.GetBytes(raw, "id").String()); itemID != "" {
			seenItemIDs[itemID] = struct{}{}
		}
	}
	for _, raw := range s.outputItemsFallback {
		recovered = append(recovered, codexRecoveredStreamItem{outputIndex: int64(len(indexes) + len(recovered)), raw: raw})
		if callID := strings.TrimSpace(gjson.GetBytes(raw, "call_id").String()); callID != "" {
			seenCallIDs[callID] = struct{}{}
		}
		if itemID := strings.TrimSpace(gjson.GetBytes(raw, "id").String()); itemID != "" {
			seenItemIDs[itemID] = struct{}{}
		}
	}

	if len(s.functionCallsByItem) > 0 {
		keys := make([]string, 0, len(s.functionCallsByItem))
		for key := range s.functionCallsByItem {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			left := s.functionCallsByItem[keys[i]]
			right := s.functionCallsByItem[keys[j]]
			if left == nil || right == nil {
				return keys[i] < keys[j]
			}
			if left.OutputIndex != right.OutputIndex {
				return left.OutputIndex < right.OutputIndex
			}
			return keys[i] < keys[j]
		})
		for _, key := range keys {
			state := s.functionCallsByItem[key]
			if state == nil {
				continue
			}
			if strings.TrimSpace(state.CallID) == "" && state.ItemType != "local_shell_call" && state.ItemType != "tool_search_call" {
				continue
			}
			if state.ItemType == "local_shell_call" && strings.TrimSpace(state.ActionRaw) == "" {
				continue
			}
			if state.ItemType == "tool_search_call" && strings.TrimSpace(state.Execution) == "" {
				continue
			}
			if strings.TrimSpace(state.CallID) != "" {
				if _, ok := seenCallIDs[state.CallID]; ok {
					continue
				}
			}
			if _, ok := seenItemIDs[state.ItemID]; ok {
				continue
			}

			args := state.arguments()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			itemID := state.ItemID
			if strings.TrimSpace(itemID) == "" && state.ItemType != "custom_tool_call" {
				itemID = fmt.Sprintf("fc_%s", state.CallID)
			}

			item := buildCodexCompletedToolCallItem(itemID, state.CallID, state.Name, state.ItemType, args)
			if state.ItemType == "local_shell_call" {
				item = buildCodexCompletedLocalShellCallItem(itemID, state.CallID, state.ActionRaw, state.Status)
			} else if state.ItemType == "tool_search_call" {
				item = buildCodexCompletedToolSearchCallItem(itemID, state.CallID, state.Execution, state.Status, args)
			}
			recovered = append(recovered, codexRecoveredStreamItem{outputIndex: state.OutputIndex, raw: item})
			if strings.TrimSpace(state.CallID) != "" {
				seenCallIDs[state.CallID] = struct{}{}
			}
			if itemID != "" {
				seenItemIDs[itemID] = struct{}{}
			}
		}
	}

	if len(recovered) == 0 {
		return completedData, 0
	}

	sort.SliceStable(recovered, func(i, j int) bool {
		return recovered[i].outputIndex < recovered[j].outputIndex
	})

	patched := patchCodexCompletedOutputWithRecoveredItemsAtResult(completedData, outputResult, recovered)
	return patched, len(recovered)
}

func (s *codexStreamCompletionState) patchCompletedOutputFromRecordedItemsOnly(completedData []byte, outputResult gjson.Result) ([]byte, int) {
	totalItems := len(s.outputItemsByIndex) + len(s.outputItemsFallback)
	if totalItems == 0 {
		return completedData, 0
	}

	if len(s.outputItemsFallback) == 0 && len(s.outputItemsByIndex) == 1 {
		for _, raw := range s.outputItemsByIndex {
			return patchCodexCompletedOutputWithSingleItemAtResult(completedData, outputResult, raw), 1
		}
	}
	if len(s.outputItemsByIndex) == 0 {
		return patchCodexCompletedOutputWithItemsAtResult(completedData, outputResult, s.outputItemsFallback), len(s.outputItemsFallback)
	}

	items := make([][]byte, 0, totalItems)
	indexes := make([]int64, 0, len(s.outputItemsByIndex))
	for idx := range s.outputItemsByIndex {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	for _, idx := range indexes {
		items = append(items, s.outputItemsByIndex[idx])
	}
	items = append(items, s.outputItemsFallback...)
	return patchCodexCompletedOutputWithItemsAtResult(completedData, outputResult, items), totalItems
}

func patchCodexCompletedOutputWithSingleItem(completedData []byte, item []byte) []byte {
	outputResult := gjson.GetBytes(completedData, "response.output")
	return patchCodexCompletedOutputWithSingleItemAtResult(completedData, outputResult, item)
}

func patchCodexCompletedOutputWithSingleItemAtResult(completedData []byte, outputResult gjson.Result, item []byte) []byte {
	if patched, ok := patchCodexCompletedOutputSingleItemRaw(completedData, outputResult, item); ok {
		return patched
	}
	outputArray := make([]byte, 0, len(item)+2)
	outputArray = append(outputArray, '[')
	outputArray = append(outputArray, item...)
	outputArray = append(outputArray, ']')
	return patchCodexCompletedOutputArrayAtResult(completedData, outputResult, outputArray)
}

func patchCodexCompletedOutputWithItems(completedData []byte, items [][]byte) []byte {
	outputResult := gjson.GetBytes(completedData, "response.output")
	return patchCodexCompletedOutputWithItemsAtResult(completedData, outputResult, items)
}

func patchCodexCompletedOutputWithItemsAtResult(completedData []byte, outputResult gjson.Result, items [][]byte) []byte {
	if len(items) == 0 {
		return completedData
	}
	if len(items) == 1 {
		return patchCodexCompletedOutputWithSingleItemAtResult(completedData, outputResult, items[0])
	}
	if patched, ok := patchCodexCompletedOutputItemsRaw(completedData, outputResult, items); ok {
		return patched
	}

	totalLen := 2 + len(items) - 1
	for _, item := range items {
		totalLen += len(item)
	}
	outputArray := make([]byte, 0, totalLen)
	outputArray = append(outputArray, '[')
	for i, item := range items {
		if i > 0 {
			outputArray = append(outputArray, ',')
		}
		outputArray = append(outputArray, item...)
	}
	outputArray = append(outputArray, ']')
	return patchCodexCompletedOutputArrayAtResult(completedData, outputResult, outputArray)
}

func patchCodexCompletedOutputWithRecoveredItemsAtResult(completedData []byte, outputResult gjson.Result, items []codexRecoveredStreamItem) []byte {
	if len(items) == 0 {
		return completedData
	}
	if len(items) == 1 {
		return patchCodexCompletedOutputWithSingleItemAtResult(completedData, outputResult, items[0].raw)
	}
	if patched, ok := patchCodexCompletedOutputRecoveredItemsRaw(completedData, outputResult, items); ok {
		return patched
	}

	totalLen := 2 + len(items) - 1
	for _, item := range items {
		totalLen += len(item.raw)
	}
	outputArray := make([]byte, 0, totalLen)
	outputArray = append(outputArray, '[')
	for i, item := range items {
		if i > 0 {
			outputArray = append(outputArray, ',')
		}
		outputArray = append(outputArray, item.raw...)
	}
	outputArray = append(outputArray, ']')
	return patchCodexCompletedOutputArrayAtResult(completedData, outputResult, outputArray)
}

func patchCodexCompletedOutputSingleItemRaw(data []byte, result gjson.Result, item []byte) ([]byte, bool) {
	start, end, ok := codexJSONResultRawRange(data, result)
	if !ok {
		return nil, false
	}
	patched := make([]byte, 0, len(data)-(end-start)+len(item)+2)
	patched = append(patched, data[:start]...)
	patched = append(patched, '[')
	patched = append(patched, item...)
	patched = append(patched, ']')
	patched = append(patched, data[end:]...)
	return patched, true
}

func patchCodexCompletedOutputItemsRaw(data []byte, result gjson.Result, items [][]byte) ([]byte, bool) {
	start, end, ok := codexJSONResultRawRange(data, result)
	if !ok {
		return nil, false
	}
	totalLen := 2 + len(items) - 1
	for _, item := range items {
		totalLen += len(item)
	}
	patched := make([]byte, 0, len(data)-(end-start)+totalLen)
	patched = append(patched, data[:start]...)
	patched = append(patched, '[')
	for i, item := range items {
		if i > 0 {
			patched = append(patched, ',')
		}
		patched = append(patched, item...)
	}
	patched = append(patched, ']')
	patched = append(patched, data[end:]...)
	return patched, true
}

func patchCodexCompletedOutputRecoveredItemsRaw(data []byte, result gjson.Result, items []codexRecoveredStreamItem) ([]byte, bool) {
	start, end, ok := codexJSONResultRawRange(data, result)
	if !ok {
		return nil, false
	}
	totalLen := 2 + len(items) - 1
	for _, item := range items {
		totalLen += len(item.raw)
	}
	patched := make([]byte, 0, len(data)-(end-start)+totalLen)
	patched = append(patched, data[:start]...)
	patched = append(patched, '[')
	for i, item := range items {
		if i > 0 {
			patched = append(patched, ',')
		}
		patched = append(patched, item.raw...)
	}
	patched = append(patched, ']')
	patched = append(patched, data[end:]...)
	return patched, true
}

func patchCodexCompletedOutputArrayAtResult(completedData []byte, outputResult gjson.Result, outputArray []byte) []byte {
	if patched, ok := patchCodexJSONResultRaw(completedData, outputResult, outputArray); ok {
		return patched
	}
	patched, _ := sjson.SetRawBytes(completedData, "response.output", outputArray)
	return patched
}

func patchCodexJSONResultRaw(data []byte, result gjson.Result, replacement []byte) ([]byte, bool) {
	start, end, ok := codexJSONResultRawRange(data, result)
	if !ok {
		return nil, false
	}
	patched := make([]byte, 0, len(data)-len(result.Raw)+len(replacement))
	patched = append(patched, data[:start]...)
	patched = append(patched, replacement...)
	patched = append(patched, data[end:]...)
	return patched, true
}

func codexJSONResultRawRange(data []byte, result gjson.Result) (int, int, bool) {
	if len(data) == 0 || len(result.Raw) == 0 || result.Index < 0 || result.Index+len(result.Raw) > len(data) {
		return 0, 0, false
	}
	end := result.Index + len(result.Raw)
	if !codexBytesEqualString(data[result.Index:end], result.Raw) {
		return 0, 0, false
	}
	return result.Index, end, true
}

func codexBytesEqualString(data []byte, value string) bool {
	if len(data) != len(value) {
		return false
	}
	for i := range data {
		if data[i] != value[i] {
			return false
		}
	}
	return true
}

func collectCodexOutputItemDone(eventData []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback *[][]byte) {
	itemResult := gjson.GetBytes(eventData, "item")
	if !itemResult.Exists() || itemResult.Type != gjson.JSON {
		return
	}
	itemBytes := []byte(itemResult.Raw)
	outputIndexResult := gjson.GetBytes(eventData, "output_index")
	if outputIndexResult.Exists() && outputItemsByIndex != nil {
		outputItemsByIndex[outputIndexResult.Int()] = itemBytes
		return
	}
	if outputItemsFallback != nil {
		*outputItemsFallback = append(*outputItemsFallback, itemBytes)
	}
}

func patchCodexCompletedOutput(completedData []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback [][]byte) []byte {
	totalItems := len(outputItemsByIndex) + len(outputItemsFallback)
	if totalItems == 0 {
		return completedData
	}
	if len(outputItemsFallback) == 0 && len(outputItemsByIndex) == 1 {
		for _, raw := range outputItemsByIndex {
			return patchCodexCompletedOutputWithSingleItem(completedData, raw)
		}
	}
	if len(outputItemsByIndex) == 0 {
		return patchCodexCompletedOutputWithItems(completedData, outputItemsFallback)
	}

	items := make([][]byte, 0, totalItems)
	indexes := make([]int64, 0, len(outputItemsByIndex))
	for idx := range outputItemsByIndex {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	for _, idx := range indexes {
		items = append(items, outputItemsByIndex[idx])
	}
	items = append(items, outputItemsFallback...)
	return patchCodexCompletedOutputWithItems(completedData, items)
}

func buildCodexCompletedFunctionCallItem(itemID string, callID string, name string, args string) []byte {
	return buildCodexCompletedToolCallItem(itemID, callID, name, "function_call", args)
}

func buildCodexCompletedToolCallItem(itemID string, callID string, name string, itemType string, args string) []byte {
	switch strings.TrimSpace(itemType) {
	case "custom_tool_call":
		return buildCodexCompletedCustomToolCallItem(itemID, callID, name, args)
	case "local_shell_call":
		return buildCodexCompletedLocalShellCallItem(itemID, callID, args, "completed")
	case "tool_search_call":
		return buildCodexCompletedToolSearchCallItem(itemID, callID, "client", "completed", args)
	}
	buf := make([]byte, 0, len(itemID)+len(callID)+len(name)+len(args)+80)
	buf = append(buf, `{"id":`...)
	buf = strconv.AppendQuote(buf, itemID)
	buf = append(buf, `,"type":"function_call","status":"completed","arguments":`...)
	buf = strconv.AppendQuote(buf, args)
	buf = append(buf, `,"call_id":`...)
	buf = strconv.AppendQuote(buf, callID)
	buf = append(buf, `,"name":`...)
	buf = strconv.AppendQuote(buf, name)
	buf = append(buf, '}')
	return buf
}

func buildCodexCompletedToolSearchCallItem(itemID string, callID string, execution string, status string, argumentsRaw string) []byte {
	argumentsRaw = strings.TrimSpace(argumentsRaw)
	if argumentsRaw == "" || !gjson.Valid(argumentsRaw) {
		argumentsRaw = "{}"
	}
	execution = strings.TrimSpace(execution)
	if execution == "" {
		execution = "client"
	}
	status = strings.TrimSpace(status)

	buf := make([]byte, 0, len(itemID)+len(callID)+len(execution)+len(status)+len(argumentsRaw)+100)
	if strings.TrimSpace(itemID) != "" {
		buf = append(buf, `{"id":`...)
		buf = strconv.AppendQuote(buf, itemID)
		buf = append(buf, `,"type":"tool_search_call"`...)
	} else {
		buf = append(buf, `{"type":"tool_search_call"`...)
	}
	if strings.TrimSpace(callID) != "" {
		buf = append(buf, `,"call_id":`...)
		buf = strconv.AppendQuote(buf, callID)
	}
	if status != "" {
		buf = append(buf, `,"status":`...)
		buf = strconv.AppendQuote(buf, status)
	}
	buf = append(buf, `,"execution":`...)
	buf = strconv.AppendQuote(buf, execution)
	buf = append(buf, `,"arguments":`...)
	buf = append(buf, argumentsRaw...)
	buf = append(buf, '}')
	return buf
}

func buildCodexCompletedLocalShellCallItem(itemID string, callID string, actionRaw string, status string) []byte {
	actionRaw = strings.TrimSpace(actionRaw)
	if actionRaw == "" || !gjson.Valid(actionRaw) {
		actionRaw = "{}"
	}
	status = strings.TrimSpace(status)
	if status == "" || status == "in_progress" {
		status = "completed"
	}

	buf := make([]byte, 0, len(itemID)+len(callID)+len(status)+len(actionRaw)+80)
	if strings.TrimSpace(itemID) != "" {
		buf = append(buf, `{"id":`...)
		buf = strconv.AppendQuote(buf, itemID)
		buf = append(buf, `,"type":"local_shell_call"`...)
	} else {
		buf = append(buf, `{"type":"local_shell_call"`...)
	}
	if strings.TrimSpace(callID) != "" {
		buf = append(buf, `,"call_id":`...)
		buf = strconv.AppendQuote(buf, callID)
	}
	buf = append(buf, `,"status":`...)
	buf = strconv.AppendQuote(buf, status)
	buf = append(buf, `,"action":`...)
	buf = append(buf, actionRaw...)
	buf = append(buf, '}')
	return buf
}

func buildCodexCompletedCustomToolCallItem(itemID string, callID string, name string, input string) []byte {
	buf := make([]byte, 0, len(itemID)+len(callID)+len(name)+len(input)+70)
	if strings.TrimSpace(itemID) != "" {
		buf = append(buf, `{"id":`...)
		buf = strconv.AppendQuote(buf, itemID)
		buf = append(buf, `,"type":"custom_tool_call","input":`...)
	} else {
		buf = append(buf, `{"type":"custom_tool_call","input":`...)
	}
	buf = strconv.AppendQuote(buf, input)
	buf = append(buf, `,"call_id":`...)
	buf = strconv.AppendQuote(buf, callID)
	buf = append(buf, `,"name":`...)
	buf = strconv.AppendQuote(buf, name)
	buf = append(buf, '}')
	return buf
}
