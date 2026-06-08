package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	defaultImagesMainModel       = "gpt-5.4-mini"
	defaultImagesToolModel       = "gpt-image-2"
	defaultImagesVariationPrompt = "Create a variation of the provided image."
	defaultXAIImagesModel        = "grok-imagine-image"
	xaiImagesQualityModel        = "grok-imagine-image-quality"
	xaiImagesHandlerType         = "openai-image"
	xaiImagesDefaultAspectRatio  = "1:1"
	xaiImagesDefaultResolution   = "1k"
	imagesGenerationsPath        = "/v1/images/generations"
	imagesEditsPath              = "/v1/images/edits"
	imageStreamKeepAlivePayload  = ":\n\n"
	maxImageUploadBytes          = 32 << 20
	maxImageMultipartBytes       = 128 << 20
)

var (
	errImageStreamNilChannels = errors.New("image stream received nil data and error channels")
)

func rejectUnsupportedImagesModel(c *gin.Context, model string) bool {
	if isSupportedImagesModel(model) {
		return false
	}

	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Model %s is not supported on %s or %s. Use %s, %s, %s, or a configured openai-compatibility image model.", model, imagesGenerationsPath, imagesEditsPath, defaultImagesToolModel, defaultXAIImagesModel, xaiImagesQualityModel),
			Type:    "invalid_request_error",
		},
	})
	return true
}

func (h *OpenAIAPIHandler) rejectImagesEndpointIfDisabled(c *gin.Context) bool {
	if h == nil || h.Cfg == nil || h.Cfg.DisableImageGeneration != config.DisableImageGenerationAll {
		return false
	}
	c.JSON(http.StatusNotFound, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: "Image generation endpoints are disabled",
			Type:    "invalid_request_error",
			Code:    "not_found",
		},
	})
	return true
}

func (h *OpenAIAPIHandler) ImagesGenerations(c *gin.Context) {
	if h.rejectImagesEndpointIfDisabled(c) {
		return
	}

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
	if !json.Valid(rawJSON) {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: body must be valid JSON",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	if prompt == "" {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: prompt is required",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	responseFormat := strings.TrimSpace(gjson.GetBytes(rawJSON, "response_format").String())
	responseFormatProvided := responseFormat != ""
	if !responseFormatProvided {
		responseFormat = "b64_json"
	}
	stream := gjson.GetBytes(rawJSON, "stream").Bool()

	nativeJSON := rawJSON
	if !responseFormatProvided {
		nativeJSON, _ = sjson.SetBytes(nativeJSON, "response_format", responseFormat)
	}
	if isDefaultImagesToolModel(imageModel) {
		imageReq := buildOpenAICompatImagesJSONRequest(nativeJSON, imageModel, stream)
		h.handleRoutedImages(c, imageReq, imageModel, stream)
		return
	}
	if isXAIImagesModel(imageModel) {
		xaiReq := buildXAIImagesGenerationsRequest(rawJSON, imageModel, responseFormat)
		h.handleXAIImages(c, xaiReq, responseFormat, "image_generation", stream)
		return
	}
	if openAICompatibleImageModel(imageModel) {
		compatReq := buildOpenAICompatImagesJSONRequest(nativeJSON, imageModel, stream)
		h.handleOpenAICompatImages(c, compatReq, imageModel, responseFormat, "image_generation", stream)
		return
	}

	tool := newImageTool("generate", imageModel)
	tool = setJSONImageToolStringFields(tool, rawJSON, imageGenerateStringToolFields...)
	tool = setJSONImageToolNumberFields(tool, rawJSON, imageNumberToolFields...)

	h.dispatchImagesResponses(c, prompt, nil, tool, responseFormat, stream, "image_generation")
}

func (h *OpenAIAPIHandler) ImagesEdits(c *gin.Context) {
	if h.rejectImagesEndpointIfDisabled(c) {
		return
	}

	contentType := strings.TrimSpace(c.GetHeader("Content-Type"))
	if hasContentTypePrefix(contentType, "application/json") {
		h.imagesEditsFromJSON(c)
		return
	}
	if hasContentTypePrefix(contentType, "multipart/form-data") || contentType == "" {
		h.imagesEditsFromMultipart(c)
		return
	}

	normalizedContentType := strings.ToLower(contentType)
	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Invalid request: unsupported Content-Type %q", normalizedContentType),
			Type:    "invalid_request_error",
		},
	})
}

