package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type xaiImageResult struct {
	B64JSON       string
	URL           string
	RevisedPrompt string
	MimeType      string
}

func canonicalXAIImagesModel(model string) string {
	baseModel := imagesModelBase(model)
	if baseModel == xaiImagesQualityModel {
		return xaiImagesQualityModel
	}
	return defaultXAIImagesModel
}

func xaiImagesAspectRatio(raw string, fallback string) string {
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

func xaiImagesAspectRatioFromSize(size string, fallback string) string {
	size = strings.TrimSpace(size)
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
	raw = strings.TrimSpace(raw)
	switch {
	case strings.EqualFold(raw, "1k"):
		return "1k"
	case strings.EqualFold(raw, "2k"):
		return "2k"
	}
	if strings.Contains(strings.TrimSpace(size), "2048") {
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
