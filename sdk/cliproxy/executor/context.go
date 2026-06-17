package executor

import "context"

type downstreamWebsocketContextKey struct{}
type preferUpstreamWebsocketContextKey struct{}

// WithDownstreamWebsocket marks the current request as coming from a downstream websocket connection.
func WithDownstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, downstreamWebsocketContextKey{}, true)
}

// DownstreamWebsocket reports whether the current request originates from a downstream websocket connection.
func DownstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(downstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}

// WithPreferUpstreamWebsocket marks the current request as preferring websocket
// for the upstream provider transport without changing downstream semantics.
func WithPreferUpstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, preferUpstreamWebsocketContextKey{}, true)
}

// PreferUpstreamWebsocket reports whether upstream execution should prefer a
// websocket-capable transport. Downstream websocket requests imply this because
// existing websocket clients already expect websocket upstream behavior.
func PreferUpstreamWebsocket(ctx context.Context) bool {
	if DownstreamWebsocket(ctx) {
		return true
	}
	if ctx == nil {
		return false
	}
	raw := ctx.Value(preferUpstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}
