package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
)

func TestCodexStatusClassificationCaseInsensitive(t *testing.T) {
	if !isCodexUsageLimitError([]byte(`{"error":{"message":"You've Hit Your Usage Limit. Upgrade To Plus."}}`)) {
		t.Fatal("expected mixed-case usage limit message to match")
	}
	if !isCodexUnauthorizedError([]byte(`{"error":{"message":"Authentication Token Has Been Invalidated"}}`)) {
		t.Fatal("expected mixed-case invalidated token message to match")
	}
	if !isCodexModelCapacityError([]byte(`{"error":{"message":"Selected Model Is At Capacity"}}`)) {
		t.Fatal("expected mixed-case model capacity message to match")
	}
	if !codexTerminalErrorIsContextLength([]byte(`{"error":{"message":"Too Many Tokens in the Context Window"}}`)) {
		t.Fatal("expected mixed-case context length message to match")
	}
	if codexTerminalErrorIsContextLength([]byte(`{"error":{"message":"unrelated failure"}}`)) {
		t.Fatal("did not expect unrelated message to match context length")
	}
}

func TestCodexWebsocketErrorClassificationCaseInsensitive(t *testing.T) {
	if !codexWebsocketPreviousResponseNotFound([]byte(`{"error":{"message":"Previous Response Was Not Found"}}`)) {
		t.Fatal("expected mixed-case previous response message to match")
	}
	if !codexWebsocketPreviousResponseNotFound([]byte(`{"error":{"message":"PREVIOUS_RESPONSE_ID not found"}}`)) {
		t.Fatal("expected mixed-case previous_response_id message to match")
	}
	if !codexWebsocketNoToolCallFoundForFunctionOutput([]byte(`{"error":{"message":"No Tool Call Found For Call Output item"}}`)) {
		t.Fatal("expected mixed-case no-tool-call message to match")
	}
	if !codexWebsocketConnectionLimitReached([]byte(`{"error":{"code":"Websocket_Connection_Limit_Reached"}}`)) {
		t.Fatal("expected mixed-case websocket connection limit code to match")
	}
}

func BenchmarkCodexStatusErrorClassification(b *testing.B) {
	b.Run("code", func(b *testing.B) {
		body := []byte(`{"error":{"code":"rate_limit_exceeded"}}`)

		for b.Loop() {
			if !isCodexUsageLimitError(body) {
				b.Fatal("expected usage limit")
			}
		}
	})

	b.Run("message", func(b *testing.B) {
		body := []byte(`{"error":{"message":"You've hit your usage limit. Upgrade to Plus or continue using Codex later."}}`)

		for b.Loop() {
			if !isCodexUsageLimitError(body) {
				b.Fatal("expected usage limit")
			}
		}
	})

	b.Run("bodyFallback", func(b *testing.B) {
		body := []byte(`plain text: You've hit your usage limit. Upgrade to Plus.`)

		for b.Loop() {
			if !isCodexUsageLimitError(body) {
				b.Fatal("expected usage limit")
			}
		}
	})
}

func BenchmarkCodexWebsocketErrorClassification(b *testing.B) {
	b.Run("previousResponseCode", func(b *testing.B) {
		payload := []byte(`{"error":{"code":"previous_response_not_found"}}`)
		for b.Loop() {
			if !codexWebsocketPreviousResponseNotFound(payload) {
				b.Fatal("expected previous response not found")
			}
		}
	})

	b.Run("previousResponseMessage", func(b *testing.B) {
		payload := []byte(`{"error":{"message":"Previous response was not found"}}`)
		for b.Loop() {
			if !codexWebsocketPreviousResponseNotFound(payload) {
				b.Fatal("expected previous response not found")
			}
		}
	})

	b.Run("noToolCall", func(b *testing.B) {
		payload := []byte(`{"error":{"message":"No tool call found for call output item"}}`)
		for b.Loop() {
			if !codexWebsocketNoToolCallFoundForFunctionOutput(payload) {
				b.Fatal("expected no tool call found")
			}
		}
	})
}

func BenchmarkContainsASCIIFold(b *testing.B) {
	text := "You've hit your usage limit. Upgrade to Plus or continue using Codex later."
	for b.Loop() {
		if !asciifold.Contains(text, "continue using codex") {
			b.Fatal("expected match")
		}
	}
}

func BenchmarkCodexErrorCode(b *testing.B) {
	b.Run("known", func(b *testing.B) {
		body := []byte(`{"error":{"code":"Context_Length_Exceeded"}}`)
		for b.Loop() {
			if got := codexErrorCode(body); got != "context_length_exceeded" {
				b.Fatalf("got %q", got)
			}
		}
	})

	b.Run("unknown", func(b *testing.B) {
		body := []byte(`{"error":{"code":"Custom_Error_Code"}}`)
		for b.Loop() {
			if got := codexErrorCode(body); got != "custom_error_code" {
				b.Fatalf("got %q", got)
			}
		}
	})
}
