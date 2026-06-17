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
