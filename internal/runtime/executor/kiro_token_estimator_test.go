package executor

import (
	"testing"

	kiroclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/kiro/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestKiroCountTextSmoke confirms the cl100k_base codec initialises and
// produces strictly positive counts for non-empty input. It's the closest
// thing to a contract test we can write without committing specific token
// IDs, which change subtly across tokenizer releases.
func TestKiroCountTextSmoke(t *testing.T) {
	if got := kiroCountText(""); got != 0 {
		t.Fatalf("empty string should produce 0 tokens, got %d", got)
	}
	if got := kiroCountText("hello world"); got <= 0 {
		t.Fatalf("non-empty string should produce >0 tokens, got %d", got)
	}
}

// TestKiroEstimateInputTokensFromKiroRequest walks a realistic Kiro request
// body shape — current message + history + tools + tool results — and
// verifies all four sources contribute to the estimate.
func TestKiroEstimateInputTokensFromKiroRequest(t *testing.T) {
	payload := []byte(`{
		"conversationState": {
			"currentMessage": {
				"userInputMessage": {
					"content": "Please refactor the auth module to support OAuth2.",
					"modelId": "claude-sonnet-4.5",
					"origin": "AI_EDITOR",
					"userInputMessageContext": {
						"tools": [
							{
								"toolSpecification": {
									"name": "shell",
									"description": "run a shell command",
									"inputSchema": {"json": {"type": "object", "properties": {"command": {"type": "string"}}}}
								}
							}
						],
						"toolResults": [
							{"content": [{"text": "ls output here"}], "status": "success", "toolUseId": "t1"}
						]
					}
				}
			},
			"history": [
				{"userInputMessage": {"content": "earlier question"}},
				{"assistantResponseMessage": {"content": "earlier answer", "toolUses": [{"name": "lookup", "input": {"query": "hello"}}]}}
			]
		}
	}`)
	tokens := kiroEstimateInputTokensFromKiroRequest(payload)
	if tokens <= 0 {
		t.Fatalf("expected positive estimate, got %d", tokens)
	}
	// Empty history entry + no current message should estimate 0.
	empty := kiroEstimateInputTokensFromKiroRequest([]byte(`{"conversationState":{"history":[]}}`))
	if empty != 0 {
		t.Fatalf("expected 0 tokens for empty payload, got %d", empty)
	}
	// Completely empty input is treated as zero — avoids spurious non-zero
	// stats if a caller invokes the estimator with a dropped payload.
	if kiroEstimateInputTokensFromKiroRequest(nil) != 0 {
		t.Fatalf("nil payload must produce 0 tokens")
	}
	if kiroEstimateInputTokensFromKiroRequest([]byte(`not json`)) != 0 {
		t.Fatalf("invalid JSON must produce 0 tokens")
	}
}

// TestKiroEstimateOutputTokensCombinesTextAndToolInput makes sure tool_use
// input JSON is counted — the previous length/4 heuristic missed this and
// under-reported requests that returned only a function call.
func TestKiroEstimateOutputTokensCombinesTextAndToolInput(t *testing.T) {
	withToolOnly := kiroEstimateOutputTokens("", []kiroclaude.KiroToolUse{{
		Name:  "shell",
		Input: map[string]any{"command": "ls -la"},
	}})
	if withToolOnly <= 0 {
		t.Fatalf("tool_use input should contribute >0 tokens, got %d", withToolOnly)
	}
	withText := kiroEstimateOutputTokens("Some assistant text.", nil)
	if withText <= 0 {
		t.Fatalf("text should contribute >0 tokens, got %d", withText)
	}
	combined := kiroEstimateOutputTokens("Some assistant text.", []kiroclaude.KiroToolUse{{
		Name:  "shell",
		Input: map[string]any{"command": "ls -la"},
	}})
	if combined <= withText || combined <= withToolOnly {
		t.Fatalf("combined estimate should exceed each component; text=%d tool=%d combined=%d",
			withText, withToolOnly, combined)
	}
}

// TestFillKiroUsageEstimatesLeavesReportedValues verifies the estimator is a
// no-op when the upstream event stream already supplied token counts.
func TestFillKiroUsageEstimatesLeavesReportedValues(t *testing.T) {
	detail := usage.Detail{InputTokens: 11, OutputTokens: 22, TotalTokens: 33}
	kiroReq := []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"hi"}}}}`)
	report := fillKiroUsageEstimates(&detail, kiroReq, "some response", nil)
	if report.FilledInput || report.FilledOutput {
		t.Fatalf("upstream-reported values must not be overwritten: %+v", report)
	}
	if detail.InputTokens != 11 || detail.OutputTokens != 22 || detail.TotalTokens != 33 {
		t.Fatalf("detail mutated unexpectedly: %+v", detail)
	}
}

// TestFillKiroUsageEstimatesFillsMissingFields is the primary regression
// guard: when Kiro's upstream returned a usage block with all zeros (the
// common case today), the estimator MUST populate non-zero counts from the
// request body and the accumulated output.
func TestFillKiroUsageEstimatesFillsMissingFields(t *testing.T) {
	detail := usage.Detail{}
	kiroReq := []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"Summarise this repository in detail."}}}}`)
	report := fillKiroUsageEstimates(&detail, kiroReq, "Here is the summary.", nil)
	if !report.FilledInput {
		t.Fatalf("expected FilledInput=true when upstream omitted input_tokens")
	}
	if !report.FilledOutput {
		t.Fatalf("expected FilledOutput=true when upstream omitted output_tokens")
	}
	if detail.InputTokens <= 0 {
		t.Fatalf("InputTokens not filled: %+v", detail)
	}
	if detail.OutputTokens <= 0 {
		t.Fatalf("OutputTokens not filled: %+v", detail)
	}
	if detail.TotalTokens != detail.InputTokens+detail.OutputTokens {
		t.Fatalf("TotalTokens = %d, want %d",
			detail.TotalTokens, detail.InputTokens+detail.OutputTokens)
	}
}

