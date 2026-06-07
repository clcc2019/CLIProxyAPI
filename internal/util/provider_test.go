package util

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSensitiveJSONBytes(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-test",
		"api_key":"sk-secret-value",
		"client_metadata":{"access_token":"access-secret","safe":"kept"},
		"tools":[{"name":"tool","input":{"refreshToken":"refresh-secret","value":"visible"}}]
	}`)

	redacted := RedactSensitiveJSONBytes(raw)
	var got map[string]any
	if err := json.Unmarshal(redacted, &got); err != nil {
		t.Fatalf("redacted JSON is invalid: %v; body=%s", err, redacted)
	}
	if got["api_key"] != "[REDACTED]" {
		t.Fatalf("api_key = %v, want redacted; body=%s", got["api_key"], redacted)
	}
	metadata := got["client_metadata"].(map[string]any)
	if metadata["access_token"] != "[REDACTED]" {
		t.Fatalf("access_token = %v, want redacted; body=%s", metadata["access_token"], redacted)
	}
	if metadata["safe"] != "kept" {
		t.Fatalf("safe metadata = %v, want kept", metadata["safe"])
	}
	toolInput := got["tools"].([]any)[0].(map[string]any)["input"].(map[string]any)
	if toolInput["refreshToken"] != "[REDACTED]" {
		t.Fatalf("refreshToken = %v, want redacted; body=%s", toolInput["refreshToken"], redacted)
	}
	if toolInput["value"] != "visible" {
		t.Fatalf("visible value = %v, want visible", toolInput["value"])
	}
}

func TestRedactSensitiveJSONBytesLeavesNonJSONUnchanged(t *testing.T) {
	raw := []byte("token=secret")
	if got := RedactSensitiveJSONBytes(raw); string(got) != string(raw) {
		t.Fatalf("RedactSensitiveJSONBytes(non-json) = %q, want %q", got, raw)
	}
}

func TestRedactSensitiveJSONBytesRedactsCredentialTextValues(t *testing.T) {
	raw := []byte(`{
		"error":{"message":"upstream failed Authorization: Bearer sk-secret-token","details":["api_key=sk-array-secret","visible"]},
		"safe":"visible"
	}`)

	redacted := string(RedactSensitiveJSONBytes(raw))
	for _, leaked := range []string{"sk-secret-token", "sk-array-secret"} {
		if strings.Contains(redacted, leaked) {
			t.Fatalf("JSON string value leaked %q: %s", leaked, redacted)
		}
	}
	if !containsAll(redacted, "Authorization: Bearer [REDACTED]", "api_key=[REDACTED]", "visible") {
		t.Fatalf("JSON string value redaction missing expected content: %s", redacted)
	}
}

func TestRedactSensitiveLogBytesRedactsSSEDataJSON(t *testing.T) {
	raw := []byte("event: response.output_item.done\ndata: {\"access_token\":\"secret-token\",\"value\":\"visible\"}\n\ndata: [DONE]\n")

	redacted := string(RedactSensitiveLogBytes(raw))
	if redacted == string(raw) {
		t.Fatalf("expected SSE log payload to be redacted")
	}
	if redacted == "" || redacted == "[REDACTED]" {
		t.Fatalf("unexpected redacted SSE payload: %q", redacted)
	}
	if redactedContains := containsAll(redacted, "secret-token"); redactedContains {
		t.Fatalf("SSE log leaked token: %s", redacted)
	}
	if !containsAll(redacted, "[REDACTED]", "visible", "data: [DONE]") {
		t.Fatalf("SSE log missing expected redacted content: %s", redacted)
	}
}

func TestRedactSensitiveLogBytesRedactsPlainTextCredentials(t *testing.T) {
	raw := []byte("upstream failed: Authorization: Bearer sk-secret-token api_key=sk-query-secret access_token: token-secret safe=value")

	redacted := string(RedactSensitiveLogBytes(raw))
	for _, leaked := range []string{"sk-secret-token", "sk-query-secret", "token-secret"} {
		if strings.Contains(redacted, leaked) {
			t.Fatalf("plain text log leaked %q: %s", leaked, redacted)
		}
	}
	if !containsAll(redacted, "Authorization: Bearer [REDACTED]", "api_key=[REDACTED]", "access_token: [REDACTED]", "safe=value") {
		t.Fatalf("plain text log missing expected redactions: %s", redacted)
	}
}

func TestRedactSensitiveJSONBytesKeepsTokenUsageFields(t *testing.T) {
	raw := []byte(`{
		"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"token_usage":40},
		"refreshToken":"refresh-secret",
		"session_token":"session-secret"
	}`)

	redacted := RedactSensitiveJSONBytes(raw)
	var got map[string]any
	if err := json.Unmarshal(redacted, &got); err != nil {
		t.Fatalf("redacted JSON is invalid: %v; body=%s", err, redacted)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(20) || usage["total_tokens"] != float64(30) || usage["token_usage"] != float64(40) {
		t.Fatalf("usage fields were unexpectedly redacted: %#v; body=%s", usage, redacted)
	}
	if got["refreshToken"] != "[REDACTED]" {
		t.Fatalf("refreshToken = %v, want redacted; body=%s", got["refreshToken"], redacted)
	}
	if got["session_token"] != "[REDACTED]" {
		t.Fatalf("session_token = %v, want redacted; body=%s", got["session_token"], redacted)
	}
}

func TestMaskSensitiveQuery(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{name: "unchanged", raw: "model=gpt-5&stream=true", want: "model=gpt-5&stream=true"},
		{name: "preserves empty and encoded segments", raw: "page=1&&filter=a%20b&", want: "page=1&&filter=a%20b&"},
		{name: "masks token", raw: "auth_token=abcdefghij&safe=a%2Fb", want: "auth_token=abcd...ghij&safe=a%2Fb"},
		{name: "matches encoded uppercase key", raw: "safe=1&API%5FKEY=abcdef&next=2", want: "safe=1&API%5FKEY=ab...ef&next=2"},
		{name: "sensitive key without equals", raw: "safe=1&token&next=2", want: "safe=1&token=&next=2"},
		{name: "masks multiple values", raw: "token=abcdefghij&&client_secret=uvwxyz&safe=1", want: "token=abcd...ghij&&client_secret=uv...yz&safe=1"},
		{name: "malformed encoded value", raw: "token=%zz&safe=1", want: "token=%25...z&safe=1"},
		{name: "malformed encoded key", raw: "auth%zz_token=abcdefghij", want: "auth%zz_token=abcd...ghij"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaskSensitiveQuery(tt.raw); got != tt.want {
				t.Fatalf("MaskSensitiveQuery(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

var maskSensitiveQueryBenchmarkSink string

func BenchmarkMaskSensitiveQuery(b *testing.B) {
	b.Run("passthrough", func(b *testing.B) {
		raw := "model=gpt-5&stream=true&include=usage&request_id=req-123"
		b.ReportAllocs()
		for b.Loop() {
			maskSensitiveQueryBenchmarkSink = MaskSensitiveQuery(raw)
		}
	})

	b.Run("masked", func(b *testing.B) {
		raw := "model=gpt-5&auth_token=abcdefghijklmnopqrstuvwxyz&stream=true"
		b.ReportAllocs()
		for b.Loop() {
			maskSensitiveQueryBenchmarkSink = MaskSensitiveQuery(raw)
		}
	})
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
