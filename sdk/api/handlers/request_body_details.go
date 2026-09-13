package handlers

import (
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

func immutableRequestBodyString(rawJSON []byte) string {
	if len(rawJSON) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(rawJSON), len(rawJSON))
}

// RequestBodyDetails captures hot-path request fields without full unmarshalling.
type RequestBodyDetails struct {
	Model     string
	Stream    bool
	HasStream bool
}

// ParseRequestBodyDetails extracts commonly used request fields in one pass.
func ParseRequestBodyDetails(rawJSON []byte) RequestBodyDetails {
	jsonText := immutableRequestBodyString(rawJSON)
	details := RequestBodyDetails{}
	modelFound, streamFound := false, false
	forEachTopLevelRequestField(jsonText, func(key string, value gjson.Result) bool {
		switch key {
		case "model":
			if !modelFound {
				details.Model = strings.Clone(value.String())
				modelFound = true
			}
		case "stream":
			if !streamFound {
				details.HasStream = true
				details.Stream = value.Type == gjson.True
				streamFound = true
			}
		}
		return !modelFound || !streamFound
	})
	return details
}

// OpenAIChatRequestBodyDetails captures chat-completions request shape checks in one pass.
type OpenAIChatRequestBodyDetails struct {
	RequestBodyDetails
	HasMessages     bool
	HasInput        bool
	HasInstructions bool
}

// ParseOpenAIChatRequestBodyDetails extracts OpenAI chat request routing fields in one pass.
func ParseOpenAIChatRequestBodyDetails(rawJSON []byte) OpenAIChatRequestBodyDetails {
	jsonText := immutableRequestBodyString(rawJSON)
	details := OpenAIChatRequestBodyDetails{}
	modelFound, streamFound := false, false
	messagesFound, inputFound, instructionsFound := false, false, false
	forEachTopLevelRequestField(jsonText, func(key string, value gjson.Result) bool {
		switch key {
		case "model":
			if !modelFound {
				details.Model = strings.Clone(value.String())
				modelFound = true
			}
		case "stream":
			if !streamFound {
				details.HasStream = true
				details.Stream = value.Type == gjson.True
				streamFound = true
			}
		case "messages":
			if !messagesFound {
				details.HasMessages = true
				messagesFound = true
			}
		case "input":
			if !inputFound {
				details.HasInput = true
				inputFound = true
			}
		case "instructions":
			if !instructionsFound {
				details.HasInstructions = true
				instructionsFound = true
			}
		}
		return !modelFound || !streamFound || !messagesFound || !inputFound || !instructionsFound
	})
	return details
}

// forEachTopLevelRequestField keeps routing extraction zero-copy while avoiding
// a separate JSON scan for every field. The callback can stop once all fields it
// needs have been observed.
func forEachTopLevelRequestField(jsonText string, callback func(string, gjson.Result) bool) {
	if jsonText == "" {
		return
	}
	gjson.Parse(jsonText).ForEach(func(key, value gjson.Result) bool {
		if key.Type != gjson.String {
			return true
		}
		return callback(key.String(), value)
	})
}

// UsesResponsesFormat reports whether the payload looks like an OpenAI Responses request.
func (d OpenAIChatRequestBodyDetails) UsesResponsesFormat() bool {
	return !d.HasMessages && (d.HasInput || d.HasInstructions)
}
