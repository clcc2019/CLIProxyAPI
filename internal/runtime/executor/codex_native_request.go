package executor

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type codexNativeClientRequestContextKey struct{}

// codexNativeClientRequest identifies requests that already carry a first-
// party Codex wire identity. These requests use a forward-compatible pruning
// policy so newly added Codex fields survive proxying; translated third-party
// requests continue through the strict compatibility schema.
func codexNativeClientRequest(from sdktranslator.Format, headers http.Header, body []byte) bool {
	if !strings.EqualFold(strings.TrimSpace(from.String()), "openai-response") {
		return false
	}
	if codexFirstPartyOriginator(firstNonEmptyHeaderValue(headers, nil, "Originator")) {
		return true
	}
	if codexFirstPartyUserAgent(firstNonEmptyHeaderValue(headers, nil, "User-Agent")) {
		return true
	}
	if strings.TrimSpace(firstNonEmptyHeaderValue(headers, nil, codexHeaderTurnMetadata)) != "" {
		return true
	}
	turnMetadata := gjson.GetBytes(body, "client_metadata."+codexClientMetadataTurnMetadata)
	return turnMetadata.Type == gjson.String && strings.TrimSpace(turnMetadata.String()) != ""
}

func contextWithCodexNativeClientRequest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexNativeClientRequestContextKey{}, true)
}

func codexNativeClientRequestFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	native, _ := ctx.Value(codexNativeClientRequestContextKey{}).(bool)
	return native
}

// codexTranslateRequestWithOriginal keeps first-party Codex Responses payloads
// out of the generic compatibility translator. The final upstream normalizer
// still applies transport invariants, model capabilities, and safety cleanup,
// but fields introduced by newer Codex clients are not deleted before the
// forward-compatible native policy gets a chance to inspect them.
func codexTranslateRequestWithOriginal(
	cfg *config.Config,
	ctx context.Context,
	from sdktranslator.Format,
	to sdktranslator.Format,
	model string,
	payload []byte,
	original []byte,
	stream bool,
	headers http.Header,
) (translated []byte, originalTranslated []byte, native bool) {
	native = codexNativeClientRequest(from, headers, payload) ||
		codexNativeClientRequest(from, codexGinHeadersFromContext(ctx), payload)
	if !native {
		translated, originalTranslated = helps.TranslateRequestWithOriginal(cfg, from, to, model, payload, original, stream)
		return translated, originalTranslated, false
	}

	translated = bytes.Clone(payload)
	if len(original) > 0 {
		// Payload rules and response translators only inspect the original request;
		// they never mutate it. Keep the owned clone for the body that continues
		// through normalization, but borrow the caller's immutable original bytes
		// instead of copying the same native payload a second time.
		originalTranslated = original
	}
	return translated, originalTranslated, true
}

func codexFirstPartyOriginator(originator string) bool {
	originator = strings.TrimSpace(originator)
	return originator == "codex_cli_rs" ||
		originator == "codex-tui" ||
		originator == "codex_vscode" ||
		originator == "codex_atlas" ||
		originator == "codex_chatgpt_desktop" ||
		strings.HasPrefix(originator, "Codex ")
}

func codexFirstPartyUserAgent(userAgent string) bool {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return false
	}
	product := userAgent
	if idx := strings.IndexAny(product, "/ "); idx >= 0 {
		product = product[:idx]
	}
	return codexFirstPartyOriginator(product) || strings.HasPrefix(userAgent, "Codex ")
}
