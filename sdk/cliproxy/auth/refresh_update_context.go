package auth

import "context"

type refreshUpdateContextKey struct{}

type RefreshUpdateCallback func(context.Context, *Auth)

func WithRefreshUpdateCallback(ctx context.Context, cb RefreshUpdateCallback) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, refreshUpdateContextKey{}, cb)
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
