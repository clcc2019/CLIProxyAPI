//go:build !has_git

// Package store — git stub.
//
// When `has_git` is NOT set, go-git/v6 (the single largest optional
// dependency, ~5 MB) is excluded from the binary. This stub keeps the
// public type identifiers and method set so cmd/server/config_loader.go
// continues to compile regardless of the build profile.
package store

import (
	"context"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// GitTokenStore is a no-op placeholder for the git-backed token store.
type GitTokenStore struct{}

// NewGitTokenStore returns a stub. EnsureRepository (the first real
// method called from config_loader) surfaces the not-compiled-in error
// so operators see a clear message.
func NewGitTokenStore(_, _, _, _ string) *GitTokenStore {
	return &GitTokenStore{}
}

func (s *GitTokenStore) SetBaseDir(_ string) {}
func (s *GitTokenStore) AuthDir() string     { return "" }
func (s *GitTokenStore) ConfigPath() string  { return "" }
func (s *GitTokenStore) EnsureRepository() error {
	return errBackendNotCompiled("git", "has_git")
}
func (s *GitTokenStore) Save(_ context.Context, _ *cliproxyauth.Auth) (string, error) {
	return "", errBackendNotCompiled("git", "has_git")
}
func (s *GitTokenStore) List(_ context.Context) ([]*cliproxyauth.Auth, error) {
	return nil, errBackendNotCompiled("git", "has_git")
}
func (s *GitTokenStore) Delete(_ context.Context, _ string) error {
	return errBackendNotCompiled("git", "has_git")
}
func (s *GitTokenStore) PersistAuthFiles(_ context.Context, _ string, _ ...string) error {
	return errBackendNotCompiled("git", "has_git")
}

// PersistConfig is called by config_loader after each config write; stub
// just reports not-compiled-in.
func (s *GitTokenStore) PersistConfig(_ context.Context) error {
	return errBackendNotCompiled("git", "has_git")
}
