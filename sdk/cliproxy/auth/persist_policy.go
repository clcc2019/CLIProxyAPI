package auth

import "context"

type skipPersistContextKey struct{}
type refreshPersistContextKey struct{}

// WithSkipPersist returns a derived context that disables persistence for Manager Update/Register calls.
// It is intended for code paths that are reacting to file watcher events, where the file on disk is
// already the source of truth and persisting again would create a write-back loop.
func WithSkipPersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipPersistContextKey{}, true)
}

func shouldSkipPersist(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(skipPersistContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

// WithRefreshUpdate marks an auth update as refresh-originated.
// Refresh updates are ignored when the current auth was already removed or disabled,
// preventing stale in-flight refresh results from recreating deleted credentials.
func WithRefreshUpdate(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, refreshPersistContextKey{}, true)
}

func withRefreshUpdate(ctx context.Context) context.Context {
	return WithRefreshUpdate(ctx)
}

func isRefreshUpdate(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(refreshPersistContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}
