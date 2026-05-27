package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
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
	imageGenerateStringToolFields = []string{"size", "quality", "background", "output_format", "moderation", "style"}
	imageEditStringToolFields     = []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation", "style"}
	imageNumberToolFields         = []string{"output_compression", "partial_images"}
	errImageStreamNilChannels     = errors.New("image stream received nil data and error channels")
)

type imageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

type sseFrameAccumulator struct {
	pending []byte
}

type xaiImageResult struct {
	B64JSON       string
	URL           string
	RevisedPrompt string
	MimeType      string
}

type imagesStreamExecutionResult struct {
	Data            <-chan []byte
	UpstreamHeaders http.Header
	Errs            <-chan *interfaces.ErrorMessage
}

type imageStreamTiming struct {
	keepAliveInterval   time.Duration
	dataIntervalTimeout time.Duration
	lastDataAt          time.Time
	lastWriteAt         time.Time
	keepAliveTicker     *time.Ticker
	dataIntervalTicker  *time.Ticker
	keepAliveC          <-chan time.Time
	dataIntervalC       <-chan time.Time
}

func newImageStreamTiming(keepAliveInterval, dataIntervalTimeout time.Duration) *imageStreamTiming {
	now := time.Now()
	timing := &imageStreamTiming{
		keepAliveInterval:   keepAliveInterval,
		dataIntervalTimeout: dataIntervalTimeout,
		lastDataAt:          now,
		lastWriteAt:         now,
	}
	if keepAliveInterval > 0 {
		timing.keepAliveTicker = time.NewTicker(keepAliveInterval)
		timing.keepAliveC = timing.keepAliveTicker.C
	}
	if dataIntervalTimeout > 0 {
		timing.dataIntervalTicker = time.NewTicker(dataIntervalTimeout)
		timing.dataIntervalC = timing.dataIntervalTicker.C
	}
	return timing
}

func (h *OpenAIAPIHandler) newImageStreamTiming() *imageStreamTiming {
	return newImageStreamTiming(h.imageStreamKeepAliveInterval(), h.imageStreamDataIntervalTimeout())
}

func (t *imageStreamTiming) Stop() {
	if t == nil {
		return
	}
	if t.keepAliveTicker != nil {
		t.keepAliveTicker.Stop()
	}
	if t.dataIntervalTicker != nil {
		t.dataIntervalTicker.Stop()
	}
}

func (t *imageStreamTiming) MarkData() {
	if t != nil {
		t.lastDataAt = time.Now()
	}
}

func (t *imageStreamTiming) MarkWrite() {
	if t != nil {
		t.lastWriteAt = time.Now()
	}
}

func (t *imageStreamTiming) KeepAliveDue(now time.Time) bool {
	return t != nil && t.keepAliveInterval > 0 && now.Sub(t.lastWriteAt) >= t.keepAliveInterval
}

func (t *imageStreamTiming) IdleTimedOut(now time.Time) bool {
	return t != nil && t.dataIntervalTimeout > 0 && now.Sub(t.lastDataAt) >= t.dataIntervalTimeout
}

