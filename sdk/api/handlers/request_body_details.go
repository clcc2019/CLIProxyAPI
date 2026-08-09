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
	model := gjson.Get(jsonText, "model")
	stream := gjson.Get(jsonText, "stream")
	details := RequestBodyDetails{}
	details.Model = strings.Clone(model.String())
	details.HasStream = stream.Exists()
	details.Stream = stream.Type == gjson.True
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
	model := gjson.Get(jsonText, "model")
	stream := gjson.Get(jsonText, "stream")
	details := OpenAIChatRequestBodyDetails{}
	details.Model = strings.Clone(model.String())
	details.HasStream = stream.Exists()
	details.Stream = stream.Type == gjson.True
	details.HasMessages = gjson.Get(jsonText, "messages").Exists()
	details.HasInput = gjson.Get(jsonText, "input").Exists()
	details.HasInstructions = gjson.Get(jsonText, "instructions").Exists()
	return details
}

// UsesResponsesFormat reports whether the payload looks like an OpenAI Responses request.
func (d OpenAIChatRequestBodyDetails) UsesResponsesFormat() bool {
	return !d.HasMessages && (d.HasInput || d.HasInstructions)
}
