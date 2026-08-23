package executor

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexDeferredReasoningEffort struct {
	effort         string
	originalRaw    string
	originalExists bool
}

type codexDeferredReasoningEffortContextKey struct{}

func contextWithCodexDeferredReasoningEffort(ctx context.Context, deferred codexDeferredReasoningEffort) context.Context {
	if deferred.effort == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexDeferredReasoningEffortContextKey{}, deferred)
}

func codexDeferredReasoningEffortFromContext(ctx context.Context) codexDeferredReasoningEffort {
	if ctx == nil {
		return codexDeferredReasoningEffort{}
	}
	deferred, _ := ctx.Value(codexDeferredReasoningEffortContextKey{}).(codexDeferredReasoningEffort)
	return deferred
}

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

func applyCodexThinkingWithInstructionsDeferred(body []byte, req cliproxyexecutor.Request, fromFormat, toFormat, provider string) ([]byte, codexDeferredReasoningEffort, error) {
	body, err := thinking.ApplyThinking(body, req.Model, fromFormat, toFormat, provider)
	if err != nil {
		return nil, codexDeferredReasoningEffort{}, err
	}
	effort, apply := codexUpstreamReasoningEffort(req, fromFormat)
	if !apply {
		return normalizeCodexInstructions(body), codexDeferredReasoningEffort{}, nil
	}
	current := codexGJSONGetImmutableBytes(body, "reasoning.effort")
	return normalizeCodexInstructions(body), codexDeferredReasoningEffort{
		effort:         effort,
		originalRaw:    current.Raw,
		originalExists: current.Exists(),
	}, nil
}

func applyCodexThinkingInternal(body []byte, req cliproxyexecutor.Request, fromFormat, toFormat, provider string, normalizeInstructions bool) ([]byte, error) {
	body, err := thinking.ApplyThinking(body, req.Model, fromFormat, toFormat, provider)
	if err != nil {
		return nil, err
	}

	effort, apply := codexUpstreamReasoningEffort(req, fromFormat)
	if !apply {
		if normalizeInstructions {
			body = normalizeCodexInstructions(body)
		}
		return body, nil
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

func codexUpstreamReasoningEffort(req cliproxyexecutor.Request, fromFormat string) (string, bool) {
	if effort := upstreamReasoningEffortOverride(req); effort != "" {
		return effort, true
	}
	if thinking.ExtractReasoningEffort(req.Payload, fromFormat, req.Model) != "" {
		return "", false
	}
	return string(thinking.LevelLow), true
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
