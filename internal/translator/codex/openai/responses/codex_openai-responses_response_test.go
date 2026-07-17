package responses

import (
	"bytes"
	"context"
	"testing"
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

func BenchmarkConvertCodexResponseToOpenAIResponsesCanonicalSSE(b *testing.B) {
	input := []byte(`data: {"type":"response.output_text.delta","sequence_number":42,"delta":"hello"}`)

	b.ReportAllocs()
	for b.Loop() {
		codexResponsesStreamBenchmarkSink = ConvertCodexResponseToOpenAIResponses(context.Background(), "", nil, nil, input, nil)
	}
}
