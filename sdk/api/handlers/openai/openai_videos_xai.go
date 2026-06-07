package openai

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type xaiVideoCreateMetadata struct {
	Model     string
	Prompt    string
	Seconds   string
	Size      string
	CreatedAt int64
}

func videosModelBase(model string) string {
	_, baseModel := imagesModelParts(model)
	return strings.ToLower(strings.TrimSpace(baseModel))
}

func isXAIVideosModel(model string) bool {
	prefix, baseModel := imagesModelParts(model)
	baseModel = strings.TrimSpace(baseModel)
	if !strings.EqualFold(baseModel, defaultXAIVideosModel) && !strings.EqualFold(baseModel, xaiVideos15PreviewModel) {
		return false
	}

	prefix = strings.TrimSpace(prefix)
	return prefix == "" ||
		strings.EqualFold(prefix, "xai") ||
		strings.EqualFold(prefix, "x-ai") ||
		strings.EqualFold(prefix, "grok")
}

func isSupportedVideosModel(model string) bool {
	return isXAIVideosModel(model)
}

func canonicalXAIVideosModel(model string) string {
	switch videosModelBase(model) {
	case defaultXAIVideosModel:
		return defaultXAIVideosModel
	case xaiVideos15PreviewModel:
		return xaiVideos15PreviewModel
	}
	return defaultXAIVideosModel
}

func buildXAIVideosCreateRequest(rawJSON []byte, model string) ([]byte, xaiVideoCreateMetadata, error) {
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	if prompt == "" {
		return nil, xaiVideoCreateMetadata{}, fmt.Errorf("prompt is required")
	}

	seconds, duration, err := normalizeXAIVideosSeconds(gjson.GetBytes(rawJSON, "seconds").String())
	if err != nil {
		return nil, xaiVideoCreateMetadata{}, err
	}

	size, aspectRatio, resolution, err := xaiVideosSizeOptions(gjson.GetBytes(rawJSON, "size").String())
	if err != nil {
		return nil, xaiVideoCreateMetadata{}, err
	}
	if value := xaiVideosAspectRatio(gjson.GetBytes(rawJSON, "aspect_ratio").String(), ""); value != "" {
		aspectRatio = value
	}
	if value := xaiVideosResolution(gjson.GetBytes(rawJSON, "resolution").String(), ""); value != "" {
		resolution = value
	}

	imageURL, err := xaiVideosInputImageURL(rawJSON)
	if err != nil {
		return nil, xaiVideoCreateMetadata{}, err
	}
	referenceImages := collectXAIVideoReferenceImages(rawJSON)
	if len(referenceImages) > maxXAIVideoReferences {
		return nil, xaiVideoCreateMetadata{}, fmt.Errorf("reference_images supports at most %d images on xAI", maxXAIVideoReferences)
	}
	if imageURL != "" && len(referenceImages) > 0 {
		return nil, xaiVideoCreateMetadata{}, fmt.Errorf("image and reference_images cannot be combined on xAI")
	}
	if len(referenceImages) > 0 && duration > 10 {
		duration = 10
		seconds = "10"
	}

	videoModel := canonicalXAIVideosModel(model)
	req := []byte(`{}`)
	req, _ = sjson.SetBytes(req, "model", videoModel)
	req, _ = sjson.SetBytes(req, "prompt", prompt)
	req, _ = sjson.SetRawBytes(req, "duration", []byte(strconv.FormatInt(duration, 10)))
	req, _ = sjson.SetBytes(req, "aspect_ratio", aspectRatio)
	req, _ = sjson.SetBytes(req, "resolution", resolution)
	if imageURL != "" {
		req, _ = sjson.SetBytes(req, "image.url", imageURL)
	}
	for _, image := range referenceImages {
		req, _ = sjson.SetBytes(req, "reference_images.-1.url", image)
	}

	meta := xaiVideoCreateMetadata{
		Model:     videoModel,
		Prompt:    prompt,
		Seconds:   seconds,
		Size:      size,
		CreatedAt: time.Now().Unix(),
	}
	return req, meta, nil
}

func normalizeXAIVideosSeconds(raw string) (string, int64, error) {
	seconds := strings.TrimSpace(raw)
	if seconds == "" {
		seconds = defaultVideosSeconds
	}
	duration, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("seconds must be an integer")
	}
	if duration < 1 {
		duration = 1
	}
	if duration > 15 {
		duration = 15
	}
	return strconv.FormatInt(duration, 10), duration, nil
}

func xaiVideosSizeOptions(raw string) (size string, aspectRatio string, resolution string, err error) {
	size = strings.TrimSpace(raw)
	if size == "" {
		size = defaultVideosSize
	}
	switch size {
	case "720x1280", "1024x1792":
		return size, "9:16", defaultVideosResolution, nil
	case "1280x720", "1792x1024":
		return size, "16:9", defaultVideosResolution, nil
	default:
		return "", "", "", fmt.Errorf("size must be one of 720x1280, 1280x720, 1024x1792, or 1792x1024")
	}
}

func xaiVideosAspectRatio(raw string, fallback string) string {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "1:1" || strings.EqualFold(raw, "square"):
		return "1:1"
	case raw == "16:9" || strings.EqualFold(raw, "landscape"):
		return "16:9"
	case raw == "9:16" || strings.EqualFold(raw, "portrait"):
		return "9:16"
	case raw == "4:3":
		return "4:3"
	case raw == "3:4":
		return "3:4"
	case raw == "3:2":
		return "3:2"
	case raw == "2:3":
		return "2:3"
	default:
		return fallback
	}
}

