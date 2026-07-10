package executor

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// codexApplyModelHeaderOverrides applies trusted model-specific identity
// headers after the normal client/auth/config precedence has been resolved.
// Model metadata must never be able to replace credentials or transport
// headers, so only the Codex client identity fields are accepted here.
func codexApplyModelHeaderOverrides(headers http.Header, baseModel string) {
	if headers == nil {
		return
	}
	overrides := registry.LookupModelHeaderOverrides(baseModel, "codex")
	if overrides.UserAgent != "" {
		codexSetSingleHeaderValue(headers, "User-Agent", overrides.UserAgent)
	}
	if overrides.Originator != "" {
		codexSetSingleHeaderValue(headers, "Originator", overrides.Originator)
	}
	if overrides.Version != "" {
		codexSetSingleHeaderValue(headers, "Version", overrides.Version)
	}
}
