package auth

import (
	"context"
	"strings"
)

type executionContextStateKey struct{}

type RefreshUpdateCallback func(context.Context, *Auth)
type AuthUpdateCallback func(context.Context, *Auth)
type RateLimitUpdateCallback func(context.Context, string, []RateLimitSnapshot)

type executionContextState struct {
	principal          string
	refreshUpdate      RefreshUpdateCallback
	authUpdate         AuthUpdateCallback
	rateLimitUpdate    RateLimitUpdateCallback
	refreshCoordinator RefreshCoordinator
}

func executionStateFromContext(ctx context.Context) executionContextState {
	if ctx == nil {
		return executionContextState{}
	}
	state, _ := ctx.Value(executionContextStateKey{}).(executionContextState)
	return state
}

func withExecutionState(ctx context.Context, state executionContextState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionContextStateKey{}, state)
}

func withExecutionCallbacks(ctx context.Context, refresh RefreshUpdateCallback, authUpdate AuthUpdateCallback, rateLimit RateLimitUpdateCallback, coordinator RefreshCoordinator) context.Context {
	state := executionStateFromContext(ctx)
	if refresh != nil {
		state.refreshUpdate = refresh
	}
	if authUpdate != nil {
		state.authUpdate = authUpdate
	}
	if rateLimit != nil {
		state.rateLimitUpdate = rateLimit
	}
	if coordinator != nil {
		state.refreshCoordinator = coordinator
	}
	return withExecutionState(ctx, state)
}

func withExecutionAuthPrincipal(ctx context.Context, auth *Auth) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	kind, principal := authCredentialPrincipal(auth)
	if kind == "" || principal == "" {
		return ctx
	}
	value := kind + "\x00" + principal
	state := executionStateFromContext(ctx)
	if state.principal == value {
		return ctx
	}
	state.principal = value
	return withExecutionState(ctx, state)
}

func executionAuthPrincipalMatches(ctx context.Context, auth *Auth) bool {
	if ctx == nil {
		return true
	}
	expected := executionStateFromContext(ctx).principal
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
	principal := executionStateFromContext(source).principal
	if principal == "" {
		return ctx
	}
	state := executionStateFromContext(ctx)
	if state.principal == principal {
		return ctx
	}
	state.principal = principal
	return withExecutionState(ctx, state)
}

func WithRefreshUpdateCallback(ctx context.Context, cb RefreshUpdateCallback) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cb == nil {
		return ctx
	}
	state := executionStateFromContext(ctx)
	state.refreshUpdate = cb
	return withExecutionState(ctx, state)
}

func WithAuthUpdateCallback(ctx context.Context, cb AuthUpdateCallback) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cb == nil {
		return ctx
	}
	state := executionStateFromContext(ctx)
	state.authUpdate = cb
	return withExecutionState(ctx, state)
}

func WithRateLimitUpdateCallback(ctx context.Context, cb RateLimitUpdateCallback) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cb == nil {
		return ctx
	}
	state := executionStateFromContext(ctx)
	state.rateLimitUpdate = cb
	return withExecutionState(ctx, state)
}

func PublishRefreshUpdate(ctx context.Context, auth *Auth) {
	if ctx == nil || auth == nil {
		return
	}
	cb := executionStateFromContext(ctx).refreshUpdate
	if cb == nil {
		return
	}
	cb(ctx, auth.Clone())
}

func PublishAuthUpdate(ctx context.Context, auth *Auth) {
	if ctx == nil || auth == nil {
		return
	}
	cb := executionStateFromContext(ctx).authUpdate
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
	cb := executionStateFromContext(ctx).authUpdate
	if cb == nil {
		return
	}
	cb(withExecutionAuthProfileUpdate(ctx), auth.Clone())
}

func PublishRateLimitUpdate(ctx context.Context, authID string, snapshots []RateLimitSnapshot) {
	if ctx == nil || strings.TrimSpace(authID) == "" || len(snapshots) == 0 {
		return
	}
	cb := executionStateFromContext(ctx).rateLimitUpdate
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
