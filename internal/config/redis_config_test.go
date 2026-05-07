package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_RedisDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.Redis.Enabled {
		t.Fatal("redis enabled by default")
	}
	if cfg.Redis.Addr != "127.0.0.1:6379" {
		t.Fatalf("redis addr = %q, want default", cfg.Redis.Addr)
	}
	if cfg.Redis.KeyPrefix != "cliproxyapi" {
		t.Fatalf("redis key prefix = %q, want default", cfg.Redis.KeyPrefix)
	}
}

func TestLoadConfigOptional_RedisSanitizesPrefixAndDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	input := []byte(`
port: 8317
redis:
  enabled: true
  addr: " localhost:6379 "
  db: -1
  key-prefix: " :custom-prefix: "
`)
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if !cfg.Redis.Enabled {
		t.Fatal("redis enabled = false, want true")
	}
	if cfg.Redis.Addr != "localhost:6379" {
		t.Fatalf("redis addr = %q, want trimmed", cfg.Redis.Addr)
	}
	if cfg.Redis.DB != 0 {
		t.Fatalf("redis db = %d, want 0", cfg.Redis.DB)
	}
	if cfg.Redis.KeyPrefix != "custom-prefix" {
		t.Fatalf("redis key prefix = %q, want sanitized", cfg.Redis.KeyPrefix)
	}
}