func (h *OpenAIAPIHandler) imageStreamKeepAliveInterval() time.Duration {
	seconds := 0
	if h != nil && h.Cfg != nil {
		seconds = h.Cfg.ImageStreamKeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (h *OpenAIAPIHandler) imageStreamDataIntervalTimeout() time.Duration {
	seconds := 0
	if h != nil && h.Cfg != nil {
		seconds = h.Cfg.ImageStreamDataIntervalTimeoutSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func maybeWriteImageStreamKeepAlive(timing *imageStreamTiming, now time.Time, writeKeepAlive func()) {
	if timing == nil || !timing.KeepAliveDue(now) || writeKeepAlive == nil {
		return
	}
	writeKeepAlive()
}

func imageStreamIdleTimeoutError(timeout time.Duration) *interfaces.ErrorMessage {
	if timeout <= 0 {
		timeout = 0
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusGatewayTimeout,
		Error:      fmt.Errorf("upstream image stream idle for %s", timeout),
	}
}

func waitImagesStreamExecution(c *gin.Context, timing *imageStreamTiming, writeKeepAlive func(), execute func() imagesStreamExecutionResult) (imagesStreamExecutionResult, bool) {
	resultChan := make(chan imagesStreamExecutionResult, 1)
	go func() {
		resultChan <- execute()
	}()

	for {
		select {
		case <-c.Request.Context().Done():
			return imagesStreamExecutionResult{}, true
		case result := <-resultChan:
			return result, false
		case now := <-timing.keepAliveC:
			maybeWriteImageStreamKeepAlive(timing, now, writeKeepAlive)
		}
	}
}

func (a *sseFrameAccumulator) AddChunk(chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return nil
	}

	if responsesSSENeedsLineBreak(a.pending, chunk) {
		a.pending = append(a.pending, '\n')
	}
	a.pending = append(a.pending, chunk...)

	var frames [][]byte
	for {
		frameLen := responsesSSEFrameLen(a.pending)
		if frameLen == 0 {
			break
		}
		frames = append(frames, a.pending[:frameLen])
		copy(a.pending, a.pending[frameLen:])
		a.pending = a.pending[:len(a.pending)-frameLen]
	}

	if len(bytes.TrimSpace(a.pending)) == 0 {
		a.pending = a.pending[:0]
		return frames
	}
	if len(a.pending) == 0 || !responsesSSECanEmitWithoutDelimiter(a.pending, false) {
		return frames
	}
	frames = append(frames, a.pending)
	a.pending = a.pending[:0]
	return frames
}

func (a *sseFrameAccumulator) Flush() [][]byte {
	if len(a.pending) == 0 {
		return nil
	}

	var frames [][]byte
	for {
		frameLen := responsesSSEFrameLen(a.pending)
		if frameLen == 0 {
			break
		}
		frames = append(frames, a.pending[:frameLen])
		copy(a.pending, a.pending[frameLen:])
		a.pending = a.pending[:len(a.pending)-frameLen]
	}

	if len(bytes.TrimSpace(a.pending)) == 0 {
		a.pending = nil
		return frames
	}
	if responsesSSECanEmitWithoutDelimiter(a.pending, false) {
		frames = append(frames, a.pending)
	}
	a.pending = nil
	return frames
}

func imagesModelParts(model string) (prefix string, baseModel string) {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return strings.TrimSpace(model[:idx]), strings.TrimSpace(model[idx+1:])
	}
	return "", model
}

func imagesModelBase(model string) string {
	_, baseModel := imagesModelParts(model)
	return strings.ToLower(strings.TrimSpace(baseModel))
}

func isXAIImagesModel(model string) bool {
	prefix, baseModel := imagesModelParts(model)
	baseModel = strings.ToLower(strings.TrimSpace(baseModel))
	if baseModel != defaultXAIImagesModel && baseModel != xaiImagesQualityModel {
		return false
	}

	prefix = strings.ToLower(strings.TrimSpace(prefix))
	return prefix == "" || prefix == "xai" || prefix == "x-ai" || prefix == "grok"
}

func isSupportedImagesModel(model string) bool {
	baseModel := imagesModelBase(model)
	if baseModel == defaultImagesToolModel {
		return true
	}
	return isXAIImagesModel(model) || isOpenAICompatImagesModel(model)
}

func isDefaultImagesToolModel(model string) bool {
	return imagesModelBase(model) == defaultImagesToolModel
}

func isOpenAICompatImagesModel(model string) bool {
	return openAICompatibleImageModel(model)
}

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

func normalizeImagesResponseFormat(responseFormat string) string {
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		return "url"
	}
	return "b64_json"
}

func mimeTypeFromOutputFormat(outputFormat string) string {
	if outputFormat == "" {
		return "image/png"
	}
	if strings.Contains(outputFormat, "/") {
		return outputFormat
	}
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func multipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	f, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("open upload file failed: %w", err)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("openai images: close upload file error: %v", errClose)
		}
	}()

	data, err := util.ReadResponseBodyLimited(f, maxImageUploadBytes)
	if err != nil {
		if errors.Is(err, util.ErrResponseBodyTooLarge) {
			return "", fmt.Errorf("upload file exceeds maximum allowed size of %d bytes", maxImageUploadBytes)
		}
		return "", fmt.Errorf("read upload file failed: %w", err)
	}

	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	return "data:" + mediaType + ";base64," + b64, nil
}

