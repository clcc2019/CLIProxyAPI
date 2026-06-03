package auth

import (
	"context"
	"time"
)

// Store abstracts persistence of Auth state across restarts.
type Store interface {
	// List returns all auth records stored in the backend.
	List(ctx context.Context) ([]*Auth, error)
	// Save persists the provided auth record, replacing any existing one with same ID.
	Save(ctx context.Context, auth *Auth) (string, error)
	// Delete removes the auth record identified by id.
	Delete(ctx context.Context, id string) error
}

// RuntimeStateStore persists mutable runtime state to an optional external
// store, such as Redis. Auth credential files may also carry a metadata copy.
type RuntimeStateStore interface {
	Load(ctx context.Context) (map[string]AuthRuntimeState, error)
	Save(ctx context.Context, authID string, state AuthRuntimeState) error
	Delete(ctx context.Context, authID string) error
}

// ProxyLease records a stable proxy assignment for an auth file.
type ProxyLease struct {
	AuthID     string
	ProxyURL   string
	AssignedAt time.Time
	UpdatedAt  time.Time
}

// ProxyLeaseFailure records the result of a proxy-pool health update.
type ProxyLeaseFailure struct {
	ProxyURL   string
	Failures   int
	CooledDown bool
	RecoverAt  time.Time
}

// ProxyLeaseStore persists proxy-pool leases to an external store, such as Redis.
type ProxyLeaseStore interface {
	AcquireProxyLease(ctx context.Context, authID string, proxyURLs []string) (ProxyLease, bool, error)
	ReleaseProxyLease(ctx context.Context, authID string) error
	ReconcileProxyLeases(ctx context.Context, activeAuthIDs []string, proxyURLs []string) error
	RecordProxyLeaseFailure(ctx context.Context, authID, proxyURL string, threshold int, cooldown time.Duration) (ProxyLeaseFailure, error)
	ClearProxyLeaseFailure(ctx context.Context, proxyURL string) error
}

// ProxyLeaseBatchStore is an optional extension for stores that can acquire
// leases for multiple auth records in one backend operation.
type ProxyLeaseBatchStore interface {
	AcquireProxyLeases(ctx context.Context, authIDs []string, proxyURLs []string) ([]ProxyLease, error)
}
