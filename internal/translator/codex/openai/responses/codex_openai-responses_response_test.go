package responses

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var codexResponsesStreamBenchmarkSink [][]byte

func TestConvertCodexResponseToOpenAIResponsesReusesCanonicalSSEChunk(t *testing.T) {
	t.Parallel()

	input := []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`)
	got := ConvertCodexResponseToOpenAIResponses(context.Background(), "", nil, nil, input, nil)
	if len(got) != 1 || !bytes.Equal(got[0], input) {
		t.Fatalf("converted stream chunk = %q, want %q", got, input)
	}
	if len(got[0]) == 0 || &got[0][0] != &input[0] {
		t.Fatal("canonical SSE chunk was copied")
	}
}

func TestConvertCodexResponseToOpenAIResponsesNormalizesSSEDataPrefix(t *testing.T) {
	t.Parallel()

	input := []byte("data:\t  {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}  \r\n")
	want := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}")

	got := ConvertCodexResponseToOpenAIResponses(context.Background(), "", nil, nil, input, nil)
	if len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("converted stream chunk = %q, want %q", got, want)
	}
}

func TestConvertCodexResponseToOpenAIResponses_UsesClientModelAcrossLifecycle(t *testing.T) {
	t.Parallel()

	originalRequest := []byte(`{"model":"tenant/codex-alias"}`)
	translatedRequest := []byte(`{"request":{"model":"codex-internal-model"}}`)
	inputs := map[string][]byte{
		"response.created":     []byte(`data: {"type":"response.created","response":{"id":"resp_model","model":"upstream-model","output":[]}}`),
		"response.in_progress": []byte(`data: {"type":"response.in_progress","response":{"id":"resp_model","model":"upstream-model","output":[{"type":"message"}]}}`),
		"response.completed":   []byte(`data: {"type":"response.completed","response":{"id":"resp_model","model":"upstream-model","output":[]}}`),
	}

	for event, input := range inputs {
		outputs := ConvertCodexResponseToOpenAIResponses(context.Background(), "fallback-model", originalRequest, translatedRequest, input, nil)
		if len(outputs) != 1 {
			t.Fatalf("%s outputs = %d, want 1", event, len(outputs))
		}
		payload := strings.TrimSpace(strings.TrimPrefix(string(outputs[0]), "data:"))
		if got := gjson.Get(payload, "response.model").String(); got != "tenant/codex-alias" {
			t.Fatalf("%s response.model = %q, want tenant/codex-alias; payload=%s", event, got, payload)
		}
		if event == "response.in_progress" {
			output := gjson.Get(payload, "response.output")
			if !output.IsArray() || len(output.Array()) != 0 {
				t.Fatalf("response.in_progress output = %s, want []", output.Raw)
			}
		}
	}
}

func BenchmarkConvertCodexResponseToOpenAIResponsesCanonicalSSE(b *testing.B) {
	input := []byte(`data: {"type":"response.output_text.delta","sequence_number":42,"delta":"hello"}`)

	b.ReportAllocs()
	for b.Loop() {
		codexResponsesStreamBenchmarkSink = ConvertCodexResponseToOpenAIResponses(context.Background(), "", nil, nil, input, nil)
	}
}
