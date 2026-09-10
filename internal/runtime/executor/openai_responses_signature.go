package executor

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIResponsesFunctionOutputEncryptedContentPlaceholder = "[encrypted tool output omitted]"

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
	if !openAIResponsesBodyMayContainEncryptedContent(body) {
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

func dropOpenAIResponsesFunctionOutputEncryptedContent(ctx context.Context, provider string, body []byte, reason string) ([]byte, bool) {
	if !openAIResponsesBodyMayContainEncryptedContent(body) {
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
		reason = "upstream rejected encrypted function output content"
	}

	items := input.Array()
	rawItems := make([][]byte, 0, len(items))
	changed := false
	for index, item := range items {
		rawItem := []byte(item.Raw)
		next, itemChanged := dropFunctionOutputEncryptedContentFromItem(ctx, provider, index, rawItem, item, reason)
		rawItems = append(rawItems, next)
		if itemChanged {
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	updated, err := sjson.SetRawBytes(body, "input", codexRawJSONArray(rawItems))
	if err != nil {
		helps.LogWithRequestID(ctx).Debugf("%s: failed to rewrite input after function output encrypted_content drop: %v", provider, err)
		return body, false
	}
	return updated, true
}

func dropFunctionOutputEncryptedContentFromItem(ctx context.Context, provider string, index int, rawItem []byte, item gjson.Result, reason string) ([]byte, bool) {
	itemType := strings.TrimSpace(item.Get("type").String())
	if !isToolOutputInputItemType(itemType) {
		return rawItem, false
	}
	if !functionOutputItemMayContainEncryptedContent(item) {
		return rawItem, false
	}

	next := rawItem
	changed := false
	if output := item.Get("output"); output.Exists() {
		if strippedOutput, outputChanged := stripToolOutputEncryptedContentValue(output); outputChanged {
			var err error
			next, err = sjson.SetRawBytes(next, "output", strippedOutput)
			if err != nil {
				helps.LogWithRequestID(ctx).Debugf("%s: failed to drop function output encrypted_content at input[%d]: %v", provider, index, err)
				return rawItem, false
			}
			changed = true
		}
	}
	if item.Get("encrypted_content").Exists() {
		var err error
		next, err = sjson.DeleteBytes(next, "encrypted_content")
		if err != nil {
			helps.LogWithRequestID(ctx).Debugf("%s: failed to drop top-level tool output encrypted_content at input[%d]: %v", provider, index, err)
			return rawItem, changed
		}
		changed = true
	}
	if !changed {
		return rawItem, false
	}

	callID := strings.TrimSpace(item.Get("call_id").String())
	if callID == "" {
		callID = fmt.Sprintf("input[%d]", index)
	}
	helps.LogWithRequestID(ctx).Debugf("%s: dropped function output encrypted_content at input[%d] call_id=%q reason=%s", provider, index, callID, reason)
	return next, true
}

func isToolOutputInputItemType(itemType string) bool {
	switch itemType {
	case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
		return true
	default:
		return false
	}
}

func stripToolOutputEncryptedContentValue(output gjson.Result) ([]byte, bool) {
	switch {
	case output.IsArray():
		return stripToolOutputEncryptedContentArray(output)
	case output.IsObject():
		if content := output.Get("content"); content.IsArray() && toolOutputArrayHasEncryptedContent(content) {
			strippedContent, changed := stripToolOutputEncryptedContentArray(content)
			if !changed {
				return []byte(output.Raw), false
			}
			updated, err := sjson.SetRawBytes([]byte(output.Raw), "content", strippedContent)
			if err != nil {
				return []byte(output.Raw), false
			}
			return updated, true
		}
		if toolOutputPartHasEncryptedContent(output) {
			return []byte(`{"type":"input_text","text":` + strconv.Quote(openAIResponsesFunctionOutputEncryptedContentPlaceholder) + `}`), true
		}
	case output.Type == gjson.String:
		// Tool output is often serialized as a JSON string (MCP/custom tools).
		// Parse that string before deciding it is encrypted so readable content
		// survives and only the provider-bound encrypted part is replaced. Plain
		// prose that merely mentions the field name must remain untouched.
		raw := strings.TrimSpace(output.String())
		if !gjson.Valid(raw) {
			return []byte(output.Raw), false
		}
		parsed := gjson.Parse(raw)
		stripped, changed := stripToolOutputEncryptedContentValue(parsed)
		if !changed {
			return []byte(output.Raw), false
		}
		return []byte(strconv.Quote(string(stripped))), true
	}
	return []byte(output.Raw), false
}

func stripToolOutputEncryptedContentArray(output gjson.Result) ([]byte, bool) {
	outputItems := output.Array()
	rawOutputItems := make([][]byte, 0, len(outputItems))
	changed := false
	for _, outputItem := range outputItems {
		if toolOutputPartHasEncryptedContent(outputItem) {
			changed = true
			rawOutputItems = append(rawOutputItems, functionOutputEncryptedContentPlaceholderJSON())
			continue
		}
		rawOutputItems = append(rawOutputItems, []byte(outputItem.Raw))
	}
	if !changed {
		return []byte(output.Raw), false
	}
	return codexRawJSONArray(rawOutputItems), true
}

func toolOutputArrayHasEncryptedContent(output gjson.Result) bool {
	if strings.Contains(output.Raw, `"encrypted_content"`) || strings.Contains(output.Raw, "encryptedContent") {
		return true
	}
	found := false
	output.ForEach(func(_, item gjson.Result) bool {
		found = toolOutputPartHasEncryptedContent(item)
		return !found
	})
	return found
}

func toolOutputPartHasEncryptedContent(item gjson.Result) bool {
	itemType := strings.TrimSpace(item.Get("type").String())
	if itemType == "encrypted_content" {
		return true
	}
	if encrypted := item.Get("encrypted_content"); encrypted.Exists() && encrypted.Type == gjson.String && strings.TrimSpace(encrypted.String()) != "" {
		return true
	}
	return codexToolOutputMetaEncryptedContent(item)
}

func codexToolOutputMetaEncryptedContent(item gjson.Result) bool {
	meta := item.Get("_meta")
	if !meta.Exists() {
		return false
	}
	return meta.Get("codex/encryptedContent").Bool() || meta.Get("encryptedContent").Bool()
}

func functionOutputEncryptedContentPlaceholderJSON() []byte {
	return []byte(`{"type":"input_text","text":` + strconv.Quote(openAIResponsesFunctionOutputEncryptedContentPlaceholder) + `}`)
}

func openAIResponsesBodyMayContainEncryptedContent(body []byte) bool {
	return bytes.Contains(body, []byte("encrypted_content")) ||
		bytes.Contains(body, []byte("encryptedContent"))
}

func functionOutputItemMayContainEncryptedContent(item gjson.Result) bool {
	return strings.Contains(item.Raw, "encrypted_content") ||
		strings.Contains(item.Raw, "encryptedContent")
}
