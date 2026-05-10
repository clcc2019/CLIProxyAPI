// Package executor — Kiro token usage estimation.
//
// Kiro's upstream (Amazon Q / CodeWhisperer GenerateAssistantResponse) frequently
// omits `messageMetadataEvent` / `tokenUsage` entirely in the event stream it
// returns. That leaves the UsageReporter with `InputTokens=0` and
// `OutputTokens=0`, so aggregated statistics look empty for Kiro even when the
// user is clearly sending and receiving content.
//
// This file provides a local estimator that fills in missing fields. Kiro
// speaks a Claude-shaped protocol on the client side, so we tokenize with
// cl100k_base — the same heuristic tiktoken uses for Anthropic-family prompts.
// Numbers are approximations, not billing-accurate, but they let the
// dashboards, per-auth usage aggregates, and detail logs show non-zero values
// that correctly track request size.
//
// The estimator walks the Kiro request body (conversationState.history +
// currentMessage.userInputMessage + tool specifications) for input tokens, and
// walks the accumulated assistant text plus tool_use JSON for output tokens.
package executor

import (
	"encoding/json"
	"strings"
	"sync"

	kiroclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

// kiroTokenizerOnce lazily initializes a cl100k_base codec and reuses it for
// every Kiro request. The codec is safe for concurrent use.
var (
	kiroTokenizerOnce sync.Once
	kiroTokenizer     tokenizer.Codec
)

// kiroEncoder returns the shared cl100k_base codec. Returns nil if the codec
// failed to initialize; callers must handle a nil return gracefully.
func kiroEncoder() tokenizer.Codec {
	kiroTokenizerOnce.Do(func() {
		enc, err := tokenizer.Get(tokenizer.Cl100kBase)
		if err == nil {
			kiroTokenizer = enc
		}
	})
	return kiroTokenizer
}

// kiroCountText returns the tokenized length of s using cl100k_base. Falls
// back to a 4-chars-per-token approximation when the codec is unavailable so
// downstream statistics always see a non-zero value for non-empty content.
func kiroCountText(s string) int64 {
	if s == "" {
		return 0
	}
	enc := kiroEncoder()
	if enc == nil {
		// Defensive fallback; cl100k_base ships with tiktoken-go so this
		// path is extremely unlikely, but we'd rather report a rough
		// estimate than zero.
		return int64((len(s) + 3) / 4)
	}
	count, err := enc.Count(s)
	if err != nil {
		return int64((len(s) + 3) / 4)
	}
	return int64(count)
}

// kiroEstimateInputTokensFromKiroRequest estimates prompt tokens from the Kiro
// request body (the JSON delivered to /generateAssistantResponse). The body
// shape is defined by internal/translator/kiro/claude/kiro_claude_request.go;
// see KiroConversationState / KiroUserInputMessage / KiroToolSpecification.
//
// Returns 0 when the payload is empty, not JSON, or contains no text.
func kiroEstimateInputTokensFromKiroRequest(kiroReq []byte) int64 {
	if len(kiroReq) == 0 {
		return 0
	}
	root := gjson.ParseBytes(kiroReq)
	var segments []string

	// currentMessage.userInputMessage.content (+ tools/toolResults)
	collectKiroUserInputMessage(root.Get("conversationState.currentMessage.userInputMessage"), &segments)

	// history[].userInputMessage.content / assistantResponseMessage.content
	history := root.Get("conversationState.history")
	if history.IsArray() {
		history.ForEach(func(_, entry gjson.Result) bool {
			collectKiroUserInputMessage(entry.Get("userInputMessage"), &segments)
			collectKiroAssistantResponseMessage(entry.Get("assistantResponseMessage"), &segments)
			return true
		})
	}

	joined := strings.TrimSpace(strings.Join(segments, "\n"))
	if joined == "" {
		return 0
	}
	return kiroCountText(joined)
}

func collectKiroUserInputMessage(msg gjson.Result, segments *[]string) {
	if !msg.Exists() || segments == nil {
		return
	}
	appendIfNonEmpty(segments, msg.Get("content").String())
	ctx := msg.Get("userInputMessageContext")
	if !ctx.Exists() {
		return
	}
	tools := ctx.Get("tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			spec := tool.Get("toolSpecification")
			if !spec.Exists() {
				return true
			}
			appendIfNonEmpty(segments, spec.Get("name").String())
			appendIfNonEmpty(segments, spec.Get("description").String())
			if schema := spec.Get("inputSchema.json"); schema.Exists() {
				appendIfNonEmpty(segments, schema.Raw)
			}
			return true
		})
	}
	results := ctx.Get("toolResults")
	if results.IsArray() {
		results.ForEach(func(_, r gjson.Result) bool {
			r.Get("content").ForEach(func(_, c gjson.Result) bool {
				appendIfNonEmpty(segments, c.Get("text").String())
				return true
			})
			return true
		})
	}
}

