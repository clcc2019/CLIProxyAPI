// Package openai provides HTTP handlers for OpenAIResponses API endpoints.
// This package implements the OpenAIResponses-compatible API interface, including model listing
// and chat completion functionality. It supports both streaming and non-streaming responses,
// and manages a pool of clients to interact with backend services.
// The handlers translate OpenAIResponses API requests to the appropriate backend format and
// convert responses back to OpenAIResponses-compatible format.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
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
	if len(f.pending) == 0 || !responsesSSECanEmitWithoutDelimiter(f.pending, f.trustedData) {
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
	if !responsesSSECanEmitWithoutDelimiter(f.pending, f.trustedData) {
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

func responsesSSEFrameWithDataPayload(frame, payload []byte) []byte {
	lines := bytes.Split(bytes.TrimRight(frame, "\r\n"), []byte("\n"))
	out := make([][]byte, 0, len(lines)+1)
	dataWritten := false
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			if !dataWritten {
				out = appendResponsesSSEDataLines(out, payload)
				dataWritten = true
			}
			continue
		}
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	if !dataWritten {
		out = appendResponsesSSEDataLines(out, payload)
	}
	return append(bytes.Join(out, []byte("\n")), '\n', '\n')
}

func appendResponsesSSEDataLines(out [][]byte, payload []byte) [][]byte {
	payloadLines := bytes.Split(payload, []byte("\n"))
	for _, line := range payloadLines {
		out = append(out, append([]byte("data: "), line...))
	}
	return out
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
	lines := bytes.Split(frame, []byte("\n"))
	var payload []byte
	for i := range lines {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload = append(payload, line[len("data: "):]...)
	}
	if len(payload) == 0 {
		return nil, false
	}
	return payload, true
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
	return responsesSSECanEmitWithoutDelimiter(chunk, trustedData)
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

func responsesSSEDataLinesValid(chunk []byte) bool {
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
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if !json.Valid(data) {
			return false
		}
	}
	return true
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

// OpenAIResponsesAPIHandler contains the handlers for OpenAIResponses API endpoints.
// It holds a pool of clients to interact with the backend service.
type OpenAIResponsesAPIHandler struct {
	*handlers.BaseAPIHandler
}

// NewOpenAIResponsesAPIHandler creates a new OpenAIResponses API handlers instance.
// It takes an BaseAPIHandler instance as input and returns an OpenAIResponsesAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handlers instance
//
// Returns:
//   - *OpenAIResponsesAPIHandler: A new OpenAIResponses API handlers instance
func NewOpenAIResponsesAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIResponsesAPIHandler {
	return &OpenAIResponsesAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the identifier for this handler implementation.
func (h *OpenAIResponsesAPIHandler) HandlerType() string {
	return OpenaiResponse
}

// Models returns the OpenAIResponses-compatible model metadata supported by this handler.
func (h *OpenAIResponsesAPIHandler) Models() []map[string]any {
	modelRegistry := registry.GetGlobalRegistry()
	return compactOpenAIModelMaps(modelRegistry.GetAvailableOpenAIModelSummaries())
}

// OpenAIResponsesModels handles the /v1/models endpoint.
// It returns a list of available AI models with their capabilities
// and specifications in OpenAIResponses-compatible format.
func (h *OpenAIResponsesAPIHandler) OpenAIResponsesModels(c *gin.Context) {
	modelRegistry := registry.GetGlobalRegistry()
	models := handlers.FilterOpenAIModelSummariesForClient(c, modelRegistry.GetAvailableOpenAIModelSummaries())

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   models,
	})
}

// Responses handles the /v1/responses endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *OpenAIResponsesAPIHandler) Responses(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	// If data retrieval fails, return a 400 Bad Request error.
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	requestDetails := handlers.ParseRequestBodyDetails(rawJSON)
	if requestDetails.Stream {
		h.handleStreamingResponse(c, requestDetails.Model, rawJSON)
	} else {
		h.handleNonStreamingResponse(c, requestDetails.Model, rawJSON)
	}

}

func (h *OpenAIResponsesAPIHandler) Compact(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	requestDetails := handlers.ParseRequestBodyDetails(rawJSON)
	if requestDetails.Stream {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported for compact responses",
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if requestDetails.HasStream {
		if updated, err := sjson.DeleteBytes(rawJSON, "stream"); err == nil {
			rawJSON = updated
		}
	}

	c.Header("Content-Type", "application/json")
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	executionSessionID := responsesExplicitExecutionSessionID(c.Request, rawJSON)
	if executionSessionID != "" {
		cliCtx = handlers.WithExecutionSessionID(cliCtx, executionSessionID)
	}
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), requestDetails.Model, rawJSON, "responses/compact")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}
	resp = newResponsesNoticeFilter().FilterResponseObject(resp)
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	if executionSessionID != "" && h != nil && h.AuthManager != nil {
		h.AuthManager.ResetExecutionSession(executionSessionID)
	}
	cliCancel()
}

// handleNonStreamingResponse handles non-streaming chat completion responses
// for upstream models. It selects a client from the pool, sends the request, and
// aggregates the response before sending it back to the client in OpenAIResponses format.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - modelName: The model name declared in the request
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleNonStreamingResponse(c *gin.Context, modelName string, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	if executionSessionID := responsesExplicitExecutionSessionID(c.Request, rawJSON); executionSessionID != "" {
		cliCtx = handlers.WithExecutionSessionID(cliCtx, executionSessionID)
	}
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}
	resp = newResponsesNoticeFilter().FilterResponseObject(resp)
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleStreamingResponse handles streaming responses for upstream models.
// It establishes a streaming connection with the backend service and forwards
// the response chunks to the client in real-time using Server-Sent Events.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - modelName: The model name declared in the request
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleStreamingResponse(c *gin.Context, modelName string, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	// New core execution path
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	if executionSessionID := responsesExplicitExecutionSessionID(c.Request, rawJSON); executionSessionID != "" {
		cliCtx = handlers.WithExecutionSessionID(cliCtx, executionSessionID)
	}
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}
	framer := &responsesSSEFramer{noticeFilter: newResponsesNoticeFilter(), trustedData: true}

	first, errMsg, err := handlers.AwaitStreamFirstChunk(c.Request.Context(), dataChan, errChan)
	if err != nil {
		cliCancel(err)
		return
	}
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}
	dataChan = first.Data
	errChan = first.Errs

	// Success! Set headers.
	setSSEHeaders()
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)

	// Write first chunk logic (matching forwardResponsesStream)
	if framer.WriteChunk(c.Writer, first.Chunk) {
		flusher.Flush()
	}

	// Continue
	h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, framer)
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, framer *responsesSSEFramer) {
	if framer == nil {
		framer = &responsesSSEFramer{}
	}
	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		WriteChunk: func(chunk []byte) bool {
			return framer.WriteChunk(c.Writer, chunk)
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			_ = framer.Flush(c.Writer)
			if errMsg == nil {
				return
			}
			status := http.StatusInternalServerError
			if errMsg.StatusCode > 0 {
				status = errMsg.StatusCode
			}
			errText := http.StatusText(status)
			if errMsg.Error != nil && errMsg.Error.Error() != "" {
				errText = errMsg.Error.Error()
			}
			chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, 0)
			handlers.WriteSSEEventDataFrameWithLeadingNewline(c.Writer, "error", chunk)
		},
		WriteDone: func() {
			_ = framer.Flush(c.Writer)
			_, _ = c.Writer.Write([]byte("\n"))
		},
	})
}