func (h *OpenAIAPIHandler) ImagesVariations(c *gin.Context) {
	if h.rejectImagesEndpointIfDisabled(c) {
		return
	}

	contentType := strings.TrimSpace(c.GetHeader("Content-Type"))
	if hasContentTypePrefix(contentType, "multipart/form-data") || contentType == "" {
		h.imagesVariationsFromMultipart(c)
		return
	}

	normalizedContentType := strings.ToLower(contentType)
	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Invalid request: unsupported Content-Type %q", normalizedContentType),
			Type:    "invalid_request_error",
		},
	})
}

func hasContentTypePrefix(contentType, prefix string) bool {
	if len(contentType) < len(prefix) {
		return false
	}
	return strings.EqualFold(contentType[:len(prefix)], prefix)
}

func (h *OpenAIAPIHandler) imagesEditsFromMultipart(c *gin.Context) {
	rawBody, err := util.ReadResponseBodyLimited(c.Request.Body, maxImageMultipartBytes)
	if err != nil {
		if errors.Is(err, util.ErrResponseBodyTooLarge) {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: multipart body exceeds maximum allowed size of %d bytes", maxImageMultipartBytes),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))

	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	prompt := strings.TrimSpace(c.PostForm("prompt"))
	if prompt == "" {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: prompt is required",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	imageFiles := multipartImageFiles(form)
	if len(imageFiles) == 0 {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: image is required",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	images := make([]string, 0, len(imageFiles))
	for _, fh := range imageFiles {
		dataURL, err := multipartFileToDataURL(fh)
		if err != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		images = append(images, dataURL)
	}

	imageModel := strings.TrimSpace(c.PostForm("model"))
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	responseFormat := strings.TrimSpace(c.PostForm("response_format"))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := parseBoolField(c.PostForm("stream"), false)

	if isDefaultImagesToolModel(imageModel) {
		imageReq, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, imageModel, stream)
		if errBuild != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", errBuild),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		c.Request.Header.Set("Content-Type", contentType)
		h.handleRoutedImages(c, imageReq, imageModel, stream)
		return
	}
	if isXAIImagesModel(imageModel) {
		aspectRatio := xaiImagesAspectRatio(c.PostForm("aspect_ratio"), "")
		aspectRatio = xaiImagesAspectRatioFromSize(c.PostForm("size"), aspectRatio)
		resolution := xaiImagesResolution(c.PostForm("resolution"), c.PostForm("size"), "")
		n := parseIntField(c.PostForm("n"), 0)
		xaiReq := buildXAIImagesEditRequest(imageModel, prompt, images, responseFormat, aspectRatio, resolution, n)
		h.handleXAIImages(c, xaiReq, responseFormat, "image_edit", stream)
		return
	}
	if isOpenAICompatImagesModel(imageModel) {
		compatReq, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, imageModel, stream)
		if errBuild != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", errBuild),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		c.Request.Header.Set("Content-Type", contentType)
		h.handleOpenAICompatImages(c, compatReq, imageModel, responseFormat, "image_edit", stream)
		return
	}

	var maskDataURL *string
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, err := multipartFileToDataURL(maskFiles[0])
		if err != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		maskDataURL = &dataURL
	}

	responseFormat = strings.TrimSpace(c.PostForm("response_format"))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream = parseBoolField(c.PostForm("stream"), false)

	if openAICompatibleImageModel(imageModel) {
		h.dispatchImagesNative(c, rawBody, imageModel, stream, "images/edits")
		return
	}

	tool := newImageTool("edit", imageModel)
	tool = setFormImageToolStringFields(tool, c, imageEditStringToolFields...)
	tool = setFormImageToolNumberFields(tool, c, imageNumberToolFields...)

	if maskDataURL != nil && strings.TrimSpace(*maskDataURL) != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", strings.TrimSpace(*maskDataURL))
	}

	h.dispatchImagesResponses(c, prompt, images, tool, responseFormat, stream, "image_edit")
}

