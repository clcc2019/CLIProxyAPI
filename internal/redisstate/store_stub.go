//go:build no_redis

// Package redisstate — redis stub.
//
// Only builds using the no_redis tag exclude the redis/go-redis driver.
// This file keeps the public API surface stable for those explicitly
// size-constrained builds.
package redisstate

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Store is a no-op placeholder for explicit no_redis builds.
type Store struct{}

func Available() bool { return false }

// New always errors in the stub. Normal builds compile the real Redis store;
// no_redis builds continue in in-memory-state mode.
func New(_ context.Context, _ config.RedisConfig) (*Store, error) {
	return nil, fmt.Errorf(
		"redis state store is not compiled in because this binary was built with -tags=no_redis",
	)
}

func (s *Store) Addr() string { return "" }
func (s *Store) Close() error { return nil }
func (s *Store) LoadUsageState(_ context.Context) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *Store) SaveUsageState(_ context.Context, _ []byte) error { return nil }
func (s *Store) LoadCache(_ context.Context, _, _ string) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *Store) SaveCache(_ context.Context, _, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (s *Store) DeleteCache(_ context.Context, _, _ string) error { return nil }
func (s *Store) Load(_ context.Context) (map[string]coreauth.AuthRuntimeState, error) {
	return nil, nil
}
func (s *Store) Save(_ context.Context, _ string, _ coreauth.AuthRuntimeState) error { return nil }
func (s *Store) Delete(_ context.Context, _ string) error                            { return nil }
