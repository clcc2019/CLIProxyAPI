package auth

import (
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func init() {
	registerRefreshLead("codex", func() Authenticator { return NewCodexAuthenticator() })
	registerRefreshLead("claude", func() Authenticator { return NewClaudeAuthenticator() })
	registerRefreshLead("gemini", func() Authenticator { return NewGeminiAuthenticator() })
	registerRefreshLead("gemini-cli", func() Authenticator { return NewGeminiAuthenticator() })
	registerRefreshLead("antigravity", func() Authenticator { return NewAntigravityAuthenticator() })
	registerRefreshLead("kimi", func() Authenticator { return NewKimiAuthenticator() })
	registerRefreshLead("kiro", func() Authenticator { return NewKiroAuthenticator() })
	cliproxyauth.RegisterDefaultAutoRefreshProviderWithInterval("kiro", func() time.Duration {
		return time.Duration(kiroauth.RandomRefreshIntervalSeconds()) * time.Second
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
