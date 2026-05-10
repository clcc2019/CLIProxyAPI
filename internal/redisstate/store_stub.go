//go:build !has_redis

// Package redisstate — redis stub.
//
// The default slim build excludes the redis/go-redis driver (~2.1 MB of
// binary size). This file keeps the public API surface stable so
// sdk/cliproxy/service.go compiles whether or not Redis support is
// included; `New` returns a not-compiled-in error so operators see a
// clear message if they configured Redis but forgot the has_redis tag.
package redisstate

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Store is a no-op placeholder when `has_redis` is not set.
type Store struct{}

// New always errors in the stub — callers see an actionable message and
// the service continues in in-memory-state mode.
func New(_ context.Context, _ config.RedisConfig) (*Store, error) {
	return nil, fmt.Errorf(
		"redis state store is not compiled in; rebuild with -tags=has_redis (or the slim Makefile target) to enable it",
	)
}

func (s *Store) Addr() string { return "" }
func (s *Store) Close() error { return nil }
func (s *Store) LoadUsageState(_ context.Context) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *Store) SaveUsageState(_ context.Context, _ []byte) error { return nil }
func (s *Store) Load(_ context.Context) (map[string]coreauth.AuthRuntimeState, error) {
	return nil, nil
}
func (s *Store) Save(_ context.Context, _ string, _ coreauth.AuthRuntimeState) error { return nil }
func (s *Store) Delete(_ context.Context, _ string) error                            { return nil }
