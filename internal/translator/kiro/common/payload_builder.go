// Package common — Kiro payload builder helpers.
//
// Both translator/kiro/claude and translator/kiro/openai drive the
// BuildKiroPayload* entry points through an almost identical sequence of
// prompt transformations. Each step is independently useful to test, so we
// expose them as small helpers that operate on string prompts and primitive
// inference-config values. The packages still own their own Kiro struct
// definitions (KiroPayload / KiroConversationState / etc.) because they
// predate this extraction; keeping the types local avoids a cross-package
// refactor while still eliminating ~150 lines of duplication between the two
// builders.
package common

import (
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// KiroMaxOutputTokens caps the `max_tokens` field both builders apply to the
// inferenceConfig. Kiro currently truncates responses above this, so we
// expose the constant here instead of re-declaring it in each builder.
const KiroMaxOutputTokens = 32000

// InferenceParams groups together the three knobs (max_tokens / temperature /
// top_p) both builders read from the incoming request. hasTemperature /
// hasTopP discriminate "not supplied" from "supplied zero" so the outbound
// KiroInferenceConfig correctly omits unset fields.
type InferenceParams struct {
	MaxTokens      int64
	Temperature    float64
	TopP           float64
	HasTemperature bool
	HasTopP        bool
}

// ExtractInferenceParams reads max_tokens / temperature / top_p from a JSON
// payload. Handles the common `max_tokens=-1` "use maximum" idiom surfaced by
// some clients (Cline / Continue) by clamping to KiroMaxOutputTokens.
func ExtractInferenceParams(body []byte) InferenceParams {
	params := InferenceParams{}
	if mt := gjson.GetBytes(body, "max_tokens"); mt.Exists() {
		params.MaxTokens = mt.Int()
		if params.MaxTokens == -1 {
			params.MaxTokens = KiroMaxOutputTokens
		}
	}
	if temp := gjson.GetBytes(body, "temperature"); temp.Exists() {
		params.Temperature = temp.Float()
		params.HasTemperature = true
	}
	if tp := gjson.GetBytes(body, "top_p"); tp.Exists() {
		params.TopP = tp.Float()
		params.HasTopP = true
	}
	return params
}

// HasAnyInferenceConfig reports whether any field was supplied. Used by the
// builders to decide whether to emit an InferenceConfig block at all.
func (p InferenceParams) HasAnyInferenceConfig() bool {
	return p.MaxTokens > 0 || p.HasTemperature || p.HasTopP
}

// InjectTimestampContext prepends a "[Context: Current time is ...]" banner
// to the system prompt so the model knows the current wall-clock time. This
// matters for agentic loops that reason about deadlines or stale data.
func InjectTimestampContext(systemPrompt string, now time.Time) string {
	banner := fmt.Sprintf("[Context: Current time is %s]", now.Format("2006-01-02 15:04:05 MST"))
	if systemPrompt == "" {
		return banner
	}
	return banner + "\n\n" + systemPrompt
}

// AppendSystemHint appends `hint` to `systemPrompt` joined by a newline,
// handling the empty-prompt case. Used for agentic hints, tool_choice hints,
// and response_format hints — all of which fall back to system-prompt
// injection because Kiro's API does not expose them natively.
func AppendSystemHint(systemPrompt, hint string) string {
	if hint == "" {
		return systemPrompt
	}
	if systemPrompt == "" {
		return hint
	}
	return systemPrompt + "\n" + hint
}

// PrependThinkingHint wraps the system prompt with the Kiro <thinking_mode>
// + <max_thinking_length> control tags. Both builders emit the identical
// string; centralising it prevents the two copies from drifting. The budget
// is capped at 16000 tokens to reserve room for tool outputs — production
// data showed longer thinking windows frequently triggered truncation.
func PrependThinkingHint(systemPrompt string) string {
	const hint = `<thinking_mode>enabled</thinking_mode>
<max_thinking_length>16000</max_thinking_length>`
	if systemPrompt == "" {
		return hint
	}
	return hint + "\n\n" + systemPrompt
}

// BuildFallbackSystemPromptContent produces the content used when the request
// has no current user message. Kiro rejects empty currentMessage.content, so
// builders synthesise one from the system prompt. The SYSTEM PROMPT fencing
// mirrors the upstream convention and helps the model distinguish injected
// guidance from real user turns.
func BuildFallbackSystemPromptContent(systemPrompt string) string {
	if strings.TrimSpace(systemPrompt) == "" {
		return ""
	}
	return "--- SYSTEM PROMPT ---\n" + systemPrompt + "\n--- END SYSTEM PROMPT ---\n"
}

// NormalizeOrigin maps client-side origin tokens to the canonical values the
// Kiro API accepts. Both `CLI` (Amazon Q quota) and `AI_EDITOR` (Kiro IDE
// quota) are billed separately upstream, so correct normalisation is a
// quota-correctness issue, not a cosmetic one.
func NormalizeOrigin(origin string) string {
	switch origin {
	case "KIRO_CLI", "AMAZON_Q":
		return "CLI"
	case "KIRO_AI_EDITOR", "KIRO_IDE":
		return "AI_EDITOR"
	default:
		return origin
	}
}
