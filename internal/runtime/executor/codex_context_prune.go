package executor

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/openairesponses"
)

const codexCompactContextPruneMaxAttempts = openairesponses.CompactContextPruneMaxAttempts

func codexShouldPruneCompactContext(statusCode int, body []byte, attempt int) bool {
	if attempt >= codexCompactContextPruneMaxAttempts {
		return false
	}
	if statusCode != http.StatusBadRequest {
		return false
	}
	return codexTerminalErrorIsContextLength(body)
}

func codexPruneOldestInputContext(body []byte) (openairesponses.CompactContextPruneResult, bool) {
	return openairesponses.PruneOldestInputContext(body)
}
