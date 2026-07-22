package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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
	body, err := thinking.ApplyThinking(body, req.Model, fromFormat, toFormat, provider)
	if err != nil {
		return nil, err
	}

	effort := upstreamReasoningEffortOverride(req)
	if effort == "" {
		if thinking.ExtractReasoningEffort(req.Payload, fromFormat, req.Model) != "" {
			return body, nil
		}
		effort = string(thinking.LevelLow)
	}
	result, err := sjson.SetBytes(body, "reasoning.effort", effort)
	if err != nil {
		return nil, err
	}
	return result, nil
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