func buildOpenAICompatImagesJSONRequest(rawJSON []byte, imageModel string, stream bool) []byte {
	payload := rawJSON
	if model := strings.TrimSpace(imageModel); model != "" {
		payload, _ = sjson.SetBytes(payload, "model", model)
	}
	if stream {
		payload, _ = sjson.SetBytes(payload, "stream", true)
	} else {
		payload, _ = sjson.DeleteBytes(payload, "stream")
	}
	return payload
}

func cloneMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func buildOpenAICompatImagesMultipartRequest(form *multipart.Form, imageModel string, stream bool) ([]byte, string, error) {
	if form == nil {
		return nil, "", fmt.Errorf("multipart form is nil")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if errWrite := writer.WriteField("model", imageModel); errWrite != nil {
		return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, "", fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
		for _, value := range values {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				return nil, "", fmt.Errorf("write form field %s failed: %w", key, errWrite)
			}
		}
	}

	for key, files := range form.File {
		for _, fileHeader := range files {
			if fileHeader == nil {
				continue
			}
			header := cloneMIMEHeader(fileHeader.Header)
			header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/octet-stream")
			}
			part, errCreate := writer.CreatePart(header)
			if errCreate != nil {
				return nil, "", fmt.Errorf("create file field %s failed: %w", key, errCreate)
			}
			src, errOpen := fileHeader.Open()
			if errOpen != nil {
				return nil, "", fmt.Errorf("open upload file failed: %w", errOpen)
			}
			_, errCopy := io.Copy(part, src)
			if errClose := src.Close(); errClose != nil {
				log.Errorf("openai images: close upload file error: %v", errClose)
				if errCopy == nil {
					errCopy = errClose
				}
			}
			if errCopy != nil {
				return nil, "", fmt.Errorf("copy upload file failed: %w", errCopy)
			}
		}
	}

	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func canonicalXAIImagesModel(model string) string {
	baseModel := imagesModelBase(model)
	if baseModel == xaiImagesQualityModel {
		return xaiImagesQualityModel
	}
	return defaultXAIImagesModel
}

func xaiImagesAspectRatio(raw string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1:1", "square":
		return "1:1"
	case "16:9", "landscape":
		return "16:9"
	case "9:16", "portrait":
		return "9:16"
	case "4:3":
		return "4:3"
	case "3:4":
		return "3:4"
	case "3:2":
		return "3:2"
	case "2:3":
		return "2:3"
	default:
		return fallback
	}
}

func xaiImagesAspectRatioFromSize(size string, fallback string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	switch size {
	case "1024x1024", "2048x2048", "1:1":
		return "1:1"
	case "1792x1024", "16:9":
		return "16:9"
	case "1024x1792", "9:16":
		return "9:16"
	case "1536x1024", "3:2":
		return "3:2"
	case "1024x1536", "2:3":
		return "2:3"
	default:
		return fallback
	}
}

func xaiImagesResolution(raw string, size string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1k", "2k":
		return strings.ToLower(strings.TrimSpace(raw))
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(size)), "2048") {
		return "2k"
	}
	return fallback
}

func xaiImagesRef(imageURL string) []byte {
	ref := []byte(`{"type":"image_url","url":""}`)
	ref, _ = sjson.SetBytes(ref, "url", strings.TrimSpace(imageURL))
	return ref
}

