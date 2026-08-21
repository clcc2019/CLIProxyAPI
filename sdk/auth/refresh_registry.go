package auth

import (
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func init() {
	registerRefreshLead("codex", func() Authenticator { return NewCodexAuthenticator() })
	cliproxyauth.RegisterDefaultAutoRefreshProviderWithTiming("codex", codexRefreshExpiryLead, codexRefreshFallbackInterval)
	registerRefreshLead("claude", func() Authenticator { return NewClaudeAuthenticator() })
	registerRefreshLeadDuration("kimi", 5*time.Minute)
	registerRefreshLead("xai", func() Authenticator { return NewXAIAuthenticator() })
}

func registerRefreshLeadDuration(provider string, lead time.Duration) {
	cliproxyauth.RegisterRefreshLeadProvider(provider, func() *time.Duration {
		value := lead
		return &value
	})
}

func registerRefreshLead(provider string, factory func() Authenticator) {
	cliproxyauth.RegisterRefreshLeadProvider(provider, func() *time.Duration {
		if factory == nil {
			return nil
		}
		auth := factory()
		if auth == nil {
			return nil
		}
		return auth.RefreshLead()
	})
}
