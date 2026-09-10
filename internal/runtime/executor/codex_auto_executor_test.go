package executor

import (
	"context"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexUseWebsocketTransportPrefersUpstreamWebsocket(t *testing.T) {
	wsAuth := &cliproxyauth.Auth{Attributes: map[string]string{"websockets": "true"}}
	httpAuth := &cliproxyauth.Auth{}

	tests := []struct {
		name string
		ctx  context.Context
		auth *cliproxyauth.Auth
		want bool
	}{
		{name: "http default", ctx: context.Background(), auth: wsAuth, want: false},
		{name: "prefer upstream websocket", ctx: cliproxyexecutor.WithPreferUpstreamWebsocket(context.Background()), auth: wsAuth, want: true},
		{name: "downstream websocket implies preference", ctx: cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth: wsAuth, want: true},
		{name: "missing websocket auth", ctx: cliproxyexecutor.WithPreferUpstreamWebsocket(context.Background()), auth: httpAuth, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexUseWebsocketTransport(tc.ctx, tc.auth); got != tc.want {
				t.Fatalf("codexUseWebsocketTransport() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexUseWebsocketTransportDisablesCustomAzureAPIKeyUpstream(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"websockets": "true",
			"api_key":    "test-key",
			"base_url":   "https://example.services.ai.azure.com/openai/v1",
		},
	}
	ctx := cliproxyexecutor.WithPreferUpstreamWebsocket(context.Background())
	if got := codexUseWebsocketTransport(ctx, auth); got {
		t.Fatal("Azure codex-api-key upstream should use HTTP even when websockets=true")
	}
}

func TestCodexWebsocketUpstreamSupportedAllowsChatGPTOAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}
	if !codexWebsocketUpstreamSupported(auth) {
		t.Fatal("official ChatGPT Codex backend should support websocket upstream")
	}
}

func TestCodexUseWebsocketTransportHonorsAzureProviderForCustomDomains(t *testing.T) {
	ctx := cliproxyexecutor.WithPreferUpstreamWebsocket(context.Background())
	for _, baseURL := range []string{"", "https://gateway.example.test/openai/v1"} {
		auth := &cliproxyauth.Auth{Provider: "azure", Attributes: map[string]string{"websockets": "true", "base_url": baseURL}, Metadata: map[string]any{"access_token": "azure-token"}}
		if codexUseWebsocketTransport(ctx, auth) {
			t.Fatalf("Azure provider attempted websocket transport for %q", baseURL)
		}
	}
}
