package openai

import (
	"net/http/httptest"
	"testing"
)

func TestResponsesExplicitExecutionSessionIDPriority(t *testing.T) {
	t.Parallel()

	rawJSON := []byte(`{"prompt_cache_key":"cache-1","conversation_id":"conv-1","thread_id":"thread-1","session_id":"session-1"}`)
	got := responsesExplicitExecutionSessionID(nil, rawJSON)
	if got != "session-1" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want session-1", got)
	}

	rawJSON = []byte(`{"prompt_cache_key":"cache-1","conversation_id":"conv-1","thread_id":"thread-1"}`)
	got = responsesExplicitExecutionSessionID(nil, rawJSON)
	if got != "thread-1" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want thread-1", got)
	}

	rawJSON = []byte(`{"prompt_cache_key":"cache-1","conversation_id":"conv-1"}`)
	got = responsesExplicitExecutionSessionID(nil, rawJSON)
	if got != "conv-1" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want conv-1", got)
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
	req.Header.Set("Conversation_id", "header-conv")

	got := responsesExplicitExecutionSessionID(req, []byte(`{"session_id":"body-session"}`))
	if got != "header-session" {
		t.Fatalf("responsesExplicitExecutionSessionID() = %q, want header-session", got)
	}
}