func xaiVideosResolution(raw string, fallback string) string {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.EqualFold(raw, "480p"):
		return "480p"
	case strings.EqualFold(raw, "720p"):
		return "720p"
	default:
		return fallback
	}
}

func xaiVideosInputImageURL(rawJSON []byte) (string, error) {
	inputRef := gjson.GetBytes(rawJSON, "input_reference")
	if inputRef.Exists() {
		imageURL := strings.TrimSpace(inputRef.Get("image_url").String())
		fileID := strings.TrimSpace(inputRef.Get("file_id").String())
		if imageURL != "" && fileID != "" {
			return "", fmt.Errorf("input_reference must provide exactly one of image_url or file_id")
		}
		if fileID != "" {
			return "", fmt.Errorf("input_reference.file_id is not supported for xAI video generation; use input_reference.image_url")
		}
		if imageURL != "" {
			return imageURL, nil
		}
	}

	image := gjson.GetBytes(rawJSON, "image")
	if image.Exists() {
		if image.Type == gjson.String {
			return strings.TrimSpace(image.String()), nil
		}
		if value := strings.TrimSpace(image.Get("url").String()); value != "" {
			return value, nil
		}
		if value := strings.TrimSpace(image.Get("image_url.url").String()); value != "" {
			return value, nil
		}
	}

	return strings.TrimSpace(gjson.GetBytes(rawJSON, "image_url").String()), nil
}

func collectXAIVideoReferenceImages(rawJSON []byte) []string {
	out := make([]string, 0)
	appendRef := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	collectArray := func(result gjson.Result) {
		if !result.IsArray() {
			return
		}
		result.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				appendRef(item.String())
				return true
			}
			if value := item.Get("url").String(); value != "" {
				appendRef(value)
				return true
			}
			if value := item.Get("image_url.url").String(); value != "" {
				appendRef(value)
			}
			return true
		})
	}
	collectArray(gjson.GetBytes(rawJSON, "reference_images"))
	collectArray(gjson.GetBytes(rawJSON, "reference_image_urls"))
	return out
}

func buildVideosCreateAPIResponseFromXAI(payload []byte, meta xaiVideoCreateMetadata) ([]byte, error) {
	requestID := strings.TrimSpace(gjson.GetBytes(payload, "request_id").String())
	if requestID == "" {
		requestID = strings.TrimSpace(gjson.GetBytes(payload, "id").String())
	}
	if requestID == "" {
		return nil, fmt.Errorf("xAI video response did not include request_id")
	}

	out := []byte(`{"object":"video","progress":0,"status":"queued"}`)
	out, _ = sjson.SetBytes(out, "id", requestID)
	out, _ = sjson.SetBytes(out, "model", meta.Model)
	out, _ = sjson.SetBytes(out, "prompt", meta.Prompt)
	out, _ = sjson.SetBytes(out, "seconds", meta.Seconds)
	out, _ = sjson.SetBytes(out, "size", meta.Size)
	out, _ = sjson.SetBytes(out, "created_at", meta.CreatedAt)
	if status := openAIVideoStatus(gjson.GetBytes(payload, "status").String()); status != "" {
		out, _ = sjson.SetBytes(out, "status", status)
	}
	if progress := gjson.GetBytes(payload, "progress"); progress.Exists() {
		out, _ = sjson.SetRawBytes(out, "progress", []byte(progress.Raw))
	}
	return out, nil
}

func buildVideosRetrieveAPIResponseFromXAI(videoID string, payload []byte, fallbackModel string) ([]byte, error) {
	out := []byte(`{"object":"video"}`)
	out, _ = sjson.SetBytes(out, "id", videoID)

	model := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if model == "" {
		model = fallbackModel
	}
	out, _ = sjson.SetBytes(out, "model", model)

	if status := openAIVideoStatus(gjson.GetBytes(payload, "status").String()); status != "" {
		out, _ = sjson.SetBytes(out, "status", status)
	}
	if progress := gjson.GetBytes(payload, "progress"); progress.Exists() {
		out, _ = sjson.SetRawBytes(out, "progress", []byte(progress.Raw))
	}
	if duration := gjson.GetBytes(payload, "video.duration"); duration.Exists() {
		out, _ = sjson.SetBytes(out, "seconds", duration.String())
	}
	if video := gjson.GetBytes(payload, "video"); video.Exists() && json.Valid([]byte(video.Raw)) {
		out, _ = sjson.SetRawBytes(out, "video", []byte(video.Raw))
	}
	if usage := gjson.GetBytes(payload, "usage"); usage.Exists() && json.Valid([]byte(usage.Raw)) {
		out, _ = sjson.SetRawBytes(out, "usage", []byte(usage.Raw))
	}
	if errPayload := gjson.GetBytes(payload, "error"); errPayload.Exists() && json.Valid([]byte(errPayload.Raw)) {
		out, _ = sjson.SetRawBytes(out, "error", []byte(errPayload.Raw))
	}
	return out, nil
}

func openAIVideoStatus(status string) string {
	status = strings.TrimSpace(status)
	switch {
	case strings.EqualFold(status, "queued") || strings.EqualFold(status, "pending"):
		return "queued"
	case strings.EqualFold(status, "in_progress") || strings.EqualFold(status, "processing") || strings.EqualFold(status, "running"):
		return "in_progress"
	case strings.EqualFold(status, "completed") || strings.EqualFold(status, "done") || strings.EqualFold(status, "succeeded") || strings.EqualFold(status, "success"):
		return "completed"
	case strings.EqualFold(status, "failed") || strings.EqualFold(status, "error") || strings.EqualFold(status, "expired") || strings.EqualFold(status, "cancelled") || strings.EqualFold(status, "canceled"):
		return "failed"
	default:
		return ""
	}
}