func buildXAIImagesBaseRequest(model string, prompt string, responseFormat string, aspectRatio string, resolution string, n int64) []byte {
	req := []byte(`{}`)
	req, _ = sjson.SetBytes(req, "model", canonicalXAIImagesModel(model))
	req, _ = sjson.SetBytes(req, "prompt", strings.TrimSpace(prompt))
	req, _ = sjson.SetBytes(req, "response_format", normalizeImagesResponseFormat(responseFormat))
	if aspectRatio != "" {
		req, _ = sjson.SetBytes(req, "aspect_ratio", aspectRatio)
	}
	if resolution != "" {
		req, _ = sjson.SetBytes(req, "resolution", resolution)
	}
	if n > 0 {
		req, _ = sjson.SetBytes(req, "n", n)
	}
	return req
}

func buildXAIImagesGenerationsRequest(rawJSON []byte, model string, responseFormat string) []byte {
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	size := strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String())
	aspectRatio := xaiImagesAspectRatio(gjson.GetBytes(rawJSON, "aspect_ratio").String(), "")
	aspectRatio = xaiImagesAspectRatioFromSize(size, aspectRatio)
	if aspectRatio == "" {
		aspectRatio = xaiImagesDefaultAspectRatio
	}
	resolution := xaiImagesResolution(gjson.GetBytes(rawJSON, "resolution").String(), size, xaiImagesDefaultResolution)
	n := int64(0)
	if v := gjson.GetBytes(rawJSON, "n"); v.Exists() && v.Type == gjson.Number {
		n = v.Int()
	}
	return buildXAIImagesBaseRequest(model, prompt, responseFormat, aspectRatio, resolution, n)
}

func buildXAIImagesEditRequest(model string, prompt string, images []string, responseFormat string, aspectRatio string, resolution string, n int64) []byte {
	req := buildXAIImagesBaseRequest(model, prompt, responseFormat, aspectRatio, resolution, n)
	trimmedImages := make([]string, 0, len(images))
	for _, img := range images {
		if strings.TrimSpace(img) != "" {
			trimmedImages = append(trimmedImages, strings.TrimSpace(img))
		}
	}
	if len(trimmedImages) == 1 {
		req, _ = sjson.SetRawBytes(req, "image", xaiImagesRef(trimmedImages[0]))
		return req
	}
	for _, img := range trimmedImages {
		req, _ = sjson.SetRawBytes(req, "images.-1", xaiImagesRef(img))
	}
	return req
}

func collectXAIImagesFromJSON(rawJSON []byte) []string {
	var images []string
	appendImage := func(url string) {
		url = strings.TrimSpace(url)
		if url != "" {
			images = append(images, url)
		}
	}

	if image := gjson.GetBytes(rawJSON, "image"); image.Exists() {
		if image.Type == gjson.String {
			appendImage(image.String())
		} else if image.Type == gjson.JSON {
			appendImage(image.Get("image_url.url").String())
			if imageURL := image.Get("image_url"); imageURL.Type == gjson.String {
				appendImage(imageURL.String())
			}
			appendImage(image.Get("url").String())
		}
	}
	if imagesResult := gjson.GetBytes(rawJSON, "images"); imagesResult.IsArray() {
		for _, img := range imagesResult.Array() {
			if img.Type == gjson.String {
				appendImage(img.String())
				continue
			}
			appendImage(img.Get("image_url.url").String())
			if imageURL := img.Get("image_url"); imageURL.Type == gjson.String {
				appendImage(imageURL.String())
			}
			appendImage(img.Get("url").String())
		}
	}
	return images
}

func xaiImagesEditOptionsFromJSON(rawJSON []byte) (aspectRatio string, resolution string, n int64) {
	size := strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String())
	aspectRatio = xaiImagesAspectRatio(gjson.GetBytes(rawJSON, "aspect_ratio").String(), "")
	aspectRatio = xaiImagesAspectRatioFromSize(size, aspectRatio)
	resolution = xaiImagesResolution(gjson.GetBytes(rawJSON, "resolution").String(), size, "")
	if v := gjson.GetBytes(rawJSON, "n"); v.Exists() && v.Type == gjson.Number {
		n = v.Int()
	}
	return aspectRatio, resolution, n
}

