package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type imageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
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

	responseFormat = normalizeImagesResponseFormat(responseFormat)

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
