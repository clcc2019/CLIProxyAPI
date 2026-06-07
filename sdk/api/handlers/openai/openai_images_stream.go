package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (h *OpenAIAPIHandler) forwardNativeImagesStream(c *gin.Context, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, writeEvent imageStreamEventWriter, timing *imageStreamTiming, writeKeepAlive func()) {
	requestCtx := c.Request.Context()
	var keepAliveC, dataIntervalC <-chan time.Time
	if timing != nil {
		keepAliveC = timing.keepAliveC
		dataIntervalC = timing.dataIntervalC
	}
	if data == nil && errs == nil {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
		emitImagesStreamError(writeEvent, errMsg)
		cancel(errImageStreamNilChannels)
		return
	}

	for {
		select {
		case <-requestCtx.Done():
			cancel(requestCtx.Err())
			return
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				if data == nil {
					errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
					emitImagesStreamError(writeEvent, errMsg)
					cancel(errImageStreamNilChannels)
					return
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			emitImagesStreamError(writeEvent, errMsg)
			cancel(handlers.ErrorMessageCause(errMsg))
			return
		case chunk, ok := <-data:
			if !ok {
				if errMsg, okPendingErr := handlers.PendingStreamError(errs); okPendingErr {
					emitImagesStreamError(writeEvent, errMsg)
					cancel(handlers.ErrorMessageCause(errMsg))
					return
				}
				cancel(nil)
				return
			}
			if timing != nil {
				timing.MarkData()
			}
			writeNativeImagesStreamChunk(writeEvent, chunk)
		case now := <-keepAliveC:
			maybeWriteImageStreamKeepAlive(timing, now, writeKeepAlive)
		case now := <-dataIntervalC:
			if !timing.IdleTimedOut(now) {
				continue
			}
			errMsg := imageStreamIdleTimeoutError(timing.dataIntervalTimeout)
			emitImagesStreamError(writeEvent, errMsg)
			cancel(handlers.ErrorMessageCause(errMsg))
			return
		}
	}
}

func writeNativeImagesStreamChunk(writeEvent imageStreamEventWriter, chunk []byte) {
	if writeEvent == nil || len(bytes.TrimSpace(chunk)) == 0 {
		return
	}
	eventName := ""
	if json.Valid(chunk) {
		eventName = strings.TrimSpace(gjson.GetBytes(chunk, "type").String())
	}
	writeEvent(eventName, chunk)
}

func collectImagesFromResponsesStream(ctx context.Context, data <-chan []byte, errs <-chan *interfaces.ErrorMessage, responseFormat string) ([]byte, *interfaces.ErrorMessage) {
	if data == nil && errs == nil {
		return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
	}
	acc := &sseFrameAccumulator{}
	state := newImageResponseCollectState()

	processFrame := func(frame []byte) ([]byte, bool, *interfaces.ErrorMessage) {
		var result []byte
		var done bool
		var errMsg *interfaces.ErrorMessage
		translatorcommon.ForEachSSEDataLine(frame, func(payload []byte) bool {
			if bytes.Equal(payload, []byte("[DONE]")) {
				return true
			}
			if !json.Valid(payload) {
				errMsg = &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("invalid SSE data JSON")}
				return false
			}

			switch gjson.GetBytes(payload, "type").String() {
			case "response.output_item.done":
				if err := state.AddOutputItemDone(payload); err != nil {
					errMsg = &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err}
					return false
				}
				return true
			case "response.completed":
			default:
				return true
			}

			results, createdAt, usageRaw, firstMeta, err := extractImagesFromResponsesCompleted(payload)
			if err != nil {
				errMsg = &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err}
				return false
			}
			if len(results) == 0 {
				results = state.PendingResults()
				if len(results) > 0 {
					firstMeta = results[0]
				}
			}
			if len(results) == 0 {
				errMsg = &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("upstream did not return image output")}
				return false
			}
			out, err := buildImagesAPIResponse(results, createdAt, usageRaw, firstMeta, responseFormat)
			if err != nil {
				errMsg = &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: err}
				return false
			}
			result = out
			done = true
			return false
		})
		return result, done, errMsg
	}

	for {
		select {
		case <-ctx.Done():
			return nil, &interfaces.ErrorMessage{StatusCode: http.StatusRequestTimeout, Error: ctx.Err()}
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				if data == nil {
					return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			return nil, errMsg
		case chunk, ok := <-data:
			if !ok {
				if errMsg, okPendingErr := handlers.PendingStreamError(errs); okPendingErr {
					return nil, errMsg
				}
				var result []byte
				var done bool
				var errMsg *interfaces.ErrorMessage
				acc.FlushFrames(func(frame []byte) bool {
					result, done, errMsg = processFrame(frame)
					return errMsg == nil && !done
				})
				if errMsg != nil {
					return nil, errMsg
				}
				if done {
					return result, nil
				}
				return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("stream disconnected before completion")}
			}
			var result []byte
			var done bool
			var errMsg *interfaces.ErrorMessage
			acc.ForEachChunkFrame(chunk, func(frame []byte) bool {
				result, done, errMsg = processFrame(frame)
				return errMsg == nil && !done
			})
			if errMsg != nil {
				return nil, errMsg
			}
			if done {
				return result, nil
			}
		}
	}
}

