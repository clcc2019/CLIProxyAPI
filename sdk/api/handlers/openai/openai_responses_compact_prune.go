package openai

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/openairesponses"
	log "github.com/sirupsen/logrus"
)

const openAIResponsesCompactPruneMaxAttempts = openairesponses.CompactContextPruneMaxAttempts

func (h *OpenAIResponsesAPIHandler) executeCompactWithPruneFallback(ctx context.Context, model string, rawJSON []byte) ([]byte, http.Header, *interfaces.ErrorMessage) {
	body := rawJSON
	for attempt := 0; ; attempt++ {
		resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(ctx, h.HandlerType(), model, body, "responses/compact")
		if errMsg == nil {
			return resp, upstreamHeaders, nil
		}
		if !openAIResponsesCompactShouldPruneContext(errMsg, attempt) {
			return nil, nil, errMsg
		}
		pruneResult, ok := openairesponses.PruneOldestInputContext(body)
		if !ok {
			log.Warnf("responses/compact context too large but input context could not be pruned (attempt=%d/%d)", attempt+1, openAIResponsesCompactPruneMaxAttempts)
			return nil, nil, errMsg
		}
		log.Warnf(
			"responses/compact context too large; retrying handler execution with pruned input context (attempt=%d/%d, prune=%s, input_items=%d->%d, input_bytes=%d->%d)",
			attempt+1,
			openAIResponsesCompactPruneMaxAttempts,
			pruneResult.Kind,
			pruneResult.OldItems,
			pruneResult.NewItems,
			pruneResult.OldBytes,
			pruneResult.NewBytes,
		)
		body = pruneResult.Body
	}
}

func openAIResponsesCompactShouldPruneContext(errMsg *interfaces.ErrorMessage, attempt int) bool {
	if attempt >= openAIResponsesCompactPruneMaxAttempts || errMsg == nil {
		return false
	}
	if errMsg.Error == nil {
		return false
	}
	message := strings.TrimSpace(errMsg.Error.Error())
	if !openAIResponsesCompactErrorTextMentionsContextLength(message) {
		return false
	}
	switch status := errMsg.StatusCode; {
	case status <= 0:
		return true
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		return true
	case status >= http.StatusInternalServerError:
		return true
	default:
		return false
	}
}

func openAIResponsesCompactErrorTextMentionsContextLength(message string) bool {
	return asciifold.Contains(message, "context_length_exceeded") ||
		asciifold.Contains(message, "context_too_large") ||
		asciifold.Contains(message, "context window") ||
		asciifold.Contains(message, "context length") ||
		asciifold.Contains(message, "maximum context") ||
		asciifold.Contains(message, "max context") ||
		asciifold.Contains(message, "too many tokens")
}
