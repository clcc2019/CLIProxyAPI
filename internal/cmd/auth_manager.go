package cmd

import (
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
)

// newAuthManager creates a new authentication manager instance with supported
// authenticators and a file-based token store.
//
// Returns:
//   - *sdkAuth.Manager: A configured authentication manager instance
func newAuthManager() *sdkAuth.Manager {
	store := sdkAuth.GetTokenStore()
	manager := sdkAuth.NewManager(store,
		sdkAuth.NewCodexAuthenticator(),
		sdkAuth.NewClaudeAuthenticator(),
		sdkAuth.NewKimiAuthenticator(),
		sdkAuth.NewXAIAuthenticator(),
	)
	return manager
}
