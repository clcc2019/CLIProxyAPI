package util

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
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

func TestRedactSensitiveLogBytesRedactsAgentIdentityCredentials(t *testing.T) {
	raw := []byte(`{"agent_private_key":"private-key-material","message":"Authorization: AgentAssertion assertion-payload-secret"}`)
	redacted := string(RedactSensitiveLogBytes(raw))
	for _, leaked := range []string{"private-key-material", "assertion-payload-secret"} {
		if strings.Contains(redacted, leaked) {
			t.Fatalf("agent identity credential leaked %q: %s", leaked, redacted)
		}
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("agent identity credentials were not redacted: %s", redacted)
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

func TestRedactSensitiveLogBytesRedactsPluralPlainTextCredentials(t *testing.T) {
	raw := []byte("access_tokens=leaked-access refresh_tokens: leaked-refresh client_secrets=leaked-client")
	redacted := string(RedactSensitiveLogBytes(raw))
	for _, leaked := range []string{"leaked-access", "leaked-refresh", "leaked-client"} {
		if strings.Contains(redacted, leaked) {
			t.Fatalf("plain text leaked %q after plural-key redaction: %s", leaked, redacted)
		}
	}
	if strings.Count(redacted, "[REDACTED]") != 3 {
		t.Fatalf("plural plain-text credentials were not all redacted: %s", redacted)
	}
}

func TestRedactSensitiveLogBytesReturnsOriginalSafeJSON(t *testing.T) {
	raw := []byte(` {"type":"response.output_text.delta","delta":"visible"} `)
	redacted := RedactSensitiveLogBytes(raw)
	if string(redacted) != string(raw) {
		t.Fatalf("safe JSON changed: got %q, want %q", redacted, raw)
	}
	if &redacted[0] != &raw[0] {
		t.Fatal("safe JSON should reuse the original buffer")
	}
}

func TestRedactSensitiveLogBytesPrefilterCoversShortSensitiveKeys(t *testing.T) {
	raw := []byte(`{"auth":"visible-auth-value","passcode":"visible-passcode-value","safe":"kept"}`)
	redacted := string(RedactSensitiveLogBytes(raw))
	for _, leaked := range []string{"visible-auth-value", "visible-passcode-value"} {
		if strings.Contains(redacted, leaked) {
			t.Fatalf("JSON leaked %q after prefilter: %s", leaked, redacted)
		}
	}
	if !containsAll(redacted, `"auth":"[REDACTED]"`, `"passcode":"[REDACTED]"`, `"safe":"kept"`) {
		t.Fatalf("JSON prefilter redaction missing expected fields: %s", redacted)
	}
}

func TestRedactSensitiveLogBytesPrefilterDoesNotBypassEscapedSensitiveKeys(t *testing.T) {
	raw := []byte(`{"\u0061uth":"hidden-auth-value","safe":"kept"}`)
	redacted := string(RedactSensitiveLogBytes(raw))
	if strings.Contains(redacted, "hidden-auth-value") {
		t.Fatalf("escaped sensitive JSON key leaked through prefilter: %s", redacted)
	}
	if !containsAll(redacted, `"auth":"[REDACTED]"`, `"safe":"kept"`) {
		t.Fatalf("escaped sensitive JSON key was not redacted: %s", redacted)
	}
}

func TestRedactSensitiveLogBytesPrefilterDoesNotBypassUnicodeCaseFoldedKeys(t *testing.T) {
	raw := []byte(`{"toKen":"hidden-token-value","safe":"kept"}`)
	redacted := string(RedactSensitiveLogBytes(raw))
	if strings.Contains(redacted, "hidden-token-value") {
		t.Fatalf("Unicode case-folded sensitive JSON key leaked through prefilter: %s", redacted)
	}
	if !containsAll(redacted, `"toKen":"[REDACTED]"`, `"safe":"kept"`) {
		t.Fatalf("Unicode case-folded sensitive JSON key was not redacted: %s", redacted)
	}
}

func TestRedactSensitiveLogBytesReturnsOriginalWhenMarkersNeedNoRedaction(t *testing.T) {
	raw := []byte(` {"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":30,"token_usage":30}} `)
	redacted := RedactSensitiveLogBytes(raw)
	if string(redacted) != string(raw) {
		t.Fatalf("benign token usage JSON changed: got %q, want %q", redacted, raw)
	}
	if &redacted[0] != &raw[0] {
		t.Fatal("benign token usage JSON should reuse the original buffer")
	}
}

func TestRedactSensitiveLogBytesKeepsUnicodeUsageJSONOnFastPath(t *testing.T) {
	raw := []byte(`{"message":"你好，世界","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`)
	redacted := RedactSensitiveLogBytes(raw)
	if string(redacted) != string(raw) {
		t.Fatalf("Unicode usage JSON changed: got %q, want %q", redacted, raw)
	}
	if &redacted[0] != &raw[0] {
		t.Fatal("Unicode usage JSON should reuse the original buffer")
	}
}

func TestRedactSensitiveLogBytesKeepsUsageSSEOnFastPath(t *testing.T) {
	raw := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30}}}\n\n")
	redacted := RedactSensitiveLogBytes(raw)
	if string(redacted) != string(raw) {
		t.Fatalf("usage SSE changed: got %q, want %q", redacted, raw)
	}
	if &redacted[0] != &raw[0] {
		t.Fatal("usage SSE should reuse the original buffer")
	}
}

