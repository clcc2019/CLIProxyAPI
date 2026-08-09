package executor

import (
	"bytes"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestCodexSetReasoningEffort(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		effort string
		want   string
	}{
		{name: "same value", body: `{"reasoning":{"effort":"low"},"input":[]}`, effort: "low", want: `{"reasoning":{"effort":"low"},"input":[]}`},
		{name: "replace string", body: `{"reasoning":{"effort":"medium"},"input":[]}`, effort: "low", want: `{"reasoning":{"effort":"low"},"input":[]}`},
		{name: "replace null", body: `{"reasoning":{"effort":null},"input":[]}`, effort: "high", want: `{"reasoning":{"effort":"high"},"input":[]}`},
		{name: "append reasoning object", body: `{"input":[]}`, effort: "low", want: `{"input":[],"reasoning":{"effort":"low"}}`},
		{name: "append effort to existing object", body: `{"reasoning":{"summary":"auto"},"input":[]}`, effort: "high", want: `{"reasoning":{"summary":"auto","effort":"high"},"input":[]}`},
		{name: "replace non object", body: `{"reasoning":null,"input":[]}`, effort: "high", want: `{"reasoning":{"effort":"high"},"input":[]}`},
		{name: "escape effort", body: `{"reasoning":{"effort":"low"},"input":[]}`, effort: "high\ncustom", want: `{"reasoning":{"effort":"high\ncustom"},"input":[]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(tt.body)
			before := append([]byte(nil), input...)
			got, err := codexSetReasoningEffort(input, tt.effort)
			if err != nil {
				t.Fatalf("codexSetReasoningEffort() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("codexSetReasoningEffort() = %s, want %s", got, tt.want)
			}
			if !bytes.Equal(input, before) {
				t.Fatalf("codexSetReasoningEffort mutated input: got %s, want %s", input, before)
			}
		})
	}
}

func BenchmarkCodexSetReasoningEffort(b *testing.B) {
	body := []byte(`{"model":"gpt-5-codex","reasoning":{"effort":"medium","summary":"auto"},"input":[{"role":"user","content":"hello"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		result, err := codexSetReasoningEffort(body, "low")
		if err != nil || len(result) == 0 {
			b.Fatalf("codexSetReasoningEffort() error = %v", err)
		}
	}
}

func TestCodexSetReasoningEffortWithInstructions(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "add both fields", body: `{"model":"gpt-5-codex","input":[]}`, want: `{"model":"gpt-5-codex","input":[],"reasoning":{"effort":"low"},"instructions":""}`},
		{name: "replace effort and add instructions", body: `{"model":"gpt-5-codex","reasoning":{"effort":"medium"},"input":[]}`, want: `{"model":"gpt-5-codex","reasoning":{"effort":"low"},"input":[],"instructions":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			before := append([]byte(nil), body...)
			got, err := codexSetReasoningEffortWithInstructions(body, "low")
			if err != nil {
				t.Fatalf("codexSetReasoningEffortWithInstructions() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("codexSetReasoningEffortWithInstructions() = %s, want %s", got, tt.want)
			}
			if !bytes.Equal(body, before) {
				t.Fatalf("codexSetReasoningEffortWithInstructions mutated input: got %s, want %s", body, before)
			}
		})
	}
}

func BenchmarkCodexSetReasoningEffortWithInstructions(b *testing.B) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"hello"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		result, err := codexSetReasoningEffortWithInstructions(body, "low")
		if err != nil || len(result) == 0 {
			b.Fatalf("codexSetReasoningEffortWithInstructions() error = %v", err)
		}
	}
}

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
