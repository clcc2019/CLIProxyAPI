package auth

import "context"

type refreshUpdateContextKey struct{}
type authUpdateContextKey struct{}

type RefreshUpdateCallback func(context.Context, *Auth)
type AuthUpdateCallback func(context.Context, *Auth)

func WithRefreshUpdateCallback(ctx context.Context, cb RefreshUpdateCallback) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, refreshUpdateContextKey{}, cb)
}

func WithAuthUpdateCallback(ctx context.Context, cb AuthUpdateCallback) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, authUpdateContextKey{}, cb)
}

func PublishRefreshUpdate(ctx context.Context, auth *Auth) {
	if ctx == nil || auth == nil {
		return
	}
	cb, _ := ctx.Value(refreshUpdateContextKey{}).(RefreshUpdateCallback)
	if cb == nil {
		return
	}
	cb(ctx, auth.Clone())
}

func PublishAuthUpdate(ctx context.Context, auth *Auth) {
	if ctx == nil || auth == nil {
		return
	}
	cb, _ := ctx.Value(authUpdateContextKey{}).(AuthUpdateCallback)
	if cb == nil {
		return
	}
	cb(ctx, auth.Clone())
}
