package auth

import "context"

// Store abstracts persistence of Auth state across restarts.
type Store interface {
	// List returns all auth records stored in the backend.
	List(ctx context.Context) ([]*Auth, error)
	// Save persists the provided auth record, replacing any existing one with same ID.
	Save(ctx context.Context, auth *Auth) (string, error)
	// Delete removes the auth record identified by id.
	Delete(ctx context.Context, id string) error
}

// RuntimeStateStore persists mutable runtime state that is intentionally kept
// out of auth credential files, such as request counters and quota cooldowns.
type RuntimeStateStore interface {
	Load(ctx context.Context) (map[string]AuthRuntimeState, error)
	Save(ctx context.Context, authID string, state AuthRuntimeState) error
	Delete(ctx context.Context, authID string) error
}
