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
	input := codexGJSONGetImmutableBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	hasReasoning := false
	input.ForEach(func(_, item gjson.Result) bool {
		if strings.TrimSpace(item.Get("type").String()) == "reasoning" {
			hasReasoning = true
			return false
		}
		return true
	})
	if !hasReasoning {
		return body
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai responses upstream"
	}
	stripOrphanReasoningIDs := !codexGJSONGetImmutableBytes(body, "store").Bool()

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
			if stripOrphanReasoningIDs && item.Get("id").Exists() {
				next, err := sjson.DeleteBytes(rawItem, "id")
				if err != nil {
					helps.LogWithRequestID(ctx).Debugf("%s: failed to drop orphan reasoning id at input[%d]: %v", provider, index, err)
					rawItems = append(rawItems, rawItem)
					continue
				}
				rawItems = append(rawItems, next)
				changed = true
				helps.LogWithRequestID(ctx).Debugf("%s: dropped orphan reasoning id at input[%d] item_id=%q reason=missing encrypted_content with store disabled", provider, index, strings.TrimSpace(item.Get("id").String()))
				continue
			}
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
		if stripOrphanReasoningIDs && item.Get("id").Exists() {
			if nextWithoutID, errID := sjson.DeleteBytes(next, "id"); errID != nil {
				helps.LogWithRequestID(ctx).Debugf("%s: failed to drop reasoning id after invalid encrypted_content at input[%d]: %v", provider, index, errID)
			} else {
				next = nextWithoutID
			}
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
	updated, err := sjson.SetRawBytes(body, "input", codexRawJSONArray(rawItems))
	if err != nil {
		helps.LogWithRequestID(ctx).Debugf("%s: failed to rewrite sanitized reasoning input: %v", provider, err)
		return body
	}
	return updated
}

func dropOpenAIResponsesReasoningEncryptedContent(ctx context.Context, provider string, body []byte, reason string) ([]byte, bool) {
	if !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return body, false
	}
	input := codexGJSONGetImmutableBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai responses upstream"
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "upstream rejected encrypted content"
	}
	stripOrphanReasoningIDs := !codexGJSONGetImmutableBytes(body, "store").Bool()

	items := input.Array()
	rawItems := make([][]byte, 0, len(items))
	changed := false
	for index, item := range items {
		rawItem := []byte(item.Raw)
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" || !item.Get("encrypted_content").Exists() {
			rawItems = append(rawItems, rawItem)
			continue
		}
		next, err := sjson.DeleteBytes(rawItem, "encrypted_content")
		if err != nil {
			helps.LogWithRequestID(ctx).Debugf("%s: failed to drop reasoning encrypted_content at input[%d]: %v", provider, index, err)
			rawItems = append(rawItems, rawItem)
			continue
		}
		if stripOrphanReasoningIDs && item.Get("id").Exists() {
			if nextWithoutID, errID := sjson.DeleteBytes(next, "id"); errID != nil {
				helps.LogWithRequestID(ctx).Debugf("%s: failed to drop reasoning id after encrypted_content removal at input[%d]: %v", provider, index, errID)
			} else {
				next = nextWithoutID
			}
		}
		rawItems = append(rawItems, next)
		changed = true

		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			itemID = fmt.Sprintf("input[%d]", index)
		}
		helps.LogWithRequestID(ctx).Debugf("%s: dropped reasoning encrypted_content at input[%d] item_id=%q reason=%s", provider, index, itemID, reason)
	}
	if !changed {
		return body, false
	}
	updated, err := sjson.SetRawBytes(body, "input", codexRawJSONArray(rawItems))
	if err != nil {
		helps.LogWithRequestID(ctx).Debugf("%s: failed to rewrite reasoning input after encrypted_content drop: %v", provider, err)
		return body, false
	}
	return updated, true
}
