package executor

import (
	"bytes"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func translateCodexRequestPair(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool) ([]byte, []byte) {
	body := sdktranslator.TranslateRequest(from, to, model, payload, stream)
	if len(originalPayload) == 0 || bytes.Equal(originalPayload, payload) {
		return bytes.Clone(body), body
	}
	return sdktranslator.TranslateRequest(from, to, model, originalPayload, stream), body
}