func parseIntField(raw string, fallback int64) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

func parseBoolField(raw string, fallback bool) bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func multipartImageFiles(form *multipart.Form) []*multipart.FileHeader {
	if form == nil {
		return nil
	}
	if files := form.File["image[]"]; len(files) > 0 {
		return files
	}
	if files := form.File["image"]; len(files) > 0 {
		return files
	}
	return nil
}

func newImageTool(action, model string) []byte {
	tool := []byte(`{"type":"image_generation"}`)
	tool, _ = sjson.SetBytes(tool, "action", action)
	tool, _ = sjson.SetBytes(tool, "model", model)
	return tool
}

func setJSONImageToolStringFields(tool []byte, rawJSON []byte, fields ...string) []byte {
	for _, field := range fields {
		if v := strings.TrimSpace(gjson.GetBytes(rawJSON, field).String()); v != "" {
			tool, _ = sjson.SetBytes(tool, field, v)
		}
	}
	return tool
}

func setJSONImageToolNumberFields(tool []byte, rawJSON []byte, fields ...string) []byte {
	for _, field := range fields {
		if v := gjson.GetBytes(rawJSON, field); v.Exists() && v.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, v.Int())
		}
	}
	return tool
}

func setFormImageToolStringFields(tool []byte, c *gin.Context, fields ...string) []byte {
	for _, field := range fields {
		if v := strings.TrimSpace(c.PostForm(field)); v != "" {
			tool, _ = sjson.SetBytes(tool, field, v)
		}
	}
	return tool
}

func setFormImageToolNumberFields(tool []byte, c *gin.Context, fields ...string) []byte {
	for _, field := range fields {
		if v := strings.TrimSpace(c.PostForm(field)); v != "" {
			tool, _ = sjson.SetBytes(tool, field, parseIntField(v, 0))
		}
	}
	return tool
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
	if openAICompatibleImageModel(imageModel) {
		nativeJSON := rawJSON
		if !responseFormatProvided {
			nativeJSON, _ = sjson.SetBytes(nativeJSON, "response_format", responseFormat)
		}
		h.dispatchImagesNative(c, nativeJSON, imageModel, stream, "images/generations")
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

	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(contentType, "application/json") {
		h.imagesEditsFromJSON(c)
		return
	}
	if strings.HasPrefix(contentType, "multipart/form-data") || contentType == "" {
		h.imagesEditsFromMultipart(c)
		return
	}

	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Invalid request: unsupported Content-Type %q", contentType),
			Type:    "invalid_request_error",
		},
	})
}

func (h *OpenAIAPIHandler) ImagesVariations(c *gin.Context) {
	if h.rejectImagesEndpointIfDisabled(c) {
		return
	}

	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") || contentType == "" {
		h.imagesVariationsFromMultipart(c)
		return
	}

	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Invalid request: unsupported Content-Type %q", contentType),
			Type:    "invalid_request_error",
		},
	})
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

