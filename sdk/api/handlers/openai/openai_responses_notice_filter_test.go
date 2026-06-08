package openai

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesNoticeFilterDropsUsageWarnings(t *testing.T) {
	for _, message := range []string{
		"Heads up, you have less than 5% of your weekly limit left. Run /status for a breakdown",
		"Heads up, you have less than 10% of your weekly limit left. Run /status for a breakdown",
		"Heads up, you have less than 25% of your weekly limit left. Run /status for a breakdown",
		"Heads up, you have less than 25% of your 5h limit left. Run /status for a breakdown.",
		"Heads Up, you have Less Than 25% of your Weekly Limit Left. Run /Status for a breakdown.",
	} {
		filter := newResponsesNoticeFilter()

		first := filter.FilterPayload([]byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"` + message + `"}`))
		if len(first) != 0 {
			t.Fatalf("first payload should be dropped for %q", message)
		}

		second := filter.FilterPayload([]byte(`{"type":"response.output_text.done","item_id":"msg-1","text":"` + message + `"}`))
		if len(second) != 0 {
			t.Fatalf("suppressed payload should be dropped for %q", message)
		}
	}
}

func TestResponsesNoticeFilterDropsSplitUsageWarning(t *testing.T) {
	filter := newResponsesNoticeFilter()
	parts := [][]byte{
		[]byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"Heads up, you have "}`),
		[]byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"less than 10% of your "}`),
		[]byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"5h limit left. Run /status for a breakdown."}`),
	}

	for _, part := range parts {
		if got := filter.FilterPayloads(part); len(got) != 0 {
			t.Fatalf("split warning payload should be held or dropped, got %q", got)
		}
	}

	normal := []byte(`{"type":"response.output_text.delta","item_id":"msg-2","delta":"real output"}`)
	got := filter.FilterPayloads(normal)
	if len(got) != 1 || !bytes.Equal(got[0], normal) {
		t.Fatalf("normal payload = %q, want %q", got, normal)
	}
}

func TestResponsesNoticeFilterFlushesHeldNonWarningText(t *testing.T) {
	filter := newResponsesNoticeFilter()
	first := []byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"Heads up, you have "}`)
	second := []byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"a review waiting."}`)

	if got := filter.FilterPayloads(first); len(got) != 0 {
		t.Fatalf("prefix payload should be held, got %q", got)
	}
	got := filter.FilterPayloads(second)
	if len(got) != 2 || !bytes.Equal(got[0], first) || !bytes.Equal(got[1], second) {
		t.Fatalf("held non-warning payloads = %q", got)
	}
}

func TestResponsesNoticeFilterSSEFrameFlushesHeldNonWarningText(t *testing.T) {
	filter := newResponsesNoticeFilter()

	first := filter.FilterSSEFrame([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-1\",\"delta\":\"Heads up, you have \"}\n\n"))
	if len(first) != 0 {
		t.Fatalf("prefix frame should be held, got %q", first)
	}
	second := filter.FilterSSEFrame([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-1\",\"delta\":\"a review waiting.\"}\n\n"))
	if !bytes.Contains(second, []byte("Heads up, you have ")) || !bytes.Contains(second, []byte("a review waiting.")) {
		t.Fatalf("held non-warning SSE payloads were not flushed: %q", second)
	}
}

