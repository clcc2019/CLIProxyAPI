package openai

import (
	"encoding/json"
	"fmt"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	imageGenerateStringToolFields = []string{"size", "quality", "background", "output_format", "moderation", "style"}
	imageEditStringToolFields     = []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation", "style"}
	imageNumberToolFields         = []string{"output_compression", "partial_images"}
)

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
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	switch {
	case raw == "1", strings.EqualFold(raw, "true"), strings.EqualFold(raw, "yes"), strings.EqualFold(raw, "on"):
		return true
	case raw == "0", strings.EqualFold(raw, "false"), strings.EqualFold(raw, "no"), strings.EqualFold(raw, "off"):
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
