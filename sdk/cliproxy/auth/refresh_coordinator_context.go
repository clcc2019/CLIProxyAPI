package auth

import "context"

// refreshCoordinatorContextKey carries a function that serializes concurrent
// refreshes for the same auth ID through the Manager's singleflight group.
type refreshCoordinatorContextKey struct{}

// RefreshCoordinator refreshes credentials for the given auth under the
// Manager's singleflight, ensuring that concurrent callers (request-time
// refresh from the executor AND the background auto-refresh loop) never
// use the same refresh token twice. Some identity providers rotate the refresh
// token on every call; a concurrent double-use bricks the credentials with
// invalid_grant.
//
// Returns the refreshed auth (possibly nil on permanent failure) and any
// error. Implementations MUST publish the refreshed auth through their
// normal update path (e.g., Manager.Update) so it is persisted to disk
// before any further use.
type RefreshCoordinator func(ctx context.Context, auth *Auth) (*Auth, error)

// WithRefreshCoordinator annotates ctx with a coordinator that the executor
// can invoke instead of calling Refresh directly. The value is typically
// installed by the conductor before dispatching a request.
func WithRefreshCoordinator(ctx context.Context, coord RefreshCoordinator) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if coord == nil {
		return ctx
	}
	return context.WithValue(ctx, refreshCoordinatorContextKey{}, coord)
}

// RefreshCoordinatorFrom returns the coordinator installed in ctx, or nil
// when no coordinator is present (e.g., unit tests that invoke executors
// without going through the Manager).
func RefreshCoordinatorFrom(ctx context.Context) RefreshCoordinator {
	if ctx == nil {
		return nil
	}
	coord, _ := ctx.Value(refreshCoordinatorContextKey{}).(RefreshCoordinator)
	return coord
}

// CoordinatedRefresh runs the fallback refresh function through the
// Manager's singleflight when a coordinator is installed, otherwise
// invokes fallback directly. This lets executors funnel request-time refresh
// through the same singleflight as the background auto-refresh loop.
func CoordinatedRefresh(ctx context.Context, auth *Auth, fallback func(context.Context, *Auth) (*Auth, error)) (*Auth, error) {
	if fallback == nil {
		return nil, nil
	}
	if auth == nil {
		return fallback(ctx, auth)
	}
	coord := RefreshCoordinatorFrom(ctx)
	if coord == nil {
		return fallback(ctx, auth)
	}
	return coord(ctx, auth)
}
