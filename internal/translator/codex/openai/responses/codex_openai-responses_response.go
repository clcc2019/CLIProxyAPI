package responses

import (
	"bytes"
	"context"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	responsesSSEDataTag         = []byte("data:")
	responsesSSECanonicalPrefix = []byte("data: ")
)

// ConvertCodexResponseToOpenAIResponses converts OpenAI Chat Completions streaming chunks
// to OpenAI Responses SSE events (response.*).

func ConvertCodexResponseToOpenAIResponses(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) [][]byte {
	canonical := bytes.HasPrefix(rawJSON, responsesSSECanonicalPrefix) && len(bytes.TrimSpace(rawJSON)) == len(rawJSON)
	if canonical {
		payload := rawJSON[len(responsesSSECanonicalPrefix):]
		updated := setResponsesRequestFields(payload, modelName, originalRequestRawJSON, requestRawJSON)
		if len(updated) == len(payload) && bytes.Equal(updated, payload) {
			return [][]byte{rawJSON}
		}
		out := make([]byte, 0, len(updated)+len(responsesSSECanonicalPrefix))
		out = append(out, responsesSSECanonicalPrefix...)
		out = append(out, updated...)
		return [][]byte{out}
	}
	if bytes.HasPrefix(rawJSON, responsesSSEDataTag) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
		rawJSON = setResponsesRequestFields(rawJSON, modelName, originalRequestRawJSON, requestRawJSON)
		out := make([]byte, 0, len(rawJSON)+len(responsesSSECanonicalPrefix))
		out = append(out, responsesSSECanonicalPrefix...)
		out = append(out, rawJSON...)
		return [][]byte{out}
	}
	return [][]byte{setResponsesRequestFields(rawJSON, modelName, originalRequestRawJSON, requestRawJSON)}
}

func setResponsesRequestFields(rawJSON []byte, modelName string, originalRequestRawJSON, requestRawJSON []byte) []byte {
	eventType := gjson.GetBytes(rawJSON, "type").String()
	if eventType != "response.created" && eventType != "response.in_progress" && eventType != "response.completed" {
		return rawJSON
	}

	requestModelName := translatorcommon.RequestModelName(originalRequestRawJSON, requestRawJSON)
	if requestModelName == "" {
		requestModelName = modelName
	}
	updated := rawJSON
	var errSet error
	if requestModelName != "" {
		updated, errSet = sjson.SetBytes(updated, "response.model", requestModelName)
		if errSet != nil {
			return rawJSON
		}
	}
	if eventType == "response.in_progress" {
		updated, errSet = sjson.SetRawBytes(updated, "response.output", []byte("[]"))
		if errSet != nil {
			return rawJSON
		}
	}
	return updated
}

// ConvertCodexResponseToOpenAIResponsesNonStream builds a single Responses JSON
// from a non-streaming OpenAI Chat Completions response.
func ConvertCodexResponseToOpenAIResponsesNonStream(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) []byte {
	rootResult := gjson.ParseBytes(rawJSON)
	// Verify this is a response.completed event
	if rootResult.Get("type").String() != "response.completed" {
		return []byte{}
	}
	responseResult := rootResult.Get("response")
	return []byte(responseResult.Raw)
}
