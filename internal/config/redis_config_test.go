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

func TestLoadConfigOptional_ProxyPoolSanitizesProxies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	input := []byte(`
port: 8317
proxy-pool:
  enabled: true
  state-store: " Redis "
  proxy-failure-threshold: -1
  proxy-failure-cooldown: " 5m "
  proxies:
    - " http://proxy-a.example.com:8080 "
    - "http://proxy-a.example.com:8080"
    - ""
    - "http://proxy-b.example.com:8080"
`)
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if !cfg.ProxyPool.Enabled {
		t.Fatal("proxy pool enabled = false, want true")
	}
	if cfg.ProxyPool.StateStore != "redis" {
		t.Fatalf("state-store = %q, want redis", cfg.ProxyPool.StateStore)
	}
	if cfg.ProxyPool.ProxyFailureThreshold != 0 {
		t.Fatalf("proxy-failure-threshold = %d, want 0", cfg.ProxyPool.ProxyFailureThreshold)
	}
	if cfg.ProxyPool.ProxyFailureCooldown != "5m" {
		t.Fatalf("proxy-failure-cooldown = %q, want 5m", cfg.ProxyPool.ProxyFailureCooldown)
	}
	if got, want := len(cfg.ProxyPool.Proxies), 2; got != want {
		t.Fatalf("proxy count = %d, want %d: %#v", got, want, cfg.ProxyPool.Proxies)
	}
	if cfg.ProxyPool.Proxies[0] != "http://proxy-a.example.com:8080" || cfg.ProxyPool.Proxies[1] != "http://proxy-b.example.com:8080" {
		t.Fatalf("proxies = %#v", cfg.ProxyPool.Proxies)
	}
}

func TestLoadConfigOptional_ProxyPoolDisablesWithoutProxies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	input := []byte(`
port: 8317
proxy-pool:
  enabled: true
  proxies: []
`)
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.ProxyPool.Enabled {
		t.Fatal("proxy pool enabled = true, want false without proxies")
	}
}
