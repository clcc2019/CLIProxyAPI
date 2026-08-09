package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyCodexThinking applies the normal thinking configuration, then applies a
// routing-owned effort override when one is present. The override is carried in
// request metadata so OAuth model aliases can keep the upstream model ID clean.
//
// Compatibility translators can synthesize a Codex "medium" effort when the
// client omitted its reasoning setting. Detect that omission from req.Payload,
// which is still the client-format payload, and explicitly use the safer "low"
// upstream default instead of forwarding the synthesized value.
func applyCodexThinking(body []byte, req cliproxyexecutor.Request, fromFormat, toFormat, provider string) ([]byte, error) {
	return applyCodexThinkingInternal(body, req, fromFormat, toFormat, provider, false)
}

func applyCodexThinkingWithInstructions(body []byte, req cliproxyexecutor.Request, fromFormat, toFormat, provider string) ([]byte, error) {
	return applyCodexThinkingInternal(body, req, fromFormat, toFormat, provider, true)
}

func applyCodexThinkingInternal(body []byte, req cliproxyexecutor.Request, fromFormat, toFormat, provider string, normalizeInstructions bool) ([]byte, error) {
	body, err := thinking.ApplyThinking(body, req.Model, fromFormat, toFormat, provider)
	if err != nil {
		return nil, err
	}

	effort := upstreamReasoningEffortOverride(req)
	if effort == "" {
		if thinking.ExtractReasoningEffort(req.Payload, fromFormat, req.Model) != "" {
			if normalizeInstructions {
				body = normalizeCodexInstructions(body)
			}
			return body, nil
		}
		effort = string(thinking.LevelLow)
	}
	var result []byte
	if normalizeInstructions {
		result, err = codexSetReasoningEffortWithInstructions(body, effort)
	} else {
		result, err = codexSetReasoningEffort(body, effort)
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

func codexSetReasoningEffort(body []byte, effort string) ([]byte, error) {
	return codexSetReasoningEffortInternal(body, effort, false)
}

func codexSetReasoningEffortWithInstructions(body []byte, effort string) ([]byte, error) {
	return codexSetReasoningEffortInternal(body, effort, true)
}

func codexSetReasoningEffortInternal(body []byte, effort string, normalizeInstructions bool) ([]byte, error) {
	current := codexGJSONGetImmutableBytes(body, "reasoning.effort")
	var instructions gjson.Result
	needsInstructions := false
	if normalizeInstructions {
		instructions = codexGJSONGetImmutableBytes(body, "instructions")
		needsInstructions = !instructions.Exists() || instructions.Type == gjson.Null
	}
	if current.Type == gjson.String && current.String() == effort {
		if needsInstructions {
			return normalizeCodexInstructions(body), nil
		}
		return body, nil
	}
	if current.Exists() {
		if start, end, ok := codexJSONResultRawRange(body, current); ok {
			quotedCapacity := codexJSONStringCapacity(effort)
			updated := make([]byte, 0, len(body)-(end-start)+quotedCapacity)
			updated = append(updated, body[:start]...)
			updated = codexAppendJSONString(updated, effort)
			updated = append(updated, body[end:]...)
			if needsInstructions {
				updated = normalizeCodexInstructions(updated)
			}
			return updated, nil
		}
	}
	if !codexGJSONGetImmutableBytes(body, "reasoning").Exists() {
		if needsInstructions && !instructions.Exists() {
			if updated, ok := codexAppendTopLevelReasoningEffortAndInstructions(body, effort); ok {
				return updated, nil
			}
		}
		if updated, ok := codexAppendTopLevelSingleStringObjectField(body, "reasoning", "effort", effort); ok {
			if needsInstructions {
				updated = normalizeCodexInstructions(updated)
			}
			return updated, nil
		}
	}
	updated, err := sjson.SetBytes(body, "reasoning.effort", effort)
	if err == nil && needsInstructions {
		updated = normalizeCodexInstructions(updated)
	}
	return updated, err
}

func upstreamReasoningEffortOverride(req cliproxyexecutor.Request) string {
	if len(req.Metadata) == 0 {
		return ""
	}
	raw, ok := req.Metadata[cliproxyexecutor.UpstreamReasoningEffortOverrideMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}
