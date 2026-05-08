package gemini

import (
	"context"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
)

func GeminiTokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}
