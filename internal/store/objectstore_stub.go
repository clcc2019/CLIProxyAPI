//go:build !has_minio

// Package store — object (S3/Minio) stub.
//
// When `has_minio` is NOT set, this stub satisfies the type surface used by
// cmd/server/config_loader.go and auth handlers. NewObjectTokenStore returns
// a "not compiled in" sentinel error that gives operators the rebuild
// instruction they need.
package store

import (
	"context"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// ObjectStoreConfig mirrors the real struct so code outside this package
// compiles regardless of tags.
type ObjectStoreConfig struct {
	Endpoint   string
	Bucket     string
	AccessKey  string
	SecretKey  string
	Region     string
	LocalRoot  string
	UseSSL     bool
	PathStyle  bool
	ConfigKey  string
	AuthPrefix string
}

// ObjectTokenStore is a no-op placeholder for the Minio-backed store.
type ObjectTokenStore struct{}

func NewObjectTokenStore(_ ObjectStoreConfig) (*ObjectTokenStore, error) {
	return nil, errBackendNotCompiled("object (minio/s3)", "has_minio")
}

func (s *ObjectTokenStore) SetBaseDir(_ string) {}
func (s *ObjectTokenStore) ConfigPath() string  { return "" }
func (s *ObjectTokenStore) AuthDir() string     { return "" }
func (s *ObjectTokenStore) Bootstrap(_ context.Context, _ string) error {
	return errBackendNotCompiled("object (minio/s3)", "has_minio")
}
func (s *ObjectTokenStore) Save(_ context.Context, _ *cliproxyauth.Auth) (string, error) {
	return "", errBackendNotCompiled("object (minio/s3)", "has_minio")
}
func (s *ObjectTokenStore) List(_ context.Context) ([]*cliproxyauth.Auth, error) {
	return nil, errBackendNotCompiled("object (minio/s3)", "has_minio")
}
func (s *ObjectTokenStore) Delete(_ context.Context, _ string) error {
	return errBackendNotCompiled("object (minio/s3)", "has_minio")
}
func (s *ObjectTokenStore) PersistAuthFiles(_ context.Context, _ string, _ ...string) error {
	return errBackendNotCompiled("object (minio/s3)", "has_minio")
}
func (s *ObjectTokenStore) PersistConfig(_ context.Context) error {
	return errBackendNotCompiled("object (minio/s3)", "has_minio")
}
