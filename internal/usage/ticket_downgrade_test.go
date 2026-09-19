package usage

import (
	"context"
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestModelDowngradeRequestDetails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		request  string
		response string
		want     bool
	}{
		{"different model", "codex", "gpt-6-astra", "gpt-5.6-luna", true},
		{"different non-codex model", "openai", "gpt-6-astra", "gpt-5.6-luna", true},
		{"same model", "codex", "gpt-6-astra", "gpt-6-astra", false},
		{"same model different case", "codex", "gpt-6-astra", "GPT-6-ASTRA", false},
		{"unknown response model", "codex", "gpt-6-astra", "", false},
		{"request model empty", "codex", "", "gpt-5.6-luna", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats := NewRequestStatistics()
			stats.Record(context.Background(), coreusage.Record{
				Provider: tc.provider, Model: tc.request, ResponseModel: tc.response,
				APIKey: "test",
			})
			modelKey := tc.request
			if modelKey == "" {
				modelKey = "unknown"
			}
			got := stats.Snapshot().APIs["test"].Models[modelKey].Details[0]
			if got.ModelDowngraded != tc.want || got.ResponseModel != tc.response {
				t.Fatalf("unexpected detail: %+v", got)
			}
		})
	}
}

func TestRequestDetailsPreferTranslatedReasoningEffort(t *testing.T) {
	tests := []struct {
		name       string
		translated string
		requested  string
		want       string
	}{
		{
			name:       "mapped effort wins",
			translated: "max",
			requested:  "high",
			want:       "max",
		},
		{
			name:      "requested effort is fallback",
			requested: "high",
			want:      "high",
		},
		{
			name:       "whitespace is normalized",
			translated: "  xhigh  ",
			requested:  " medium ",
			want:       "xhigh",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stats := NewRequestStatistics()
			stats.Record(context.Background(), coreusage.Record{
				APIKey:               "test",
				Model:                "gpt-5.6-terra",
				ReasoningEffort:      tc.translated,
				ModelReasoningEffort: tc.requested,
			})

			detail := stats.Snapshot().APIs["test"].Models["gpt-5.6-terra"].Details[0]
			if detail.ModelReasoningEffort != tc.want {
				t.Fatalf("model_reasoning_effort = %q, want %q", detail.ModelReasoningEffort, tc.want)
			}
		})
	}
}

func TestRequestDetailsKeepRequestedAndUpstreamReasoningEffortSeparate(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:                   "test",
		RequestedModel:           "public-codex",
		Model:                    "gpt-5.6-sol",
		ReasoningEffort:          "xhigh",
		RequestedReasoningEffort: "high",
		ModelMappingChain:        "public-codex→gpt-5.6-sol",
	})
	detail := stats.Snapshot().APIs["test"].Models["gpt-5.6-sol"].Details[0]
	if detail.RequestedReasoningEffort != "high" || detail.UpstreamReasoningEffort != "xhigh" {
		t.Fatalf("reasoning fields = (%q, %q)", detail.RequestedReasoningEffort, detail.UpstreamReasoningEffort)
	}
	if detail.ModelMappingChain != "public-codex→gpt-5.6-sol" {
		t.Fatalf("mapping chain = %q", detail.ModelMappingChain)
	}
}

func TestRequestDetailsResponseModelMismatchIsTriState(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		response string
		wantNil  bool
		want     bool
	}{
		{name: "undeclared", model: "gpt-5.6-sol", wantNil: true},
		{name: "matching", model: "gpt-5.6-sol", response: "gpt-5.6-sol", want: false},
		{name: "mismatch", model: "gpt-5.6-sol", response: "gpt-5.6-luna", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats := NewRequestStatistics()
			stats.Record(context.Background(), coreusage.Record{APIKey: "test", Model: tc.model, ResponseModel: tc.response})
			detail := stats.Snapshot().APIs["test"].Models[tc.model].Details[0]
			if (detail.ResponseModelMismatch == nil) != tc.wantNil {
				t.Fatalf("mismatch nil = %v, want %v", detail.ResponseModelMismatch == nil, tc.wantNil)
			}
			if detail.ResponseModelMismatch != nil && *detail.ResponseModelMismatch != tc.want {
				t.Fatalf("mismatch = %v, want %v", *detail.ResponseModelMismatch, tc.want)
			}
		})
	}
}
