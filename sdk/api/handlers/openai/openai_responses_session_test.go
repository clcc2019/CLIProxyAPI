package openai

import (
	"net/http/httptest"
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