type imageStreamForwardOptions struct {
	cancel         func(error)
	data           <-chan []byte
	errs           <-chan *interfaces.ErrorMessage
	firstChunk     []byte
	responseFormat string
	streamPrefix   string
	writeEvent     imageStreamEventWriter
	writeKeepAlive func()
	timing         *imageStreamTiming
}

func (h *OpenAIAPIHandler) forwardImagesStream(ctx context.Context, c *gin.Context, opts imageStreamForwardOptions) {
	requestCtx := c.Request.Context()
	if opts.data == nil && opts.errs == nil {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
		emitImagesStreamError(opts.writeEvent, errMsg)
		opts.cancel(errImageStreamNilChannels)
		return
	}
	acc := &sseFrameAccumulator{}
	state := newImageResponseCollectState()

	responseFormat := normalizeImagesResponseFormat(opts.responseFormat)

	processFrame := func(frame []byte) (done bool) {
		return processImagesStreamFrame(frame, responseFormat, opts.streamPrefix, opts.writeEvent, state)
	}
	timing := opts.timing
	var keepAliveC, dataIntervalC <-chan time.Time
	if timing != nil {
		keepAliveC = timing.keepAliveC
		dataIntervalC = timing.dataIntervalC
	}

	firstDone := false
	acc.ForEachChunkFrame(opts.firstChunk, func(frame []byte) bool {
		if processFrame(frame) {
			firstDone = true
			return false
		}
		return true
	})
	if firstDone {
		opts.cancel(nil)
		return
	}

	for {
		select {
		case <-requestCtx.Done():
			opts.cancel(requestCtx.Err())
			return
		case errMsg, ok := <-opts.errs:
			if !ok {
				opts.errs = nil
				if opts.data == nil {
					errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
					emitImagesStreamError(opts.writeEvent, errMsg)
					opts.cancel(errImageStreamNilChannels)
					return
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			emitImagesStreamError(opts.writeEvent, errMsg)
			opts.cancel(handlers.ErrorMessageCause(errMsg))
			return
		case chunk, ok := <-opts.data:
			if !ok {
				if errMsg, okPendingErr := handlers.PendingStreamError(opts.errs); okPendingErr {
					emitImagesStreamError(opts.writeEvent, errMsg)
					opts.cancel(handlers.ErrorMessageCause(errMsg))
					return
				}
				done := false
				acc.FlushFrames(func(frame []byte) bool {
					if processFrame(frame) {
						done = true
						return false
					}
					return true
				})
				if done {
					opts.cancel(nil)
					return
				}
				if pending := state.PendingResults(); len(pending) > 0 {
					writeImagesCompletedEventsFromResults(pending, nil, responseFormat, opts.streamPrefix, opts.writeEvent)
				}
				opts.cancel(nil)
				return
			}
			if timing != nil {
				timing.MarkData()
			}
			done := false
			acc.ForEachChunkFrame(chunk, func(frame []byte) bool {
				if processFrame(frame) {
					done = true
					return false
				}
				return true
			})
			if done {
				opts.cancel(nil)
				return
			}
		case now := <-keepAliveC:
			maybeWriteImageStreamKeepAlive(timing, now, opts.writeKeepAlive)
		case now := <-dataIntervalC:
			if !timing.IdleTimedOut(now) {
				continue
			}
			errMsg := imageStreamIdleTimeoutError(timing.dataIntervalTimeout)
			emitImagesStreamError(opts.writeEvent, errMsg)
			opts.cancel(handlers.ErrorMessageCause(errMsg))
			return
		}
	}
}

type imageStreamEventWriter func(string, []byte)

func processImagesStreamFrame(frame []byte, responseFormat string, streamPrefix string, writeEvent imageStreamEventWriter, state *imageResponseCollectState) (done bool) {
	translatorcommon.ForEachSSEDataLine(frame, func(payload []byte) bool {
		if bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
			return true
		}

		switch gjson.GetBytes(payload, "type").String() {
		case "response.image_generation_call.partial_image":
			writeImagesPartialImageEvent(payload, responseFormat, streamPrefix, writeEvent)
		case "response.output_item.done":
			if err := state.AddOutputItemDone(payload); err != nil {
				emitImagesStreamError(writeEvent, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
				done = true
				return false
			}
		case "response.completed":
			writeImagesCompletedEvents(payload, responseFormat, streamPrefix, writeEvent, state)
			done = true
			return false
		}
		return true
	})
	return done
}

func writeImagesPartialImageEvent(payload []byte, responseFormat string, streamPrefix string, writeEvent imageStreamEventWriter) {
	b64 := strings.TrimSpace(gjson.GetBytes(payload, "partial_image_b64").String())
	if b64 == "" {
		return
	}
	outputFormat := strings.TrimSpace(gjson.GetBytes(payload, "output_format").String())
	index := gjson.GetBytes(payload, "partial_image_index").Int()
	eventName := streamPrefix + ".partial_image"
	data := []byte(`{"type":"","partial_image_index":0}`)
	data, _ = sjson.SetBytes(data, "type", eventName)
	data, _ = sjson.SetBytes(data, "partial_image_index", index)
	if responseFormat == "url" {
		mt := mimeTypeFromOutputFormat(outputFormat)
		data, _ = sjson.SetBytes(data, "url", "data:"+mt+";base64,"+b64)
	} else {
		data, _ = sjson.SetBytes(data, "b64_json", b64)
	}
	writeEvent(eventName, data)
}

func writeImagesCompletedEvents(payload []byte, responseFormat string, streamPrefix string, writeEvent imageStreamEventWriter, state *imageResponseCollectState) {
	results, _, usageRaw, _, err := extractImagesFromResponsesCompleted(payload)
	if err != nil {
		emitImagesStreamError(writeEvent, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
		return
	}
	if len(results) == 0 && state != nil {
		results = state.PendingResults()
	}
	if len(results) == 0 {
		emitImagesStreamError(writeEvent, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("upstream did not return image output")})
		return
	}
	writeImagesCompletedEventsFromResults(results, usageRaw, responseFormat, streamPrefix, writeEvent)
}

func writeImagesCompletedEventsFromResults(results []imageCallResult, usageRaw []byte, responseFormat string, streamPrefix string, writeEvent imageStreamEventWriter) {
	eventName := streamPrefix + ".completed"
	for _, img := range results {
		data := []byte(`{"type":""}`)
		data, _ = sjson.SetBytes(data, "type", eventName)
		if responseFormat == "url" {
			mt := mimeTypeFromOutputFormat(img.OutputFormat)
			data, _ = sjson.SetBytes(data, "url", "data:"+mt+";base64,"+img.Result)
		} else {
			data, _ = sjson.SetBytes(data, "b64_json", img.Result)
		}
		if len(usageRaw) > 0 && json.Valid(usageRaw) {
			data, _ = sjson.SetRawBytes(data, "usage", usageRaw)
		}
		writeEvent(eventName, data)
	}
}

func emitImagesStreamError(writeEvent imageStreamEventWriter, errMsg *interfaces.ErrorMessage) {
	if writeEvent == nil || errMsg == nil {
		return
	}
	status := http.StatusInternalServerError
	if errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	errText := http.StatusText(status)
	if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
		errText = errMsg.Error.Error()
	}
	body := handlers.BuildErrorResponseBody(status, errText)
	writeEvent("error", body)
}

func (h *OpenAIAPIHandler) writeImageStreamNilChannelsError(c *gin.Context, sseStarted bool, writeEvent imageStreamEventWriter) {
	errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
	if sseStarted {
		emitImagesStreamError(writeEvent, errMsg)
		return
	}
	h.WriteErrorResponse(c, errMsg)
}
