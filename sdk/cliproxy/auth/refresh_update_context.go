package auth

import (
	"context"
	"strings"
)

type refreshUpdateContextKey struct{}
type authUpdateContextKey struct{}
type rateLimitUpdateContextKey struct{}

type RefreshUpdateCallback func(context.Context, *Auth)
type AuthUpdateCallback func(context.Context, *Auth)
type RateLimitUpdateCallback func(context.Context, string, []RateLimitSnapshot)

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

func WithRateLimitUpdateCallback(ctx context.Context, cb RateLimitUpdateCallback) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, rateLimitUpdateContextKey{}, cb)
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

func PublishRateLimitUpdate(ctx context.Context, authID string, snapshots []RateLimitSnapshot) {
	if ctx == nil || strings.TrimSpace(authID) == "" || len(snapshots) == 0 {
		return
	}
	cb, _ := ctx.Value(rateLimitUpdateContextKey{}).(RateLimitUpdateCallback)
	if cb == nil {
		return
	}
	cb(ctx, authID, cloneRateLimitSnapshotSlice(snapshots))
}

func cloneRateLimitSnapshotSlice(src []RateLimitSnapshot) []RateLimitSnapshot {
	if len(src) == 0 {
		return nil
	}
	dst := make([]RateLimitSnapshot, len(src))
	for i, snapshot := range src {
		dst[i] = cloneRateLimitSnapshot(snapshot)
	}
	return dst
}
