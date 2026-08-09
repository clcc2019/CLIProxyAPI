package executor

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexBorrowedNativeTranslationCopiesBeforeCacheMissNormalization(t *testing.T) {
	format := sdktranslator.FromString("openai-response")
	body := []byte(`{"model":"gpt-5.4-borrowed-copy-test","input":[],"stream":false}`)
	original := bytes.Clone(body)

	translated, _, native := codexTranslateRequestWithOriginalBorrowed(
		nil,
		context.Background(),
		format,
		sdktranslator.FromString("codex"),
		"gpt-5.4-borrowed-copy-test",
		body,
		body,
		true,
		http.Header{"Originator": []string{"codex_cli_rs"}},
	)
	if !native || len(translated) == 0 || &translated[0] != &body[0] {
		t.Fatal("native hot path did not borrow the request payload")
	}

	_ = normalizeCodexFinalUpstreamBodyBorrowed(translated, "gpt-5.4-borrowed-copy-test", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})
	if !bytes.Equal(body, original) {
		t.Fatalf("cache-miss normalization mutated borrowed request\n want: %s\n got:  %s", original, body)
	}
}

func BenchmarkCodexTranslateNativeRequestWithOriginal(b *testing.B) {
	format := sdktranslator.FromString("openai-response")
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("native-codex-payload-", 256) + `"}]}]}`)
	headers := http.Header{"Originator": []string{"codex_cli_rs"}}

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		translated, original, native := codexTranslateRequestWithOriginal(
			nil,
			context.Background(),
			format,
			sdktranslator.FromString("codex"),
			"gpt-5.4",
			body,
			body,
			true,
			headers,
		)
		if !native || len(translated) != len(body) || len(original) != len(body) {
			b.Fatal("unexpected native translation result")
		}
	}
}

func BenchmarkCodexTranslateNativeRequestWithOriginalBorrowed(b *testing.B) {
	format := sdktranslator.FromString("openai-response")
	body := []byte(`{"model":"gpt-5.4","input":[]}`)
	headers := http.Header{"Originator": []string{"codex_cli_rs"}}

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		translated, original, native := codexTranslateRequestWithOriginalBorrowed(
			nil,
			context.Background(),
			format,
			sdktranslator.FromString("codex"),
			"gpt-5.4",
			body,
			body,
			true,
			headers,
		)
		if !native || len(translated) != len(body) || len(original) != len(body) {
			b.Fatal("unexpected native translation result")
		}
	}
}

