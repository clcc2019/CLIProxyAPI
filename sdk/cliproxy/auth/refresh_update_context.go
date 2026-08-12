package auth

import (
	"context"
	"strings"
)

type refreshUpdateContextKey struct{}
type authUpdateContextKey struct{}
type rateLimitUpdateContextKey struct{}
type executionAuthPrincipalContextKey struct{}

type RefreshUpdateCallback func(context.Context, *Auth)
type AuthUpdateCallback func(context.Context, *Auth)
type RateLimitUpdateCallback func(context.Context, string, []RateLimitSnapshot)

func withExecutionAuthPrincipal(ctx context.Context, auth *Auth) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	kind, principal := authCredentialPrincipal(auth)
	if kind == "" || principal == "" {
		return ctx
	}
	return context.WithValue(ctx, executionAuthPrincipalContextKey{}, kind+"\x00"+principal)
}

func executionAuthPrincipalMatches(ctx context.Context, auth *Auth) bool {
	if ctx == nil {
		return true
	}
	expected, _ := ctx.Value(executionAuthPrincipalContextKey{}).(string)
	if expected == "" {
		return true
	}
	kind, principal := authCredentialPrincipal(auth)
	return kind != "" && principal != "" && expected == kind+"\x00"+principal
}

func withExecutionAuthPrincipalSnapshot(ctx, source context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil {
		return ctx
	}
	principal, _ := source.Value(executionAuthPrincipalContextKey{}).(string)
	if principal == "" {
		return ctx
	}
	return context.WithValue(ctx, executionAuthPrincipalContextKey{}, principal)
}

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

// PublishAuthProfileUpdate publishes a narrowly scoped client-profile update.
// The manager uses the marker to merge it with the latest auth snapshot instead
// of replacing newer state captured by another concurrent request.
func PublishAuthProfileUpdate(ctx context.Context, auth *Auth) {
	if ctx == nil || auth == nil {
		return
	}
	cb, _ := ctx.Value(authUpdateContextKey{}).(AuthUpdateCallback)
	if cb == nil {
		return
	}
	cb(withExecutionAuthProfileUpdate(ctx), auth.Clone())
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