func buildImagesResponsesRequest(prompt string, images []string, toolJSON []byte) []byte {
	req := []byte(`{"instructions":"","stream":true,"reasoning":{"effort":"medium","summary":"auto"},"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"}}`)
	req, _ = sjson.SetBytes(req, "model", defaultImagesMainModel)

	input := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`)
	input, _ = sjson.SetBytes(input, "0.content.0.text", prompt)
	contentIndex := 1
	for _, img := range images {
		if strings.TrimSpace(img) == "" {
			continue
		}
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", img)
		path := fmt.Sprintf("0.content.%d", contentIndex)
		input, _ = sjson.SetRawBytes(input, path, part)
		contentIndex++
	}
	req, _ = sjson.SetRawBytes(req, "input", input)

	req, _ = sjson.SetRawBytes(req, "tools", []byte(`[]`))
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		req, _ = sjson.SetRawBytes(req, "tools.-1", toolJSON)
	}
	return req
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
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel()
}

func extractXAIImagesResponse(payload []byte) (results []xaiImageResult, createdAt int64, usageRaw []byte, err error) {
	if !json.Valid(payload) {
		return nil, 0, nil, fmt.Errorf("upstream returned invalid image response JSON")
	}

	createdAt = gjson.GetBytes(payload, "created").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	data := gjson.GetBytes(payload, "data")
	if data.IsArray() {
		for _, item := range data.Array() {
			result := xaiImageResult{
				B64JSON:       strings.TrimSpace(item.Get("b64_json").String()),
				URL:           strings.TrimSpace(item.Get("url").String()),
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				MimeType:      strings.TrimSpace(item.Get("mime_type").String()),
			}
			if result.MimeType == "" {
				result.MimeType = mimeTypeFromOutputFormat(strings.TrimSpace(item.Get("output_format").String()))
			}
			if result.MimeType == "" {
				result.MimeType = "image/png"
			}
			if result.B64JSON == "" && result.URL == "" {
				continue
			}
			results = append(results, result)
		}
	}
	if len(results) == 0 {
		return nil, 0, nil, fmt.Errorf("upstream did not return image output")
	}

	if usage := gjson.GetBytes(payload, "usage"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}

	return results, createdAt, usageRaw, nil
}

func buildImagesAPIResponseFromXAI(payload []byte, responseFormat string) ([]byte, error) {
	results, createdAt, usageRaw, err := extractXAIImagesResponse(payload)
	if err != nil {
		return nil, err
	}

	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)
	responseFormat = normalizeImagesResponseFormat(responseFormat)

	for _, img := range results {
		item := []byte(`{}`)
		if responseFormat == "url" {
			if img.URL != "" {
				item, _ = sjson.SetBytes(item, "url", img.URL)
			} else {
				item, _ = sjson.SetBytes(item, "url", "data:"+mimeTypeFromOutputFormat(img.MimeType)+";base64,"+img.B64JSON)
			}
		} else if img.B64JSON != "" {
			item, _ = sjson.SetBytes(item, "b64_json", img.B64JSON)
		} else {
			item, _ = sjson.SetBytes(item, "url", img.URL)
		}
		if img.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", img.RevisedPrompt)
		}
		out, _ = sjson.SetRawBytes(out, "data.-1", item)
	}

	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}

	return out, nil
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
		path = strings.ToLower(strings.TrimSpace(c.Request.URL.Path))
	}
	switch {
	case strings.Contains(path, "/images/edits"):
		return "images/edits"
	case strings.Contains(path, "/images/variations"):
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
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
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
		if errMsg != nil && errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
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

func openAICompatibleImageModel(modelName string) bool {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return false
	}
	if info := registry.LookupModelInfo(modelName); info != nil && strings.EqualFold(strings.TrimSpace(info.Type), registry.OpenAIImageModelType) {
		return true
	}
	if !strings.Contains(modelName, "/") {
		return false
	}
	registryRef := registry.GetGlobalRegistry()
	for _, provider := range registryRef.GetModelProviders(modelName) {
		info := registryRef.GetModelInfo(modelName, provider)
		if info == nil {
			continue
		}
		typ := strings.TrimSpace(info.Type)
		if strings.EqualFold(typ, "openai-compatibility") || strings.EqualFold(typ, registry.OpenAIImageModelType) {
			return true
		}
	}
	return false
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
	out, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, "openai", modelName, rawPayload, alt)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
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
		data, headers, errs := h.ExecuteStreamWithAuthManager(cliCtx, "openai", modelName, rawPayload, alt)
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
			if sseStarted {
				emitImagesStreamError(writeEvent, errMsg)
			} else {
				h.WriteErrorResponse(c, errMsg)
			}
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
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
			cliCancel(errMsg.Error)
			return
		}
	}
}

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
			if ok && errMsg != nil {
				emitImagesStreamError(writeEvent, errMsg)
				cancel(errMsg.Error)
				return
			}
			errs = nil
			if data == nil {
				errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
				emitImagesStreamError(writeEvent, errMsg)
				cancel(errImageStreamNilChannels)
				return
			}
		case chunk, ok := <-data:
			if !ok {
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
			cancel(errMsg.Error)
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
			if ok && errMsg != nil {
				return nil, errMsg
			}
			errs = nil
			if data == nil {
				return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
			}
		case chunk, ok := <-data:
			if !ok {
				for _, frame := range acc.Flush() {
					if out, done, errMsg := processFrame(frame); errMsg != nil {
						return nil, errMsg
					} else if done {
						return out, nil
					}
				}
				return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("stream disconnected before completion")}
			}
			for _, frame := range acc.AddChunk(chunk) {
				if out, done, errMsg := processFrame(frame); errMsg != nil {
					return nil, errMsg
				} else if done {
					return out, nil
				}
			}
		}
	}
}

func extractImagesFromResponsesCompleted(payload []byte) (results []imageCallResult, createdAt int64, usageRaw []byte, firstMeta imageCallResult, err error) {
	if gjson.GetBytes(payload, "type").String() != "response.completed" {
		return nil, 0, nil, imageCallResult{}, fmt.Errorf("unexpected event type")
	}

	createdAt = gjson.GetBytes(payload, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() != "image_generation_call" {
				return true
			}
			res := strings.TrimSpace(item.Get("result").String())
			if res == "" {
				return true
			}
			entry := imageCallResult{
				Result:        res,
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
				Size:          strings.TrimSpace(item.Get("size").String()),
				Background:    strings.TrimSpace(item.Get("background").String()),
				Quality:       strings.TrimSpace(item.Get("quality").String()),
			}
			if len(results) == 0 {
				firstMeta = entry
			}
			results = append(results, entry)
			return true
		})
	}

	if usage := gjson.GetBytes(payload, "response.tool_usage.image_gen"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}

	return results, createdAt, usageRaw, firstMeta, nil
}

type imageResponseCollectState struct {
	results []imageCallResult
	seen    map[string]struct{}
}

func newImageResponseCollectState() *imageResponseCollectState {
	return &imageResponseCollectState{seen: make(map[string]struct{})}
}

func (s *imageResponseCollectState) AddOutputItemDone(payload []byte) error {
	if s == nil {
		return nil
	}
	result, itemID, ok, err := extractImageFromResponsesOutputItemDone(payload)
	if err != nil || !ok {
		return err
	}
	appendImageCallResultDedup(&s.results, s.seen, itemID, result)
	return nil
}

func (s *imageResponseCollectState) PendingResults() []imageCallResult {
	if s == nil || len(s.results) == 0 {
		return nil
	}
	out := make([]imageCallResult, len(s.results))
	copy(out, s.results)
	return out
}

func extractImageFromResponsesOutputItemDone(payload []byte) (imageCallResult, string, bool, error) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return imageCallResult{}, "", false, fmt.Errorf("unexpected event type")
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || item.Get("type").String() != "image_generation_call" {
		return imageCallResult{}, "", false, nil
	}
	res := strings.TrimSpace(item.Get("result").String())
	if res == "" {
		return imageCallResult{}, "", false, nil
	}
	return imageCallResult{
		Result:        res,
		RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
		OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
		Size:          strings.TrimSpace(item.Get("size").String()),
		Background:    strings.TrimSpace(item.Get("background").String()),
		Quality:       strings.TrimSpace(item.Get("quality").String()),
	}, strings.TrimSpace(item.Get("id").String()), true, nil
}

func appendImageCallResultDedup(results *[]imageCallResult, seen map[string]struct{}, itemID string, result imageCallResult) bool {
	if results == nil {
		return false
	}
	key := imageCallResultKey(itemID, result)
	if key != "" && seen != nil {
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
	}
	*results = append(*results, result)
	return true
}

func imageCallResultKey(itemID string, result imageCallResult) string {
	if strings.TrimSpace(result.Result) != "" {
		return strings.TrimSpace(result.OutputFormat) + "|" + strings.TrimSpace(result.Result)
	}
	if strings.TrimSpace(itemID) != "" {
		return "item:" + strings.TrimSpace(itemID)
	}
	return ""
}

func buildImagesAPIResponse(results []imageCallResult, createdAt int64, usageRaw []byte, firstMeta imageCallResult, responseFormat string) ([]byte, error) {
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)

	responseFormat = strings.ToLower(strings.TrimSpace(responseFormat))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}

	for _, img := range results {
		item := []byte(`{}`)
		if responseFormat == "url" {
			mt := mimeTypeFromOutputFormat(img.OutputFormat)
			item, _ = sjson.SetBytes(item, "url", "data:"+mt+";base64,"+img.Result)
		} else {
			item, _ = sjson.SetBytes(item, "b64_json", img.Result)
		}
		if img.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", img.RevisedPrompt)
		}
		out, _ = sjson.SetRawBytes(out, "data.-1", item)
	}

	if firstMeta.Background != "" {
		out, _ = sjson.SetBytes(out, "background", firstMeta.Background)
	}
	if firstMeta.OutputFormat != "" {
		out, _ = sjson.SetBytes(out, "output_format", firstMeta.OutputFormat)
	}
	if firstMeta.Quality != "" {
		out, _ = sjson.SetBytes(out, "quality", firstMeta.Quality)
	}
	if firstMeta.Size != "" {
		out, _ = sjson.SetBytes(out, "size", firstMeta.Size)
	}

	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}

	return out, nil
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
			if sseStarted {
				emitImagesStreamError(writeEvent, errMsg)
			} else {
				h.WriteErrorResponse(c, errMsg)
			}
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
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
			cliCancel(errMsg.Error)
			return
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

	responseFormat := strings.ToLower(strings.TrimSpace(opts.responseFormat))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}

	processFrame := func(frame []byte) (done bool) {
		return processImagesStreamFrame(frame, responseFormat, opts.streamPrefix, opts.writeEvent, state)
	}
	timing := opts.timing
	var keepAliveC, dataIntervalC <-chan time.Time
	if timing != nil {
		keepAliveC = timing.keepAliveC
		dataIntervalC = timing.dataIntervalC
	}

	for _, frame := range acc.AddChunk(opts.firstChunk) {
		if processFrame(frame) {
			opts.cancel(nil)
			return
		}
	}

	for {
		select {
		case <-requestCtx.Done():
			opts.cancel(requestCtx.Err())
			return
		case errMsg, ok := <-opts.errs:
			if ok && errMsg != nil {
				emitImagesStreamError(opts.writeEvent, errMsg)
				opts.cancel(errMsg.Error)
				return
			}
			opts.errs = nil
			if opts.data == nil {
				errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errImageStreamNilChannels}
				emitImagesStreamError(opts.writeEvent, errMsg)
				opts.cancel(errImageStreamNilChannels)
				return
			}
		case chunk, ok := <-opts.data:
			if !ok {
				for _, frame := range acc.Flush() {
					if processFrame(frame) {
						opts.cancel(nil)
						return
					}
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
			for _, frame := range acc.AddChunk(chunk) {
				if processFrame(frame) {
					opts.cancel(nil)
					return
				}
			}
		case now := <-keepAliveC:
			maybeWriteImageStreamKeepAlive(timing, now, opts.writeKeepAlive)
		case now := <-dataIntervalC:
			if !timing.IdleTimedOut(now) {
				continue
			}
			errMsg := imageStreamIdleTimeoutError(timing.dataIntervalTimeout)
			emitImagesStreamError(opts.writeEvent, errMsg)
			opts.cancel(errMsg.Error)
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
