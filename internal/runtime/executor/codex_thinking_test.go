package executor

import (
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestApplyCodexThinking_UpstreamReasoningEffortOverride(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "test-codex-model",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		Metadata: map[string]any{
			cliproxyexecutor.UpstreamReasoningEffortOverrideMetadataKey: "max",
		},
	}

	body, err := applyCodexThinking(
		[]byte(`{"reasoning":{"effort":"medium"}}`),
		req,
		"openai",
		"codex",
		"codex",
	)
	if err != nil {
		t.Fatalf("applyCodexThinking() error = %v", err)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", got, body)
	}
}

func TestApplyCodexThinking_DefaultsMissingClientEffortToLow(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "test-codex-model",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}

	body, err := applyCodexThinking(
		// The OpenAI chat-completions translator currently supplies medium when
		// reasoning_effort is absent. That generated value must not be treated as
		// a client choice.
		[]byte(`{"reasoning":{"effort":"medium"}}`),
		req,
		"openai",
		"codex",
		"codex",
	)
	if err != nil {
		t.Fatalf("applyCodexThinking() error = %v", err)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want low; body=%s", got, body)
	}
}

func TestApplyCodexThinking_PreservesExplicitClientEffort(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "test-codex-model",
		Payload: []byte(`{"model":"test-codex-model","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
	}

	body, err := applyCodexThinking(
		[]byte(`{"reasoning":{"effort":"high"}}`),
		req,
		"openai",
		"codex",
		"codex",
	)
	if err != nil {
		t.Fatalf("applyCodexThinking() error = %v", err)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", got, body)
	}
}

func TestApplyCodexThinking_PreservesModelSuffixEffort(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "test-codex-model(high)",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}

	body, err := applyCodexThinking(
		[]byte(`{"reasoning":{"effort":"medium"}}`),
		req,
		"openai",
		"codex",
		"codex",
	)
	if err != nil {
		t.Fatalf("applyCodexThinking() error = %v", err)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", got, body)
	}
}