func (h *OpenAIAPIHandler) imagesVariationsFromMultipart(c *gin.Context) {
	rawBody, err := util.ReadResponseBodyLimited(c.Request.Body, maxImageMultipartBytes)
	if err != nil {
		if errors.Is(err, util.ErrResponseBodyTooLarge) {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: multipart body exceeds maximum allowed size of %d bytes", maxImageMultipartBytes),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))

	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	imageFiles := multipartImageFiles(form)
	if len(imageFiles) == 0 {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: image is required",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	imageModel := strings.TrimSpace(c.PostForm("model"))
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	responseFormat := strings.TrimSpace(c.PostForm("response_format"))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := parseBoolField(c.PostForm("stream"), false)

	if openAICompatibleImageModel(imageModel) {
		h.dispatchImagesNative(c, rawBody, imageModel, stream, "images/variations")
		return
	}

	images := make([]string, 0, len(imageFiles))
	for _, fh := range imageFiles {
		dataURL, err := multipartFileToDataURL(fh)
		if err != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		images = append(images, dataURL)
	}

	prompt := strings.TrimSpace(c.PostForm("prompt"))
	if prompt == "" {
		prompt = defaultImagesVariationPrompt
	}

	tool := newImageTool("edit", imageModel)
	tool = setFormImageToolStringFields(tool, c, imageEditStringToolFields...)
	tool = setFormImageToolNumberFields(tool, c, imageNumberToolFields...)

	h.dispatchImagesResponses(c, prompt, images, tool, responseFormat, stream, "image_variation")
}

func (h *OpenAIAPIHandler) imagesEditsFromJSON(c *gin.Context) {
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
	if !json.Valid(rawJSON) {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: body must be valid JSON",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	if prompt == "" {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: prompt is required",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	responseFormat := strings.TrimSpace(gjson.GetBytes(rawJSON, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := gjson.GetBytes(rawJSON, "stream").Bool()

	if isDefaultImagesToolModel(imageModel) {
		imageReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleRoutedImages(c, imageReq, imageModel, stream)
		return
	}
	if isXAIImagesModel(imageModel) {
		images := collectXAIImagesFromJSON(rawJSON)
		if len(images) == 0 {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: image is required",
					Type:    "invalid_request_error",
				},
			})
			return
		}
		aspectRatio, resolution, n := xaiImagesEditOptionsFromJSON(rawJSON)
		xaiReq := buildXAIImagesEditRequest(imageModel, prompt, images, responseFormat, aspectRatio, resolution, n)
		h.handleXAIImages(c, xaiReq, responseFormat, "image_edit", stream)
		return
	}
	if isOpenAICompatImagesModel(imageModel) {
		compatReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleOpenAICompatImages(c, compatReq, imageModel, responseFormat, "image_edit", stream)
		return
	}

	var images []string
	imagesResult := gjson.GetBytes(rawJSON, "images")
	if imagesResult.IsArray() {
		imagesResult.ForEach(func(_, img gjson.Result) bool {
			url := strings.TrimSpace(img.Get("image_url").String())
			if url == "" {
				return true
			}
			images = append(images, url)
			return true
		})
	}
	if len(images) == 0 {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: images[].image_url is required (file_id is not supported)",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	var maskDataURL *string
	if mask := gjson.GetBytes(rawJSON, "mask.image_url"); mask.Exists() {
		url := strings.TrimSpace(mask.String())
		if url != "" {
			maskDataURL = &url
		}
	} else if mask := gjson.GetBytes(rawJSON, "mask.file_id"); mask.Exists() {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: mask.file_id is not supported (use mask.image_url instead)",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	responseFormat = strings.TrimSpace(gjson.GetBytes(rawJSON, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream = gjson.GetBytes(rawJSON, "stream").Bool()

	tool := newImageTool("edit", imageModel)
	tool = setJSONImageToolStringFields(tool, rawJSON, imageEditStringToolFields...)
	tool = setJSONImageToolNumberFields(tool, rawJSON, imageNumberToolFields...)

	if maskDataURL != nil && strings.TrimSpace(*maskDataURL) != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", strings.TrimSpace(*maskDataURL))
	}

	h.dispatchImagesResponses(c, prompt, images, tool, responseFormat, stream, "image_edit")
}

func (h *OpenAIAPIHandler) dispatchImagesResponses(c *gin.Context, prompt string, images []string, tool []byte, responseFormat string, stream bool, streamPrefix string) {
	responsesReq := buildImagesResponsesRequest(prompt, images, tool)
	if stream {
		h.streamImagesFromResponses(c, responsesReq, responseFormat, streamPrefix)
		return
	}
	h.collectImagesFromResponses(c, responsesReq, responseFormat)
}

func (h *OpenAIAPIHandler) collectImagesFromResponses(c *gin.Context, responsesReq []byte, responseFormat string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, "openai-response", defaultImagesMainModel, responsesReq, "")

	out, errMsg := collectImagesFromResponsesStream(cliCtx, dataChan, errChan, responseFormat)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel()
}

func (h *OpenAIAPIHandler) handleXAIImages(c *gin.Context, xaiReq []byte, responseFormat string, streamPrefix string, stream bool) {
	if stream {
		h.streamXAIImages(c, xaiReq, responseFormat, streamPrefix)
		return
	}
	h.collectXAIImages(c, xaiReq, responseFormat)
}

func (h *OpenAIAPIHandler) handleOpenAICompatImages(c *gin.Context, compatReq []byte, imageModel string, _ string, streamPrefix string, stream bool) {
	alt := "images/generations"
	if streamPrefix == "image_edit" {
		alt = "images/edits"
	}
	h.dispatchImagesNative(c, compatReq, imageModel, stream, alt)
}

func (h *OpenAIAPIHandler) handleRoutedImages(c *gin.Context, imageReq []byte, imageModel string, stream bool) {
	h.dispatchImagesNative(c, imageReq, imageModel, stream, imagesEndpointAlt(c))
}

func imagesEndpointAlt(c *gin.Context) string {
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = strings.TrimSpace(c.Request.URL.Path)
	}
	switch {
	case asciifold.Contains(path, "/images/edits"):
		return "images/edits"
	case asciifold.Contains(path, "/images/variations"):
		return "images/variations"
	default:
		return "images/generations"
	}
}

func (h *OpenAIAPIHandler) collectXAIImages(c *gin.Context, xaiReq []byte, responseFormat string) {
	model := strings.TrimSpace(gjson.GetBytes(xaiReq, "model").String())
	h.collectImagesWithModel(c, xaiReq, model, responseFormat)
}

func (h *OpenAIAPIHandler) collectImagesWithModel(c *gin.Context, imageReq []byte, model string, responseFormat string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	model = strings.TrimSpace(model)
	resp, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(cliCtx, xaiImagesHandlerType, model, imageReq, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}

	out, err := buildImagesAPIResponseFromXAI(resp, responseFormat)
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

func (h *OpenAIAPIHandler) streamXAIImages(c *gin.Context, xaiReq []byte, responseFormat string, streamPrefix string) {
	model := strings.TrimSpace(gjson.GetBytes(xaiReq, "model").String())
	h.streamImagesWithModel(c, xaiReq, model, responseFormat, streamPrefix)
}

func (h *OpenAIAPIHandler) streamImagesWithModel(c *gin.Context, imageReq []byte, model string, responseFormat string, streamPrefix string) {
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

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	model = strings.TrimSpace(model)
	type imageStreamResult struct {
		resp            []byte
		upstreamHeaders http.Header
		errMsg          *interfaces.ErrorMessage
	}
	resultChan := make(chan imageStreamResult, 1)
	go func() {
		resp, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(cliCtx, xaiImagesHandlerType, model, imageReq, "")
		resultChan <- imageStreamResult{resp: resp, upstreamHeaders: upstreamHeaders, errMsg: errMsg}
	}()

	timing := h.newImageStreamTiming()
	defer timing.Stop()
	sseStarted := false
	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}
	ensureSSEStarted := func(upstreamHeaders http.Header) {
		if sseStarted {
			return
		}
		setSSEHeaders()
		handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
		sseStarted = true
	}
	writeEvent := func(eventName string, dataJSON []byte) {
		ensureSSEStarted(nil)
		if strings.TrimSpace(eventName) != "" {
			_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(dataJSON))
		flusher.Flush()
		timing.MarkWrite()
	}
	writeKeepAlive := func() {
		ensureSSEStarted(nil)
		_, _ = c.Writer.Write([]byte(imageStreamKeepAlivePayload))
		flusher.Flush()
		timing.MarkWrite()
	}
	writeError := func(errMsg *interfaces.ErrorMessage) {
		if sseStarted {
			emitImagesStreamError(writeEvent, errMsg)
		} else {
			h.WriteErrorResponse(c, errMsg)
		}
		cliCancel(handlers.ErrorMessageCause(errMsg))
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case now := <-timing.keepAliveC:
			maybeWriteImageStreamKeepAlive(timing, now, writeKeepAlive)
		case result := <-resultChan:
			if result.errMsg != nil {
				writeError(result.errMsg)
				return
			}

			results, _, usageRaw, err := extractXAIImagesResponse(result.resp)
			if err != nil {
				writeError(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
				return
			}

			ensureSSEStarted(result.upstreamHeaders)
			eventName := streamPrefix + ".completed"
			responseFormat = normalizeImagesResponseFormat(responseFormat)
			for _, img := range results {
				data := []byte(`{"type":""}`)
				data, _ = sjson.SetBytes(data, "type", eventName)
				if responseFormat == "url" {
					if img.URL != "" {
						data, _ = sjson.SetBytes(data, "url", img.URL)
					} else {
						data, _ = sjson.SetBytes(data, "url", "data:"+mimeTypeFromOutputFormat(img.MimeType)+";base64,"+img.B64JSON)
					}
				} else if img.B64JSON != "" {
					data, _ = sjson.SetBytes(data, "b64_json", img.B64JSON)
				} else {
					data, _ = sjson.SetBytes(data, "url", img.URL)
				}
				if len(usageRaw) > 0 && json.Valid(usageRaw) {
					data, _ = sjson.SetRawBytes(data, "usage", usageRaw)
				}
				if strings.TrimSpace(eventName) != "" {
					_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
				}
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(data))
				flusher.Flush()
				timing.MarkWrite()
			}
			cliCancel(nil)
			return
		}
	}
}

func (h *OpenAIAPIHandler) dispatchImagesNative(c *gin.Context, rawPayload []byte, modelName string, stream bool, alt string) {
	if stream {
		h.streamImagesFromNative(c, rawPayload, modelName, alt)
		return
	}
	h.collectImagesFromNative(c, rawPayload, modelName, alt)
}

func (h *OpenAIAPIHandler) collectImagesFromNative(c *gin.Context, rawPayload []byte, modelName string, alt string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	out, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(cliCtx, "openai", modelName, rawPayload, alt)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(handlers.ErrorMessageCause(errMsg))
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel()
}

func (h *OpenAIAPIHandler) streamImagesFromNative(c *gin.Context, rawPayload []byte, modelName string, alt string) {
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

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	timing := h.newImageStreamTiming()
	defer timing.Stop()
	var dataChan <-chan []byte
	var upstreamHeaders http.Header
	var errChan <-chan *interfaces.ErrorMessage

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}
	sseStarted := false
	ensureSSEStarted := func() {
		if sseStarted {
			return
		}
		setSSEHeaders()
		handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
		sseStarted = true
	}

	writeEvent := func(eventName string, dataJSON []byte) {
		ensureSSEStarted()
		if strings.TrimSpace(eventName) != "" {
			_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(dataJSON))
		flusher.Flush()
		timing.MarkWrite()
	}
	writeKeepAlive := func() {
		ensureSSEStarted()
		_, _ = c.Writer.Write([]byte(imageStreamKeepAlivePayload))
		flusher.Flush()
		timing.MarkWrite()
	}
	execution, canceled := waitImagesStreamExecution(c, timing, writeKeepAlive, func() imagesStreamExecutionResult {
		data, headers, errs := h.ExecuteImageStreamWithAuthManager(cliCtx, "openai", modelName, rawPayload, alt)
		return imagesStreamExecutionResult{Data: data, UpstreamHeaders: headers, Errs: errs}
	})
	if canceled {
		cliCancel(c.Request.Context().Err())
		return
	}
	dataChan = execution.Data
	upstreamHeaders = execution.UpstreamHeaders
	errChan = execution.Errs
	if dataChan == nil && errChan == nil {
		h.writeImageStreamNilChannelsError(c, sseStarted, writeEvent)
		cliCancel(errImageStreamNilChannels)
		return
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				if dataChan == nil {
					h.writeImageStreamNilChannelsError(c, sseStarted, writeEvent)
					cliCancel(errImageStreamNilChannels)
					return
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			if sseStarted {
				emitImagesStreamError(writeEvent, errMsg)
			} else {
				h.WriteErrorResponse(c, errMsg)
			}
			cliCancel(handlers.ErrorMessageCause(errMsg))
			return
		case chunk, ok := <-dataChan:
			if !ok {
				if errMsg, okPendingErr := handlers.PendingStreamError(errChan); okPendingErr {
					if sseStarted {
						emitImagesStreamError(writeEvent, errMsg)
					} else {
						h.WriteErrorResponse(c, errMsg)
					}
					cliCancel(handlers.ErrorMessageCause(errMsg))
					return
				}
				ensureSSEStarted()
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
				timing.MarkWrite()
				cliCancel(nil)
				return
			}

			timing.MarkData()
			writeNativeImagesStreamChunk(writeEvent, chunk)
			h.forwardNativeImagesStream(c, func(err error) { cliCancel(err) }, dataChan, errChan, writeEvent, timing, writeKeepAlive)
			return
		case now := <-timing.keepAliveC:
			maybeWriteImageStreamKeepAlive(timing, now, writeKeepAlive)
		case now := <-timing.dataIntervalC:
			if !timing.IdleTimedOut(now) {
				continue
			}
			errMsg := imageStreamIdleTimeoutError(timing.dataIntervalTimeout)
			emitImagesStreamError(writeEvent, errMsg)
			cliCancel(handlers.ErrorMessageCause(errMsg))
			return
		}
	}
}

