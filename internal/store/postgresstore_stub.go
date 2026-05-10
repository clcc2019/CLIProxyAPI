//go:build !has_postgres

// Package store — postgres stub.
//
// When the `has_postgres` build tag is NOT set, the real PostgresStore and
// its heavy dependencies (jackc/pgx, ugorji/codec, ~3.8 MB of binary size)
// are excluded. This stub provides the same type identifier and public
// method set so the rest of the codebase compiles, but every operation
// returns an ErrBackendNotCompiled error that surfaces a clear actionable
// message to operators: "rebuild with -tags=has_postgres".
//
// The stub is deliberately minimal — callers who want the postgres backend
// have to rebuild. This keeps the default slim binary free of the driver
// and protocol parsers that most deployments don't use.
package store

import (
	"context"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// PostgresStoreConfig mirrors the real struct so cmd/server/config_loader.go
// builds identically regardless of tags.
type PostgresStoreConfig struct {
	DSN         string
	Schema      string
	SpoolDir    string
	ConfigKey   string
	ConfigTable string
	AuthTable   string
}

// PostgresStore is a no-op placeholder. The real type is defined in
// postgresstore.go under the `has_postgres` build tag.
type PostgresStore struct{}

// NewPostgresStore returns the "not compiled in" sentinel. cmd/server
// checks for this and emits a user-facing warning that the operator must
// rebuild with the has_postgres tag.
func NewPostgresStore(_ context.Context, _ PostgresStoreConfig) (*PostgresStore, error) {
	return nil, errBackendNotCompiled("postgres", "has_postgres")
}

func (s *PostgresStore) Close() error { return nil }
func (s *PostgresStore) EnsureSchema(_ context.Context) error {
	return errBackendNotCompiled("postgres", "has_postgres")
}
func (s *PostgresStore) Bootstrap(_ context.Context, _ string) error {
	return errBackendNotCompiled("postgres", "has_postgres")
}
func (s *PostgresStore) ConfigPath() string  { return "" }
func (s *PostgresStore) AuthDir() string     { return "" }
func (s *PostgresStore) WorkDir() string     { return "" }
func (s *PostgresStore) SetBaseDir(_ string) {}
func (s *PostgresStore) Save(_ context.Context, _ *cliproxyauth.Auth) (string, error) {
	return "", errBackendNotCompiled("postgres", "has_postgres")
}
func (s *PostgresStore) List(_ context.Context) ([]*cliproxyauth.Auth, error) {
	return nil, errBackendNotCompiled("postgres", "has_postgres")
}
func (s *PostgresStore) Delete(_ context.Context, _ string) error {
	return errBackendNotCompiled("postgres", "has_postgres")
}
func (s *PostgresStore) PersistAuthFiles(_ context.Context, _ string, _ ...string) error {
	return errBackendNotCompiled("postgres", "has_postgres")
}
func (s *PostgresStore) PersistConfig(_ context.Context) error {
	return errBackendNotCompiled("postgres", "has_postgres")
}
