package executor

import (
	"context"
	"errors"
	"strings"
)

// ErrNoActiveUpstreamWebsocket means response.interrupt has no live upstream socket.
var ErrNoActiveUpstreamWebsocket = errors.New("no active upstream websocket for response.interrupt")

type websocketAuthCheckContextKey struct{}

// WithWebsocketAuthCheck attaches a credential validation callback for an
// upstream websocket control write.
func WithWebsocketAuthCheck(ctx context.Context, check func(string) bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, websocketAuthCheckContextKey{}, check)
}

// WebsocketAuthEnabled reports whether the credential is still available for
// use by a websocket control operation.
func WebsocketAuthEnabled(ctx context.Context, authID string) bool {
	if ctx == nil {
		return false
	}
	check, ok := ctx.Value(websocketAuthCheckContextKey{}).(func(string) bool)
	if !ok || check == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	return authID != "" && check(authID)
}
