package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexSetPairedSingleHeaderValuesKeepsSlicesIndependent(t *testing.T) {
	headers := http.Header{}
	codexSetPairedSingleHeaderValues(headers, "First", "one", "Second", "two")

	headers["First"] = append(headers["First"], "extra")
	if got := headers.Get("Second"); got != "two" {
		t.Fatalf("Second = %q after appending First, want two", got)
	}
	if got := len(headers["Second"]); got != 1 {
		t.Fatalf("len(Second) = %d, want 1", got)
	}
}

func TestCodexSetTripleSingleHeaderValuesKeepsSlicesIndependent(t *testing.T) {
	headers := http.Header{}
	codexSetTripleSingleHeaderValues(headers, "First", "one", "Second", "two", "Third", "three")

	headers["Second"] = append(headers["Second"], "extra")
	if got := headers.Get("First"); got != "one" {
		t.Fatalf("First = %q after appending Second, want one", got)
	}
	if got := headers.Get("Third"); got != "three" {
		t.Fatalf("Third = %q after appending Second, want three", got)
	}
}

func TestCodexSetSessionIdentityHeadersKeepsSlicesIndependent(t *testing.T) {
	headers := http.Header{}
	codexSetSessionIdentityHeaders(headers, "session-1", "thread-1", "request-1")

	headers[codexHeaderOfficialSessionID] = append(headers[codexHeaderOfficialSessionID], "extra")
	for key, want := range map[string]string{
		codexHeaderOfficialSessionID: "session-1",
		codexHeaderOfficialThreadID:  "thread-1",
		"X-Client-Request-Id":        "request-1",
	} {
		if got := headers.Get(key); got != want {
			t.Fatalf("%s = %q after appending another header, want %q", key, got, want)
		}
	}
}

func TestCodexSetSessionIdentityHeadersUpdatesExistingValues(t *testing.T) {
	headers := http.Header{
		codexHeaderSessionID:  {"old-session", "extra"},
		codexHeaderThreadID:   {"old-thread", "extra"},
		"X-Client-Request-Id": {"old-request", "extra"},
	}
	codexSetSessionIdentityHeaders(headers, "session-2", "thread-2", "request-2")

	for key, want := range map[string]string{
		codexHeaderOfficialSessionID: "session-2",
		codexHeaderOfficialThreadID:  "thread-2",
		"X-Client-Request-Id":        "request-2",
	} {
		if got := headers.Values(key); len(got) != 1 || got[0] != want {
			t.Fatalf("%s = %q, want [%q]", key, got, want)
		}
	}
	for _, key := range []string{codexHeaderSessionID, codexHeaderThreadID} {
		if got := headers.Values(key); len(got) != 0 {
			t.Fatalf("%s = %q, want absent", key, got)
		}
	}
}

var codexSessionIdentityHeadersBenchmarkSink http.Header

func BenchmarkCodexSetSessionIdentityHeadersBatched(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		headers := http.Header{}
		codexSetSessionIdentityHeaders(headers, "session-1", "thread-1", "request-1")
		codexSessionIdentityHeadersBenchmarkSink = headers
	}
}

func TestCodexIsAPIKeyAuthTreatsMirroredAccessTokenAsOAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "access-token",
		},
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "acct_123",
		},
	}

	if codexIsAPIKeyAuth(auth) {
		t.Fatal("mirrored OAuth access token should not be treated as API key auth")
	}
}

func TestCodexIsAPIKeyAuthTreatsOAuthIdentityWithStaleAPIKeyMirrorAsOAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "old-access-token",
		},
		Metadata: map[string]any{
			"access_token": "new-access-token",
			"account_id":   "acct_123",
		},
	}

	if codexIsAPIKeyAuth(auth) {
		t.Fatal("OAuth auth with account metadata should not be treated as API key auth")
	}
}

func TestCodexIsAPIKeyAuthHonorsMetadataOAuthKind(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "access-token",
		},
		Metadata: map[string]any{
			"auth_kind":    " OAuth ",
			"access_token": "access-token",
		},
	}

	if codexIsAPIKeyAuth(auth) {
		t.Fatal("metadata auth_kind=oauth should not be treated as API key auth")
	}
}

func TestCodexIsAPIKeyAuthHonorsExplicitAPIKeyKind(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind": " APIKey ",
			"api_key":   "sk-test",
		},
		Metadata: map[string]any{
			"access_token": "sk-test",
		},
	}

	if !codexIsAPIKeyAuth(auth) {
		t.Fatal("explicit API key auth kind should be treated as API key auth")
	}
}

func BenchmarkCodexAuthKind(b *testing.B) {
	for b.Loop() {
		if got := codexAuthKind(" ChatGPT_Auth_Tokens "); got != "chatgpt_auth_tokens" {
			b.Fatalf("codexAuthKind() = %q", got)
		}
	}
}

func TestCodexCredsPrefersOAuthAccessTokenOverStaleAPIKeyMirror(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "old-access-token",
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
		Metadata: map[string]any{
			"access_token": "new-access-token",
			"account_id":   "acct_123",
		},
	}

	apiKey, baseURL := codexCreds(auth)
	if apiKey != "new-access-token" {
		t.Fatalf("apiKey = %q, want new-access-token", apiKey)
	}
	if baseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("baseURL = %q, want configured base URL", baseURL)
	}
}

func TestCodexCredsKeepsCustomAPIKeyWhenAccessTokenMetadataIsUnidentified(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "custom-api-key",
		},
		Metadata: map[string]any{
			"access_token": "old-access-token",
		},
	}

	apiKey, _ := codexCreds(auth)
	if apiKey != "custom-api-key" {
		t.Fatalf("apiKey = %q, want custom-api-key", apiKey)
	}
}
