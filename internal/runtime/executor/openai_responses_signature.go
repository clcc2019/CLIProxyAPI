package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func sanitizeOpenAIResponsesReasoningEncryptedContent(ctx context.Context, provider string, body []byte) []byte {
	if !bytes.Contains(body, []byte("encrypted_content")) {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai responses upstream"
	}

	items := input.Array()
	rawItems := make([][]byte, 0, len(items))
	changed := false
	for index, item := range items {
		rawItem := []byte(item.Raw)
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			rawItems = append(rawItems, rawItem)
			continue
		}

		encryptedContent := item.Get("encrypted_content")
		if !encryptedContent.Exists() {
			rawItems = append(rawItems, rawItem)
			continue
		}

		reason := ""
		switch encryptedContent.Type {
		case gjson.String:
			rawSignature := encryptedContent.String()
			if rawSignature != strings.TrimSpace(rawSignature) {
				reason = "encrypted_content has leading or trailing whitespace"
			} else if _, err := signature.InspectGPTReasoningSignature(rawSignature); err != nil {
				reason = err.Error()
			}
		case gjson.Null:
			reason = "encrypted_content is null"
		default:
			reason = fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
		}
		if reason == "" {
			rawItems = append(rawItems, rawItem)
			continue
		}

		next, err := sjson.DeleteBytes(rawItem, "encrypted_content")
		if err != nil {
			helps.LogWithRequestID(ctx).Debugf("%s: failed to drop invalid reasoning encrypted_content at input[%d]: %v", provider, index, err)
			rawItems = append(rawItems, rawItem)
			continue
		}
		rawItems = append(rawItems, next)
		changed = true

		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			itemID = fmt.Sprintf("input[%d]", index)
		}
		helps.LogWithRequestID(ctx).Debugf("%s: dropped invalid reasoning encrypted_content at input[%d] item_id=%q reason=%s", provider, index, itemID, reason)
	}
	if !changed {
		return body
	}
	updated, err := helps.SetRawJSONBytes(body, "input", codexRawJSONArray(rawItems))
	if err != nil {
		helps.LogWithRequestID(ctx).Debugf("%s: failed to rewrite sanitized reasoning input: %v", provider, err)
		return body
	}
	return updated
}