func TestRedactSensitiveLogBytesDoesNotBypassTokenOutsideSSEData(t *testing.T) {
	raw := []byte("event: access_token=leaked-value\ndata: {\"usage\":{\"input_tokens\":10}}\n\n")
	redacted := string(RedactSensitiveLogBytes(raw))
	if strings.Contains(redacted, "leaked-value") {
		t.Fatalf("SSE token outside data field leaked through fast path: %s", redacted)
	}
	if !strings.Contains(redacted, "access_token=[REDACTED]") {
		t.Fatalf("SSE token outside data field was not redacted: %s", redacted)
	}
}

func TestJSONContainsNoSensitiveCredentials(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "nested usage keys",
			raw:  `{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":30}}`,
			want: true,
		},
		{name: "case insensitive usage key", raw: `{"INPUT_TOKENS_DETAILS":{"CACHED_TOKENS":4}}`, want: true},
		{name: "generic plural tokens", raw: `{"tokens":10,"token_usage":10}`, want: true},
		{name: "anthropic cache token usage", raw: `{"cache_creation_input_tokens":10,"cache_read_input_tokens":4}`, want: true},
		{name: "sensitive exact token", raw: `{"token":"secret"}`, want: false},
		{name: "sensitive camel token", raw: `{"refreshToken":"secret"}`, want: false},
		{name: "sensitive suffix token", raw: `{"custom_token":"secret"}`, want: false},
		{name: "unknown plural token key", raw: `{"custom_tokens":10}`, want: false},
		{name: "access token collection", raw: `{"access_tokens":["leaked"]}`, want: false},
		{name: "refresh token collection", raw: `{"refresh_tokens":["leaked"]}`, want: false},
		{name: "session token collection", raw: `{"session_tokens":["leaked"]}`, want: false},
		{name: "token in string value", raw: `{"message":"access_token=secret"}`, want: false},
		{name: "safe marker words in value", raw: `{"message":"The author explains API token accounting without credentials."}`, want: true},
		{name: "authorization in string value", raw: `{"message":"Authorization: Bearer leaked-value"}`, want: false},
		{name: "escaped token key", raw: `{"input_\u0074okens":10}`, want: false},
		{name: "no sensitive marker", raw: `{"type":"response.completed"}`, want: true},
		{name: "invalid JSON", raw: `{"input_tokens":`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonContainsNoSensitiveCredentials([]byte(tt.raw)); got != tt.want {
				t.Fatalf("jsonContainsNoSensitiveCredentials() = %v, want %v", got, tt.want)
			}
		})
	}
}

