package util

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeToolResultImage represents a base64-encoded image extracted from a Claude
// tool_result content block.
type ClaudeToolResultImage struct {
	MimeType string
	Data     string
}

// ClaudeToolResult is the normalized form of a Claude tool_result content field.
type ClaudeToolResult struct {
	Result      string
	ResultIsRaw bool
	Images      []ClaudeToolResultImage
}

// ConvertClaudeToolResultContent normalizes Claude tool_result content for a
// Gemini-style functionResponse while extracting inline images.
func ConvertClaudeToolResultContent(content gjson.Result) ClaudeToolResult {
	switch {
	case content.Type == gjson.String:
		return ClaudeToolResult{Result: content.String()}
	case content.IsArray():
		var images []ClaudeToolResultImage
		nonImageCount := 0
		lastNonImageRaw := ""
		filtered := []byte(`[]`)
		content.ForEach(func(_, block gjson.Result) bool {
			if isClaudeBase64Image(block) {
				if image, ok := claudeImageFromBlock(block); ok {
					images = append(images, image)
				}
				return true
			}
			nonImageCount++
			lastNonImageRaw = block.Raw
			filtered, _ = sjson.SetRawBytes(filtered, "-1", []byte(block.Raw))
			return true
		})
		switch {
		case nonImageCount == 1:
			return ClaudeToolResult{Result: lastNonImageRaw, ResultIsRaw: true, Images: images}
		case nonImageCount > 1:
			return ClaudeToolResult{Result: string(filtered), ResultIsRaw: true, Images: images}
		default:
			return ClaudeToolResult{Images: images}
		}
	case content.IsObject():
		if isClaudeBase64Image(content) {
			if image, ok := claudeImageFromBlock(content); ok {
				return ClaudeToolResult{Images: []ClaudeToolResultImage{image}}
			}
			return ClaudeToolResult{}
		}
		return ClaudeToolResult{Result: content.Raw, ResultIsRaw: true}
	case content.Raw != "":
		return ClaudeToolResult{Result: content.Raw, ResultIsRaw: true}
	default:
		return ClaudeToolResult{}
	}
}

func isClaudeBase64Image(block gjson.Result) bool {
	return block.Get("type").String() == "image" && block.Get("source.type").String() == "base64"
}

func claudeImageFromBlock(block gjson.Result) (ClaudeToolResultImage, bool) {
	data := block.Get("source.data").String()
	if data == "" {
		return ClaudeToolResultImage{}, false
	}
	return ClaudeToolResultImage{
		MimeType: block.Get("source.media_type").String(),
		Data:     data,
	}, true
}
