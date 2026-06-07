package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	videosPath              = "/v1/videos"
	xaiVideosGenerationsAPI = "/v1/videos/generations"
	xaiVideosEditsAPI       = "/v1/videos/edits"
	xaiVideosExtensionsAPI  = "/v1/videos/extensions"
	defaultXAIVideosModel   = "grok-imagine-video"
	xaiVideos15PreviewModel = "grok-imagine-video-1.5-preview"
	xaiVideosHandlerType    = "openai-video"
	defaultVideosSeconds    = "4"
	defaultVideosSize       = "720x1280"
	defaultVideosResolution = "720p"
	maxXAIVideoReferences   = 7
)

func rejectUnsupportedVideosModel(c *gin.Context, model string) bool {
	if isSupportedVideosModel(model) {
		return false
	}

	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Model %s is not supported on %s. Use %s.", model, videosPath, defaultXAIVideosModel),
			Type:    "invalid_request_error",
		},
	})
	return true
}

func rejectUnsupportedNativeVideosModel(c *gin.Context, model string) bool {
	if isSupportedVideosModel(model) {
		return false
	}

	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Model %s is not supported on %s, %s, or %s. Use %s.", model, xaiVideosGenerationsAPI, xaiVideosEditsAPI, xaiVideosExtensionsAPI, defaultXAIVideosModel),
			Type:    "invalid_request_error",
		},
	})
	return true
}

func readVideosCreateRequest(c *gin.Context) ([]byte, error) {
	contentType := strings.TrimSpace(c.ContentType())
	if isVideosCreateFormContentType(contentType) {
		return videosCreateRequestFromForm(c)
	}

	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		return nil, err
	}
	if !json.Valid(rawJSON) {
		return nil, fmt.Errorf("body must be valid JSON")
	}
	return rawJSON, nil
}

func isVideosCreateFormContentType(contentType string) bool {
	return strings.EqualFold(contentType, "multipart/form-data") ||
		strings.EqualFold(contentType, "application/x-www-form-urlencoded")
}

func readXAIVideosNativeRequest(c *gin.Context) ([]byte, error) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		return nil, err
	}
	if !json.Valid(rawJSON) {
		return nil, fmt.Errorf("body must be valid JSON")
	}
	return rawJSON, nil
}

func videosCreateRequestFromForm(c *gin.Context) ([]byte, error) {
	rawJSON := []byte(`{}`)
	for _, field := range []string{"model", "prompt", "seconds", "size", "aspect_ratio", "resolution"} {
		if value := strings.TrimSpace(c.PostForm(field)); value != "" {
			rawJSON, _ = sjson.SetBytes(rawJSON, field, value)
		}
	}
	if value := strings.TrimSpace(firstPostForm(c, "input_reference[image_url]", "input_reference.image_url", "image_url")); value != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "input_reference.image_url", value)
	}
	if value := strings.TrimSpace(firstPostForm(c, "input_reference[file_id]", "input_reference.file_id", "file_id")); value != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "input_reference.file_id", value)
	}
	if refs := strings.TrimSpace(c.PostForm("reference_image_urls")); refs != "" {
		for _, ref := range strings.Split(refs, ",") {
			if ref = strings.TrimSpace(ref); ref != "" {
				rawJSON, _ = sjson.SetBytes(rawJSON, "reference_image_urls.-1", ref)
			}
		}
	}
	return rawJSON, nil
}

func firstPostForm(c *gin.Context, keys ...string) string {
	for _, key := range keys {
		if value := c.PostForm(key); strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (h *OpenAIAPIHandler) VideosCreate(c *gin.Context) {
	rawJSON, err := readVideosCreateRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	videoModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if videoModel == "" {
		videoModel = defaultXAIVideosModel
	}
	if rejectUnsupportedVideosModel(c, videoModel) {
		return
	}

	xaiReq, meta, err := buildXAIVideosCreateRequest(rawJSON, videoModel)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	h.collectXAIVideosCreate(c, xaiReq, meta)
}

func (h *OpenAIAPIHandler) XAIVideosGenerations(c *gin.Context) {
	h.handleXAIVideosNativePost(c)
}

func (h *OpenAIAPIHandler) XAIVideosEdits(c *gin.Context) {
	h.handleXAIVideosNativePost(c)
}

func (h *OpenAIAPIHandler) XAIVideosExtensions(c *gin.Context) {
	h.handleXAIVideosNativePost(c)
}

func (h *OpenAIAPIHandler) handleXAIVideosNativePost(c *gin.Context) {
	rawJSON, err := readXAIVideosNativeRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	videoModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if videoModel == "" {
		videoModel = defaultXAIVideosModel
	}
	if rejectUnsupportedNativeVideosModel(c, videoModel) {
		return
	}

	h.collectXAIVideosNative(c, rawJSON, videoModel)
}

func (h *OpenAIAPIHandler) XAIVideosRetrieve(c *gin.Context) {
	requestID := strings.TrimSpace(c.Param("request_id"))
	if requestID == "" {
		requestID = strings.TrimSpace(c.Param("video_id"))
	}
	if requestID == "" {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: request_id is required",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	payload := []byte(`{}`)
	payload, _ = sjson.SetBytes(payload, "request_id", requestID)
	h.collectXAIVideosNative(c, payload, defaultXAIVideosModel)
}

func (h *OpenAIAPIHandler) VideosRetrieve(c *gin.Context) {
	videoID := strings.TrimSpace(c.Param("video_id"))
	if videoID == "" {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: video_id is required",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	payload := []byte(`{}`)
	payload, _ = sjson.SetBytes(payload, "request_id", videoID)

	c.Header("Content-Type", "application/json")
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, xaiVideosHandlerType, defaultXAIVideosModel, payload, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}

	out, err := buildVideosRetrieveAPIResponseFromXAI(videoID, resp, defaultXAIVideosModel)
	if err != nil {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err}
		h.WriteErrorResponse(c, errMsg)
		cliCancel(err)
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel(nil)
}

func (h *OpenAIAPIHandler) collectXAIVideosNative(c *gin.Context, rawJSON []byte, model string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, xaiVideosHandlerType, model, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel(nil)
}

func (h *OpenAIAPIHandler) collectXAIVideosCreate(c *gin.Context, xaiReq []byte, meta xaiVideoCreateMetadata) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, xaiVideosHandlerType, meta.Model, xaiReq, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}

	out, err := buildVideosCreateAPIResponseFromXAI(resp, meta)
	if err != nil {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err}
		h.WriteErrorResponse(c, errMsg)
		cliCancel(err)
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel(nil)
}
