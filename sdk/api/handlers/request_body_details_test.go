package handlers

import (
	"strings"
	"testing"
)

func TestParseRequestBodyDetails(t *testing.T) {
	tests := []struct {
		name      string
		rawJSON   string
		wantModel string
		wantHas   bool
		wantValue bool
	}{
		{
			name:      "stream true",
			rawJSON:   `{"model":"test-model","stream":true}`,
			wantModel: "test-model",
			wantHas:   true,
			wantValue: true,
		},
		{
			name:      "stream false",
			rawJSON:   `{"model":"test-model","stream":false}`,
			wantModel: "test-model",
			wantHas:   true,
			wantValue: false,
		},
		{
			name:      "stream missing",
			rawJSON:   `{"model":"test-model"}`,
			wantModel: "test-model",
			wantHas:   false,
			wantValue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			details := ParseRequestBodyDetails([]byte(tt.rawJSON))
			if details.Model != tt.wantModel {
				t.Fatalf("model = %q, want %q", details.Model, tt.wantModel)
			}
			if details.HasStream != tt.wantHas {
				t.Fatalf("HasStream = %v, want %v", details.HasStream, tt.wantHas)
			}
			if details.Stream != tt.wantValue {
				t.Fatalf("Stream = %v, want %v", details.Stream, tt.wantValue)
			}
		})
	}
}

func TestOpenAIChatRequestBodyDetailsUsesResponsesFormat(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "chat completions payload",
			body: `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			want: false,
		},
		{
			name: "responses input payload",
			body: `{"model":"gpt-test","input":"hi","stream":false}`,
			want: true,
		},
		{
			name: "responses instructions payload",
			body: `{"model":"gpt-test","instructions":"be brief"}`,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			details := ParseOpenAIChatRequestBodyDetails([]byte(tt.body))
			if got := details.UsesResponsesFormat(); got != tt.want {
				t.Fatalf("UsesResponsesFormat() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseOpenAIChatRequestBodyDetailsOwnsModel(t *testing.T) {
	rawJSON := []byte(`{"model":"gpt-5-codex","stream":true,"messages":[]}`)
	details := ParseOpenAIChatRequestBodyDetails(rawJSON)
	for i := range rawJSON {
		rawJSON[i] = 'x'
	}
	if details.Model != "gpt-5-codex" {
		t.Fatalf("model = %q after source reuse, want gpt-5-codex", details.Model)
	}
}

func TestRequestBodyDetailsRoutingEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		body string
		want OpenAIChatRequestBodyDetails
	}{
		{
			name: "nested fields do not affect routing",
			body: `{"metadata":{"model":"nested","stream":true,"messages":[]},"input":"hello","stream":false,"model":"actual"}`,
			want: OpenAIChatRequestBodyDetails{RequestBodyDetails: RequestBodyDetails{Model: "actual", HasStream: true}, HasInput: true},
		},
		{
			name: "first duplicate fields win including null",
			body: `{"model":null,"model":"later","stream":null,"stream":true,"messages":null,"input":"hello","instructions":"brief"}`,
			want: OpenAIChatRequestBodyDetails{RequestBodyDetails: RequestBodyDetails{HasStream: true}, HasMessages: true, HasInput: true, HasInstructions: true},
		},
		{
			name: "escaped field names and model",
			body: `{"mo\u0064el":"test\u002dmodel","str\u0065am":true,"in\u0070ut":[]}`,
			want: OpenAIChatRequestBodyDetails{RequestBodyDetails: RequestBodyDetails{Model: "test-model", HasStream: true, Stream: true}, HasInput: true},
		},
		{
			name: "stream string is not true",
			body: `{"model":"test","stream":"true","messages":[]}`,
			want: OpenAIChatRequestBodyDetails{RequestBodyDetails: RequestBodyDetails{Model: "test", HasStream: true}, HasMessages: true},
		},
		{name: "empty object", body: `{}`},
		{name: "non object", body: `[{"model":"nested","stream":true}]`},
		{name: "empty body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			if got := ParseRequestBodyDetails(body); got != tt.want.RequestBodyDetails {
				t.Fatalf("request details = %+v, want %+v", got, tt.want.RequestBodyDetails)
			}
			if got := ParseOpenAIChatRequestBodyDetails(body); got != tt.want {
				t.Fatalf("chat details = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseRequestBodyDetailsOwnsModel(t *testing.T) {
	body := []byte(`{"model":"test-model","stream":true}`)
	details := ParseRequestBodyDetails(body)
	for i := range body {
		body[i] = 'x'
	}
	if details.Model != "test-model" {
		t.Fatalf("model changed after source reuse: %q", details.Model)
	}
}

func BenchmarkRequestBodyRouting(b *testing.B) {
	history := `[{"role":"user","content":"` + strings.Repeat("long conversation ", 8192) + `"}]`
	for _, tc := range []struct {
		name string
		body string
	}{
		{"Small", `{"model":"test-model","stream":true,"messages":[]}`},
		{"HistoryFirst", `{"messages":` + history + `,"model":"test-model","stream":true}`},
		{"HistoryLast", `{"model":"test-model","stream":true,"messages":` + history + `}`},
		{"MissingStream", `{"model":"test-model","messages":` + history + `}`},
	} {
		body := []byte(tc.body)
		b.Run(tc.name+"/Basic", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if ParseRequestBodyDetails(body).Model != "test-model" {
					b.Fatal("unexpected model")
				}
			}
		})
		b.Run(tc.name+"/Chat", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if ParseOpenAIChatRequestBodyDetails(body).Model != "test-model" {
					b.Fatal("unexpected model")
				}
			}
		})
	}
}