// TestFillKiroUsageEstimatesPartialUpstream covers the "partial usage"
// case: upstream reported input but not output (or vice versa). Only the
// missing half should be estimated.
func TestFillKiroUsageEstimatesPartialUpstream(t *testing.T) {
	detail := usage.Detail{InputTokens: 50}
	kiroReq := []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"hi"}}}}`)
	report := fillKiroUsageEstimates(&detail, kiroReq, "response text", nil)
	if report.FilledInput {
		t.Fatalf("input should not be re-estimated when upstream provided it")
	}
	if !report.FilledOutput {
		t.Fatalf("output should have been estimated")
	}
	if detail.InputTokens != 50 {
		t.Fatalf("upstream InputTokens overwritten: %d", detail.InputTokens)
	}
	if detail.OutputTokens <= 0 {
		t.Fatalf("OutputTokens not filled: %d", detail.OutputTokens)
	}
	if detail.TotalTokens != detail.InputTokens+detail.OutputTokens {
		t.Fatalf("TotalTokens recomputation failed: %+v", detail)
	}
}

func TestFillKiroUsageEstimatesPartialOutputOnlyRecomputesTotal(t *testing.T) {
	detail := usage.Detail{OutputTokens: 12, TotalTokens: 12}
	kiroReq := []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"Count the input side too."}}}}`)
	report := fillKiroUsageEstimates(&detail, kiroReq, "already counted output", nil)
	if !report.FilledInput {
		t.Fatalf("expected missing input to be estimated")
	}
	if report.FilledOutput {
		t.Fatalf("output should not be re-estimated when upstream provided it")
	}
	if detail.InputTokens <= 0 {
		t.Fatalf("InputTokens not filled: %+v", detail)
	}
	wantTotal := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	if detail.TotalTokens != wantTotal {
		t.Fatalf("TotalTokens = %d, want recomputed %d (detail=%+v)", detail.TotalTokens, wantTotal, detail)
	}
}