func TestCodexTranslateRequestWithOriginalBypassesCompatibilityRewriteForNativeClient(t *testing.T) {
	format := sdktranslator.FromString("openai-response")
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[],
		"stream":true,
		"store":false,
		"context_management":[{"type":"compaction","compact_threshold":1234}],
		"future_codex_option":{"mode":"native"}
	}`)

	gotBody, gotOriginal, native := codexTranslateRequestWithOriginal(
		nil,
		context.Background(),
		format,
		sdktranslator.FromString("codex"),
		"gpt-5.4",
		body,
		body,
		true,
		http.Header{"Originator": []string{"codex_cli_rs"}},
	)
	if !native {
		t.Fatal("expected first-party Codex request to use native translation path")
	}
	if !gjson.GetBytes(gotBody, "context_management").IsArray() {
		t.Fatalf("native context_management was removed before normalization: %s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "future_codex_option.mode").String(); got != "native" {
		t.Fatalf("future native option = %q, want native; body=%s", got, gotBody)
	}
	if string(gotOriginal) != string(body) {
		t.Fatalf("native original payload changed: got=%s want=%s", gotOriginal, body)
	}

	gotBody[0] = '['
	if body[0] != '{' || gotOriginal[0] != '{' {
		t.Fatal("native translation result aliases an input buffer")
	}
}

func TestCodexTranslateRequestWithOriginalKeepsCompatibilityRewriteForGenericClient(t *testing.T) {
	format := sdktranslator.FromString("openai-response")
	body := []byte(`{"model":"gpt-5.4","input":[],"context_management":[{"type":"compaction"}]}`)

	gotBody, _, native := codexTranslateRequestWithOriginal(
		nil,
		context.Background(),
		format,
		sdktranslator.FromString("codex"),
		"gpt-5.4",
		body,
		body,
		true,
		http.Header{"User-Agent": []string{"generic-sdk/1.0"}},
	)
	if native {
		t.Fatal("generic Responses request unexpectedly used native translation path")
	}
	if gjson.GetBytes(gotBody, "context_management").Exists() {
		t.Fatalf("generic compatibility rewrite retained context_management: %s", gotBody)
	}
}

func TestCodexNativeClientRequestRequiresFirstPartyResponsesIdentity(t *testing.T) {
	responsesFormat := sdktranslator.FromString("openai-response")
	tests := []struct {
		name    string
		format  sdktranslator.Format
		headers http.Header
		body    []byte
		want    bool
	}{
		{
			name:    "cli originator",
			format:  responsesFormat,
			headers: http.Header{"Originator": []string{"codex_cli_rs"}},
			want:    true,
		},
		{
			name:    "vscode user agent",
			format:  responsesFormat,
			headers: http.Header{"User-Agent": []string{"codex_vscode/0.145.0 (darwin; arm64)"}},
			want:    true,
		},
		{
			name:   "canonical body metadata",
			format: responsesFormat,
			body:   []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-1\"}"}}`),
			want:   true,
		},
		{
			name:    "generic responses client",
			format:  responsesFormat,
			headers: http.Header{"User-Agent": []string{"generic-sdk/1.0"}},
			body:    []byte(`{"model":"gpt-5.4","input":[]}`),
			want:    false,
		},
		{
			name:    "wrong source format",
			format:  sdktranslator.FromString("openai"),
			headers: http.Header{"Originator": []string{"codex_cli_rs"}},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexNativeClientRequest(tt.format, tt.headers, tt.body); got != tt.want {
				t.Fatalf("codexNativeClientRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeCodexNativeBodyPreservesFutureFieldsButNotTransportOnlyFields(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","id":"msg_valid","role":"user","content":[]},
			{"type":"message","id":"legacy","role":"user","content":[]}
		],
		"future_codex_option":{"mode":"native"},
		"context_management":[{"type":"compaction","compact_threshold":1234}],
		"stream_options":{"future_delivery":"parallel"},
		"previous_response_id":"http-must-not-send",
		"generate":false
	}`)

	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind:          codexFinalUpstreamResponses,
		streamMode:           codexStreamFieldTrue,
		preserveNativeFields: true,
	})
	if got := gjson.GetBytes(gotBody, "future_codex_option.mode").String(); got != "native" {
		t.Fatalf("future Codex field = %q, want native; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "context_management.0.compact_threshold").Int(); got != 1234 {
		t.Fatalf("native context_management threshold = %d, want 1234; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "stream_options.future_delivery").String(); got != "parallel" {
		t.Fatalf("future stream option = %q, want parallel; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.id").String(); got != "msg_valid" {
		t.Fatalf("prefixed item ID = %q, want msg_valid; body=%s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "input.1.id").Exists() {
		t.Fatalf("legacy unprefixed item ID should be removed; body=%s", gotBody)
	}
	for _, field := range []string{"previous_response_id", "generate"} {
		if gjson.GetBytes(gotBody, field).Exists() {
			t.Fatalf("transport-only field %s should be removed from HTTP body: %s", field, gotBody)
		}
	}
}

func TestNormalizeCompatibilityBodyStillPrunesUnknownFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[],"future_codex_option":true}`)
	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
	})
	if gjson.GetBytes(gotBody, "future_codex_option").Exists() {
		t.Fatalf("compatibility request retained unknown field: %s", gotBody)
	}
}

func TestNormalizeStoredCodexBodyRemovesInvalidItemIDs(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","id":"legacy","role":"user","content":[]},{"type":"message","id":"msg_valid","role":"user","content":[]}]}`)
	gotBody := normalizeCodexFinalUpstreamBody(body, "gpt-5.4", nil, codexFinalUpstreamBodyOptions{
		requestKind: codexFinalUpstreamResponses,
		streamMode:  codexStreamFieldTrue,
		store:       true,
	})
	if gjson.GetBytes(gotBody, "input.0.id").Exists() {
		t.Fatalf("stored request retained invalid item ID: %s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.1.id").String(); got != "msg_valid" {
		t.Fatalf("stored prefixed item ID = %q, want msg_valid; body=%s", got, gotBody)
	}
}