func (h *OpenAIAPIHandler) streamImagesFromResponses(c *gin.Context, responsesReq []byte, responseFormat string, streamPrefix string) {
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

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	mainModel := strings.TrimSpace(gjson.GetBytes(responsesReq, "model").String())
	if mainModel == "" {
		mainModel = defaultImagesMainModel
	}
	timing := h.newImageStreamTiming()
	defer timing.Stop()
	var dataChan <-chan []byte
	var upstreamHeaders http.Header
	var errChan <-chan *interfaces.ErrorMessage

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}
	sseStarted := false
	ensureSSEStarted := func() {
		if sseStarted {
			return
		}
		setSSEHeaders()
		handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
		sseStarted = true
	}

	writeEvent := func(eventName string, dataJSON []byte) {
		ensureSSEStarted()
		if strings.TrimSpace(eventName) != "" {
			_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(dataJSON))
		flusher.Flush()
		timing.MarkWrite()
	}
	writeKeepAlive := func() {
		ensureSSEStarted()
		_, _ = c.Writer.Write([]byte(imageStreamKeepAlivePayload))
		flusher.Flush()
		timing.MarkWrite()
	}
	execution, canceled := waitImagesStreamExecution(c, timing, writeKeepAlive, func() imagesStreamExecutionResult {
		data, headers, errs := h.ExecuteStreamWithAuthManager(cliCtx, "openai-response", mainModel, responsesReq, "")
		return imagesStreamExecutionResult{Data: data, UpstreamHeaders: headers, Errs: errs}
	})
	if canceled {
		cliCancel(c.Request.Context().Err())
		return
	}
	dataChan = execution.Data
	upstreamHeaders = execution.UpstreamHeaders
	errChan = execution.Errs
	if dataChan == nil && errChan == nil {
		h.writeImageStreamNilChannelsError(c, sseStarted, writeEvent)
		cliCancel(errImageStreamNilChannels)
		return
	}

	// Peek for first chunk/error so we can still return a JSON error body.
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				if dataChan == nil {
					h.writeImageStreamNilChannelsError(c, sseStarted, writeEvent)
					cliCancel(errImageStreamNilChannels)
					return
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			if sseStarted {
				emitImagesStreamError(writeEvent, errMsg)
			} else {
				h.WriteErrorResponse(c, errMsg)
			}
			cliCancel(handlers.ErrorMessageCause(errMsg))
			return
		case chunk, ok := <-dataChan:
			if !ok {
				if errMsg, okPendingErr := handlers.PendingStreamError(errChan); okPendingErr {
					if sseStarted {
						emitImagesStreamError(writeEvent, errMsg)
					} else {
						h.WriteErrorResponse(c, errMsg)
					}
					cliCancel(handlers.ErrorMessageCause(errMsg))
					return
				}
				ensureSSEStarted()
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
				timing.MarkWrite()
				cliCancel(nil)
				return
			}

			timing.MarkData()

			h.forwardImagesStream(cliCtx, c, imageStreamForwardOptions{
				cancel:         func(err error) { cliCancel(err) },
				data:           dataChan,
				errs:           errChan,
				firstChunk:     chunk,
				responseFormat: responseFormat,
				streamPrefix:   streamPrefix,
				writeEvent:     writeEvent,
				writeKeepAlive: writeKeepAlive,
				timing:         timing,
			})
			return
		case now := <-timing.keepAliveC:
			maybeWriteImageStreamKeepAlive(timing, now, writeKeepAlive)
		case now := <-timing.dataIntervalC:
			if !timing.IdleTimedOut(now) {
				continue
			}
			errMsg := imageStreamIdleTimeoutError(timing.dataIntervalTimeout)
			emitImagesStreamError(writeEvent, errMsg)
			cliCancel(handlers.ErrorMessageCause(errMsg))
			return
		}
	}
}
