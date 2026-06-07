package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestApplyPayloadConfigWithRoot_DefaultsUseOriginalPresenceSnapshot(t *testing.T) {
	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Default: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-4.1", Protocol: "openai"}},
				Params: map[string]any{
					"temperature": 0.2,
					"top_p":       0.9,
				},
			}},
		},
	}

	payload := []byte(`{"model":"gpt-4.1"}`)
	out := ApplyPayloadConfigWithRoot(cfg, "gpt-4.1", "openai", "", payload, nil, "")

	if got := gjson.GetBytes(out, "temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2", got)
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.9 {
		t.Fatalf("top_p = %v, want 0.9", got)
	}
}

func TestApplyPayloadConfigWithRoot_DefaultsRespectProvidedOriginal(t *testing.T) {
	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Default: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-4.1", Protocol: "openai"}},
				Params: map[string]any{
					"temperature": 0.2,
					"top_p":       0.9,
				},
			}},
		},
	}

	payload := []byte(`{"model":"gpt-4.1"}`)
	original := []byte(`{"model":"gpt-4.1","temperature":0.5}`)
	out := ApplyPayloadConfigWithRoot(cfg, "gpt-4.1", "openai", "", payload, original, "")

	if got := gjson.GetBytes(out, "temperature").String(); got != "" {
		t.Fatalf("temperature should remain unset in translated payload, got %q", got)
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.9 {
		t.Fatalf("top_p = %v, want 0.9", got)
	}
}

func TestApplyPayloadConfigWithRoot_DefaultRawUsesPresenceSnapshot(t *testing.T) {
	cfg := &config.Config{
		Payload: config.PayloadConfig{
			DefaultRaw: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-4.1", Protocol: "openai"}},
				Params: map[string]any{
					"response_format": `{"type":"json_object"}`,
					"metadata":        `{"source":"proxy"}`,
				},
			}},
		},
	}

	payload := []byte(`{"model":"gpt-4.1"}`)
	out := ApplyPayloadConfigWithRoot(cfg, "gpt-4.1", "openai", "", payload, nil, "")

	if got := gjson.GetBytes(out, "response_format.type").String(); got != "json_object" {
		t.Fatalf("response_format.type = %q, want %q", got, "json_object")
	}
	if got := gjson.GetBytes(out, "metadata.source").String(); got != "proxy" {
		t.Fatalf("metadata.source = %q, want %q", got, "proxy")
	}
}

func TestNormalizePayloadFromProtocol(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: " OpenAI-Response ", want: "responses"},
		{input: "OPENAI-RESPONSES", want: "responses"},
		{input: " Response ", want: "responses"},
		{input: "Custom-Protocol", want: "custom-protocol"},
		{input: " ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := normalizePayloadFromProtocol(tt.input); got != tt.want {
				t.Fatalf("normalizePayloadFromProtocol(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func BenchmarkNormalizePayloadFromProtocol(b *testing.B) {
	for b.Loop() {
		if got := normalizePayloadFromProtocol(" OpenAI-Responses "); got != "responses" {
			b.Fatalf("normalizePayloadFromProtocol() = %q", got)
		}
	}
}