func BenchmarkResponsesNoticeFilterPayloadFastPath(b *testing.B) {
	payload := []byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"real output"}`)
	filter := newResponsesNoticeFilter()
	var buf [4][]byte

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := filter.FilterPayloadsInto(payload, buf[:0])
		if len(out) != 1 {
			b.Fatal("unexpected filtered payload count")
		}
	}
}

func BenchmarkResponsesNoticeFilterPayloadSplitWarning(b *testing.B) {
	parts := [][]byte{
		[]byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"Heads up, you have "}`),
		[]byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"less than 10% of your "}`),
		[]byte(`{"type":"response.output_text.delta","item_id":"msg-1","delta":"5h limit left. Run /status for a breakdown."}`),
	}
	var buf [4][]byte

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		filter := newResponsesNoticeFilter()
		for _, part := range parts {
			if out := filter.FilterPayloadsInto(part, buf[:0]); len(out) != 0 {
				b.Fatal("unexpected filtered payload")
			}
		}
	}
}

func TestResponsesNoticeFilterSanitizesCompletedOutput(t *testing.T) {
	filter := newResponsesNoticeFilter()

	payload := filter.FilterPayload([]byte(`{
		"type":"response.completed",
		"response":{
			"output":[
				{"id":"msg-1","type":"message","content":[{"type":"output_text","text":"Heads up, you have less than 25% of your 5h limit left. Run /status for a breakdown."}]},
				{"id":"msg-2","type":"message","content":[{"type":"output_text","text":"real output"}]}
			]
		}
	}`))

	output := gjson.GetBytes(payload, "response.output").Array()
	if len(output) != 1 {
		t.Fatalf("response output len = %d, want 1", len(output))
	}
	if output[0].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected remaining output id: %s", output[0].Get("id").String())
	}
}

func TestResponsesNoticeFilterSanitizesResponseObjectOutput(t *testing.T) {
	filter := newResponsesNoticeFilter()

	payload := filter.FilterResponseObject([]byte(`{
		"id":"resp-1",
		"output":[
			{"id":"msg-1","type":"message","content":[{"type":"output_text","text":"Heads up, you have less than 25% of your 5h limit left. Run /status for a breakdown."}]},
			{"id":"msg-2","type":"message","content":[{"type":"output_text","text":"real output"}]}
		]
	}`))

	output := gjson.GetBytes(payload, "output").Array()
	if len(output) != 1 {
		t.Fatalf("response object output len = %d, want 1", len(output))
	}
	if output[0].Get("id").String() != "msg-2" {
		t.Fatalf("unexpected remaining response object output id: %s", output[0].Get("id").String())
	}
}

func TestResponsesSSEFramerDropsUsageWarningFrame(t *testing.T) {
	var out bytes.Buffer
	framer := &responsesSSEFramer{noticeFilter: newResponsesNoticeFilter()}

	framer.WriteChunk(&out, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-1\",\"delta\":\"Heads up, you have less than 25% of your 5h limit left. Run /status for a breakdown.\"}\n\n"))
	framer.WriteChunk(&out, []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"id\":\"msg-2\",\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"real output\"}]}]}}\n\n"))

	if bytes.Contains(out.Bytes(), []byte("5h limit left")) {
		t.Fatalf("usage warning should be filtered")
	}
	if !bytes.Contains(out.Bytes(), []byte("real output")) {
		t.Fatalf("normal payload should remain")
	}
}

func TestResponsesNoticeFilterSSEFrameFormatting(t *testing.T) {
	warning := `{"type":"response.output_text.delta","item_id":"msg-warning","delta":"Heads up, you have less than 25% of your weekly limit left. Run /status for a breakdown."}`
	tests := []struct {
		name  string
		frame string
		want  string
	}{
		{
			name:  "canonical frame",
			frame: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"weekly report\"}\n\n",
			want:  "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"weekly report\"}\n\n",
		},
		{
			name:  "normalizes CRLF",
			frame: "event: done\r\ndata: [DONE]\r\n\r\n",
			want:  "event: done\ndata: [DONE]\n\n",
		},
		{
			name:  "normalizes data line",
			frame: "  data:   {\"type\":\"response.output_text.delta\",\"delta\":\"weekly report\"}  \n\n",
			want:  "data: {\"type\":\"response.output_text.delta\",\"delta\":\"weekly report\"}\n\n",
		},
		{
			name:  "drops warning data among retained data",
			frame: "event: output\ndata: " + warning + "\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-real\",\"delta\":\"weekly report\"}\n\n",
			want:  "event: output\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-real\",\"delta\":\"weekly report\"}\n\n",
		},
		{
			name:  "drops frame without data",
			frame: "event: output\nretry: 1000\n\n",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := newResponsesNoticeFilter()
			frame := []byte(tt.frame)
			got := filter.FilterSSEFrame(frame)
			if string(got) != tt.want {
				t.Fatalf("FilterSSEFrame() = %q, want %q", got, tt.want)
			}
			if tt.name == "canonical frame" && len(got) > 0 && &got[0] != &frame[0] {
				t.Fatal("canonical frame should reuse the input buffer")
			}
		})
	}
}

func BenchmarkResponsesUsageWarningText(b *testing.B) {
	text := "Heads Up, you have Less Than 25% of your Weekly Limit Left. Run /Status for a breakdown."
	for b.Loop() {
		if !responsesUsageWarningText(text) {
			b.Fatal("expected usage warning")
		}
	}
}

var responsesNoticeFilterFrameBenchmarkSink []byte

func BenchmarkResponsesNoticeFilterSSEFrame(b *testing.B) {
	b.Run("canonical", func(b *testing.B) {
		filter := newResponsesNoticeFilter()
		frame := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"weekly report\"}\n\n")
		b.ReportAllocs()
		for b.Loop() {
			responsesNoticeFilterFrameBenchmarkSink = filter.FilterSSEFrame(frame)
		}
	})

	b.Run("normalize_crlf", func(b *testing.B) {
		filter := newResponsesNoticeFilter()
		frame := []byte("event: done\r\ndata: [DONE]\r\n\r\n")
		b.ReportAllocs()
		for b.Loop() {
			responsesNoticeFilterFrameBenchmarkSink = filter.FilterSSEFrame(frame)
		}
	})
}

func BenchmarkResponsesNoticeMayNeedFiltering(b *testing.B) {
	chunk := []byte(`{"type":"response.output_text.delta","delta":"Heads Up, you have Less Than 25% of your Weekly Limit Left. Run \/Status for a breakdown."}`)
	for b.Loop() {
		if !responsesNoticeMayNeedFiltering(chunk) {
			b.Fatal("expected notice marker")
		}
	}
}
