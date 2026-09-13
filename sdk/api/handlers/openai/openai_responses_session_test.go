package openai

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesExplicitExecutionSessionIDPriority(t *testing.T) {
	t.Parallel()

	rawJSON := []byte(`{"prompt_cache_key":"cache-1","conversation_id":"conv-1","thread_id":"thread-1","session_id":"session-1"}`)
	got := responsesExplicitExecutionSessionID(nil, rawJSON)
	if got != "thread-1" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want thread-1", got)
	}

	rawJSON = []byte(`{"prompt_cache_key":"cache-1","conversation_id":"conv-1","session_id":"session-1"}`)
	got = responsesExplicitExecutionSessionID(nil, rawJSON)
	if got != "conv-1" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want conv-1", got)
	}

	rawJSON = []byte(`{"prompt_cache_key":"cache-1","session_id":"session-1"}`)
	got = responsesExplicitExecutionSessionID(nil, rawJSON)
	if got != "session-1" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want session-1", got)
	}

	rawJSON = []byte(`{"prompt_cache_key":"cache-1"}`)
	got = responsesExplicitExecutionSessionID(nil, rawJSON)
	if got != "cache-1" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want cache-1", got)
	}
}

func TestResponsesExplicitExecutionSessionIDHeaderPriority(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("Session_id", "header-session")
	req.Header.Set("Thread_id", "header-thread")
	req.Header.Set("Conversation_id", "header-conv")

	got := responsesExplicitExecutionSessionID(req, []byte(`{"session_id":"body-session"}`))
	if got != "header-thread" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want header-thread", got)
	}
}

func TestResponsesExplicitExecutionSessionIDTurnMetadata(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("Session_id", "header-session")
	req.Header.Set("X-Client-Request-Id", "request-ephemeral")
	req.Header.Set("X-Codex-Turn-Metadata", `{"session_id":"meta-session","thread_id":"meta-thread"}`)

	got := responsesExplicitExecutionSessionID(req, nil)
	if got != "meta-thread" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want meta-thread", got)
	}

	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"body-session\",\"thread_id\":\"body-thread\"}"}}`)
	got = responsesExplicitExecutionSessionID(nil, body)
	if got != "body-thread" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want body-thread", got)
	}
}

func TestResponsesExplicitExecutionSessionIDBodyFallbacks(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"nested thread takes priority", `{"thread_id":"top","metadata":{"thread_id":"nested"},"client_metadata":{"x-codex-turn-metadata":{"thread_id":" first "}}}`, "first"},
		{"empty values fall through", `{"metadata":{"thread_id":" ","threadId":" fallback "},"thread_id":"top"}`, "fallback"},
		{"conversation precedes nested session", `{"conversationId":"conversation","client_metadata":{"x-codex-turn-metadata":{"session_id":"session"}}}`, "conversation"},
		{"metadata cache fallback", `{"prompt_cache_key":" ","metadata":{"prompt_cache_key":"cache"}}`, "cache"},
		{"encoded turn metadata remains last", `{"prompt_cache_key":"cache","client_metadata":{"x-codex-turn-metadata":"{\"thread_id\":\"thread\"}"}}`, "cache"},
		{"first duplicate remains empty", `{"thread_id":" ","thread_id":"later","session_id":"session"}`, "session"},
		{"no session", `{"model":"test","input":[]}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			got := responsesExplicitExecutionSessionID(nil, body)
			for i := range body {
				body[i] = 'x'
			}
			if got != tt.want {
				t.Fatalf("session = %q, want %q", got, tt.want)
			}
		})
	}
}

func BenchmarkResponsesExecutionSessionID(b *testing.B) {
	input := `"input":[{"role":"user","content":"` + strings.Repeat("long conversation ", 8192) + `"}]`
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"SmallThread", `{"thread_id":"thread","input":[]}`, "thread"},
		{"NestedThread", `{"client_metadata":{"x-codex-turn-metadata":{"thread_id":"thread"}},` + input + `}`, "thread"},
		{"TopLevelThread", `{"thread_id":"thread",` + input + `}`, "thread"},
		{"CacheFallback", `{` + input + `,"prompt_cache_key":"cache"}`, "cache"},
		{"Missing", `{` + input + `}`, ""},
	} {
		body := []byte(tc.body)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := responsesExplicitExecutionSessionID(nil, body); got != tc.want {
					b.Fatalf("session = %q, want %q", got, tc.want)
				}
			}
		})
	}
}
