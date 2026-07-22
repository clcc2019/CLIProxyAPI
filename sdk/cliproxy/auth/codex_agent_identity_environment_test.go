package auth

import "testing"

func TestCodexAgentIdentityAuthAPIBaseURL(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want string
	}{
		{name: "nil auth", want: CodexAgentIdentityProductionAuthAPIBaseURL},
		{
			name: "production attributes",
			auth: &Auth{Attributes: map[string]string{"base_url": "https://chatgpt.com/backend-api/codex"}},
			want: CodexAgentIdentityProductionAuthAPIBaseURL,
		},
		{
			name: "staging attributes",
			auth: &Auth{Attributes: map[string]string{"base_url": "https://chatgpt-staging.com/backend-api/codex"}},
			want: CodexAgentIdentityStagingAuthAPIBaseURL,
		},
		{
			name: "staging metadata",
			auth: &Auth{Metadata: map[string]any{"baseUrl": "https://chatgpt-staging.com/backend-api"}},
			want: CodexAgentIdentityStagingAuthAPIBaseURL,
		},
		{
			name: "staging auth api",
			auth: &Auth{Attributes: map[string]string{"base_url": "https://auth.api.openai.org/api/accounts"}},
			want: CodexAgentIdentityStagingAuthAPIBaseURL,
		},
		{
			name: "custom base cannot redirect registration",
			auth: &Auth{Attributes: map[string]string{"base_url": "https://codex.example.com/backend-api"}},
			want: CodexAgentIdentityProductionAuthAPIBaseURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodexAgentIdentityAuthAPIBaseURL(tt.auth); got != tt.want {
				t.Fatalf("CodexAgentIdentityAuthAPIBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