func FuzzJSONContainsNoSensitiveCredentialsNeverSkipsRedaction(f *testing.F) {
	for _, seed := range []string{
		`{"type":"response.completed","usage":{"input_tokens":10}}`,
		`{"auth":"leaked"}`,
		`{"access_tokens":["leaked"]}`,
		`{"message":"Authorization: Bearer leaked-value"}`,
		`{"message":"The author explains API token accounting without credentials."}`,
		`"access_token=leaked-value"`,
		`{"\u0061uth":"leaked"}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		data := []byte(raw)
		if !jsonContainsNoSensitiveCredentials(data) {
			return
		}
		if redacted := RedactSensitiveJSONBytes(data); !bytes.Equal(redacted, data) {
			t.Fatalf("classifier skipped JSON changed by full redactor: input=%q redacted=%q", data, redacted)
		}
	})
}

func FuzzTextOrSSEContainsNoSensitiveCredentialsNeverSkipsRedaction(f *testing.F) {
	for _, seed := range []string{
		"event: response.completed\ndata: {\"usage\":{\"input_tokens\":10}}\n\n",
		"event: access_token=leaked-value\ndata: {}\n\n",
		"data: {\"auth\":\"leaked-value\"}\n\n",
		"data: \"Authorization: Bearer leaked-value\"\n\n",
		"The author explains API token accounting without credentials.",
		"refresh_tokens=leaked-value",
		"ACCesstoken=\n0",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		data := []byte(raw)
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 || mayHideSensitiveASCIIText(trimmed) {
			return
		}
		jsonCandidate := trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"'
		if jsonCandidate && json.Valid(trimmed) {
			return
		}
		tokenOnly := asciifold.ContainsBytes(trimmed, "token") && !mayContainNonTokenSensitiveTextBytes(trimmed)
		if !textOrSSEContainsNoSensitiveCredentials(trimmed, tokenOnly) {
			return
		}
		reference := redactSensitivePlainTextBytes(redactSensitiveSSEDataBytes(data))
		if !bytes.Equal(reference, data) {
			t.Fatalf("classifier skipped text changed by full redactor: input=%q redacted=%q", data, reference)
		}
	})
}

func TestRedactSensitiveLogBytesReturnsOriginalSafeMarkerJSON(t *testing.T) {
	raw := []byte(` {"type":"response.output_text.delta","delta":"The author explains API token accounting without credentials."} `)
	redacted := RedactSensitiveLogBytes(raw)
	if string(redacted) != string(raw) {
		t.Fatalf("safe marker JSON changed: got %q, want %q", redacted, raw)
	}
	if &redacted[0] != &raw[0] {
		t.Fatal("safe marker JSON should reuse the original buffer")
	}
}

func TestRedactSensitiveLogBytesTokenClassifierPreservesSensitivePaths(t *testing.T) {
	for _, raw := range []string{
		`{"token":"secret-exact"}`,
		`{"refreshToken":"secret-refresh"}`,
		`{"session-token":"secret-session"}`,
		`{"custom_token":"secret-custom"}`,
		`{"message":"access_token=secret-message"}`,
		`{"access_tokens":["leaked-access-value"]}`,
		`{"refresh_tokens":["leaked-refresh-value"]}`,
		`{"session_tokens":["leaked-session-value"]}`,
	} {
		redacted := string(RedactSensitiveLogBytes([]byte(raw)))
		if strings.Contains(redacted, "secret-") || strings.Contains(redacted, "leaked-") {
			t.Fatalf("token classifier bypassed sensitive JSON: input=%s output=%s", raw, redacted)
		}
		if !strings.Contains(redacted, "[REDACTED]") {
			t.Fatalf("token classifier did not preserve redaction: input=%s output=%s", raw, redacted)
		}
	}
}

func TestRedactSensitiveLogBytesFastPathsCoverEverySensitiveKeyRule(t *testing.T) {
	keys := []string{
		"authorization", "auth", "auth_token", "access_token", "refresh_token", "id_token",
		"token", "bearer_token", "session_token", "api_key", "apikey", "x_api_key",
		"secret", "client_secret", "private_key", "agent_private_key", "agentPrivateKey", "password", "passcode", "credential", "credentials",
		"AuthToken", "AccessToken", "RefreshToken", "IdToken", "BearerToken", "SessionToken",
		"XApiKey", "ClientSecret", "custom_authorization_value", "custom_api_key_value",
		"custom-apikey-value", "custom_token", "custom_secret", "user_password_hash",
		"credential_payload", " AUTH ", "session token", "X-API-KEY",
		"access_tokens", "refresh_tokens", "session_tokens", "id_tokens",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			if !shouldRedactJSONKey(key) {
				t.Fatalf("test key %q no longer matches shouldRedactJSONKey", key)
			}
			raw := []byte(`{"` + key + `":"leaked-sensitive-value","safe":"kept"}`)
			redacted := string(RedactSensitiveLogBytes(raw))
			if strings.Contains(redacted, "leaked-sensitive-value") {
				t.Fatalf("sensitive key %q leaked through fast path: %s", key, redacted)
			}
			if !containsAll(redacted, "[REDACTED]", `"safe":"kept"`) {
				t.Fatalf("sensitive key %q was not redacted correctly: %s", key, redacted)
			}
		})
	}
}

func TestRedactSensitiveLogBytesLeavesBenignTokenKeysUnchanged(t *testing.T) {
	keys := []string{
		"input_tokens", "output_tokens", "total_tokens", "cached_tokens", "reasoning_tokens",
		"input_tokens_details", "output_tokens_details", "accepted_prediction_tokens",
		"rejected_prediction_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
		"token_usage", "tokens", "mytoken",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			if shouldRedactJSONKey(key) {
				t.Fatalf("benign key %q unexpectedly matches shouldRedactJSONKey", key)
			}
			raw := []byte(`{"` + key + `":10}`)
			redacted := RedactSensitiveLogBytes(raw)
			if string(redacted) != string(raw) {
				t.Fatalf("benign key %q changed: got %s, want %s", key, redacted, raw)
			}
		})
	}
}

func TestRedactSensitiveLogBytesRedactsTopLevelJSONString(t *testing.T) {
	raw := []byte(`"Authorization: Bearer top-level-secret"`)
	redacted := string(RedactSensitiveLogBytes(raw))
	if strings.Contains(redacted, "top-level-secret") {
		t.Fatalf("top-level JSON string leaked credentials: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("top-level JSON string was not redacted: %s", redacted)
	}
}

var redactSensitiveLogBytesBenchmarkSink []byte

func BenchmarkRedactSensitiveLogBytesSafeJSON(b *testing.B) {
	payload := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	b.ReportAllocs()
	for b.Loop() {
		redactSensitiveLogBytesBenchmarkSink = RedactSensitiveLogBytes(payload)
	}
}

func BenchmarkRedactSensitiveLogBytesSafeMarkerJSON(b *testing.B) {
	payload := []byte(`{"type":"response.output_text.delta","delta":"The author explains API token accounting without credentials."}`)
	b.ReportAllocs()
	for b.Loop() {
		redactSensitiveLogBytesBenchmarkSink = RedactSensitiveLogBytes(payload)
	}
}

func BenchmarkRedactSensitiveLogBytesUsageJSON(b *testing.B) {
	payload := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":120,"output_tokens":40,"total_tokens":160}}}`)
	b.ReportAllocs()
	for b.Loop() {
		redactSensitiveLogBytesBenchmarkSink = RedactSensitiveLogBytes(payload)
	}
}

func BenchmarkRedactSensitiveLogBytesUsageSSE(b *testing.B) {
	payload := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":120,\"output_tokens\":40,\"total_tokens\":160}}}\n\n")
	b.ReportAllocs()
	for b.Loop() {
		redactSensitiveLogBytesBenchmarkSink = RedactSensitiveLogBytes(payload)
	}
}

func BenchmarkRedactSensitiveLogBytesCredentialJSON(b *testing.B) {
	payload := []byte(`{"type":"error","auth":"secret-value","message":"Authorization: Bearer sk-secret-token"}`)
	b.ReportAllocs()
	for b.Loop() {
		redactSensitiveLogBytesBenchmarkSink = RedactSensitiveLogBytes(payload)
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
