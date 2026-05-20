//go:build has_redis

package redisstate

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestRedisOptions_AppliesPoolAndTimeoutOverrides(t *testing.T) {
	cfg := config.RedisConfig{
		Addr:           "127.0.0.1:6379",
		Username:       "u",
		Password:       "p",
		DB:             2,
		PoolSize:       64,
		MinIdleConns:   8,
		DialTimeoutMs:  1500,
		ReadTimeoutMs:  2500,
		WriteTimeoutMs: 3500,
		PoolTimeoutMs:  4500,
		MaxRetries:     5,
	}
	opts, err := redisOptions(cfg)
	if err != nil {
		t.Fatalf("redisOptions: %v", err)
	}
	if opts.PoolSize != 64 {
		t.Errorf("PoolSize=%d, want 64", opts.PoolSize)
	}
	if opts.MinIdleConns != 8 {
		t.Errorf("MinIdleConns=%d, want 8", opts.MinIdleConns)
	}
	if opts.DialTimeout != 1500*time.Millisecond {
		t.Errorf("DialTimeout=%v, want 1.5s", opts.DialTimeout)
	}
	if opts.ReadTimeout != 2500*time.Millisecond {
		t.Errorf("ReadTimeout=%v, want 2.5s", opts.ReadTimeout)
	}
	if opts.WriteTimeout != 3500*time.Millisecond {
		t.Errorf("WriteTimeout=%v, want 3.5s", opts.WriteTimeout)
	}
	if opts.PoolTimeout != 4500*time.Millisecond {
		t.Errorf("PoolTimeout=%v, want 4.5s", opts.PoolTimeout)
	}
	if opts.MaxRetries != 5 {
		t.Errorf("MaxRetries=%d, want 5", opts.MaxRetries)
	}
	if opts.DB != 2 {
		t.Errorf("DB=%d, want 2", opts.DB)
	}
	if opts.Addr != "127.0.0.1:6379" {
		t.Errorf("Addr=%q, want 127.0.0.1:6379", opts.Addr)
	}
}

func TestRedisOptions_PoolFieldsLayerOnTopOfURL(t *testing.T) {
	// URL-based config also needs to receive pool overrides; otherwise
	// operators using `redis://` form would be stuck on library defaults.
	cfg := config.RedisConfig{
		URL:           "redis://localhost:6379/3",
		PoolSize:      32,
		ReadTimeoutMs: 1000,
	}
	opts, err := redisOptions(cfg)
	if err != nil {
		t.Fatalf("redisOptions: %v", err)
	}
	if opts.DB != 3 {
		t.Errorf("DB=%d, want 3 (from URL)", opts.DB)
	}
	if opts.PoolSize != 32 {
		t.Errorf("PoolSize=%d, want 32 (override)", opts.PoolSize)
	}
	if opts.ReadTimeout != time.Second {
		t.Errorf("ReadTimeout=%v, want 1s (override)", opts.ReadTimeout)
	}
}

func TestRedisOptions_ZeroValuesPreserveDefaults(t *testing.T) {
	cfg := config.RedisConfig{Addr: "127.0.0.1:6379"}
	opts, err := redisOptions(cfg)
	if err != nil {
		t.Fatalf("redisOptions: %v", err)
	}
	// All-zero pool fields should leave go-redis defaults in place. The
	// library default for PoolSize is 0 in the struct (it computes the real
	// value lazily inside NewClient), so we only check that we didn't write a
	// non-zero override.
	if opts.PoolSize != 0 {
		t.Errorf("PoolSize=%d, want 0 (library default)", opts.PoolSize)
	}
	if opts.ReadTimeout != 0 {
		t.Errorf("ReadTimeout=%v, want 0 (library default)", opts.ReadTimeout)
	}
}

func TestRedisOptions_MaxRetriesNegativeOneDisables(t *testing.T) {
	cfg := config.RedisConfig{Addr: "127.0.0.1:6379", MaxRetries: -1}
	opts, err := redisOptions(cfg)
	if err != nil {
		t.Fatalf("redisOptions: %v", err)
	}
	if opts.MaxRetries != -1 {
		t.Errorf("MaxRetries=%d, want -1 (disabled)", opts.MaxRetries)
	}
}
