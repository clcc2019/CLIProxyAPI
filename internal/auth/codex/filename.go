package codex

import (
	authfile "github.com/router-for-me/CLIProxyAPI/v7/internal/auth"
)

// CredentialFileName returns the filename used to persist Codex OAuth credentials.
func CredentialFileName(email, planType, hashAccountID string, includeProviderPrefix bool) string {
	return authfile.CredentialFileName(email)
}