func collectKiroAssistantResponseMessage(msg gjson.Result, segments *[]string) {
	if !msg.Exists() || segments == nil {
		return
	}
	appendIfNonEmpty(segments, msg.Get("content").String())
	toolUses := msg.Get("toolUses")
	if toolUses.IsArray() {
		toolUses.ForEach(func(_, tu gjson.Result) bool {
			appendIfNonEmpty(segments, tu.Get("name").String())
			if input := tu.Get("input"); input.Exists() {
				appendIfNonEmpty(segments, input.Raw)
			}
			return true
		})
	}
}

func appendIfNonEmpty(segments *[]string, value string) {
	if segments == nil {
		return
	}
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		*segments = append(*segments, trimmed)
	}
}

// kiroMarshalToolInput serializes a tool_use input map to JSON for token
// estimation purposes. Returns "" if marshalling fails.
func kiroMarshalToolInput(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(raw)
}

// kiroEstimateOutputTokens estimates completion tokens from accumulated
// assistant text plus any tool_use inputs returned by the model. Used when
// the upstream event stream did not include outputTokens / output_tokens.
func kiroEstimateOutputTokens(text string, toolUses []kiroclaude.KiroToolUse) int64 {
	var total int64
	total += kiroCountText(text)
	for _, tu := range toolUses {
		if tu.Name != "" {
			total += kiroCountText(tu.Name)
		}
		if raw := kiroMarshalToolInput(tu.Input); raw != "" {
			total += kiroCountText(raw)
		}
	}
	return total
}

// fillKiroUsageEstimates populates InputTokens / OutputTokens / TotalTokens
// from local estimates when the upstream-reported detail left them at zero.
// Accumulated content and tool uses are used for the output estimate.
// Returns a flag pair indicating which fields were filled by estimation so
// the caller can log or surface the difference.
type kiroUsageEstimateReport struct {
	FilledInput  bool
	FilledOutput bool
}

func fillKiroUsageEstimates(detail *usage.Detail, kiroReq []byte, accumulatedText string, toolUses []kiroclaude.KiroToolUse) kiroUsageEstimateReport {
	report := kiroUsageEstimateReport{}
	if detail == nil {
		return report
	}
	if detail.InputTokens == 0 {
		if est := kiroEstimateInputTokensFromKiroRequest(kiroReq); est > 0 {
			detail.InputTokens = est
			report.FilledInput = true
		}
	}
	if detail.OutputTokens == 0 {
		if est := kiroEstimateOutputTokens(accumulatedText, toolUses); est > 0 {
			detail.OutputTokens = est
			report.FilledOutput = true
		}
	}
	componentTotal := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	if componentTotal > 0 && detail.TotalTokens < componentTotal {
		detail.TotalTokens = componentTotal
	}
	return report
}

// kiroUsageEstimateLogKV returns a structured logrus Fields payload describing
// which usage buckets were filled by the local estimator vs. reported by the
// Kiro upstream. Kept small so debug logs remain readable at high RPS.
func kiroUsageEstimateLogKV(detail usage.Detail, report kiroUsageEstimateReport) map[string]any {
	return map[string]any{
		"input_tokens":     detail.InputTokens,
		"output_tokens":    detail.OutputTokens,
		"total_tokens":     detail.TotalTokens,
		"input_estimated":  report.FilledInput,
		"output_estimated": report.FilledOutput,
	}
}
