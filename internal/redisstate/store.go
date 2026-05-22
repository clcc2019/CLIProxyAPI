//go:build !no_redis

// Package redisstate provides a Redis-backed state store. Redis is compiled
// into the default binary; use the no_redis build tag only for explicitly
// size-constrained builds that do not need Redis persistence.
package redisstate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	defaultAddr      = "127.0.0.1:6379"
	defaultKeyPrefix = "cliproxyapi"
)

type Store struct {
	client    *redis.Client
	keyPrefix string
	addr      string
}

func Available() bool { return true }

func New(ctx context.Context, cfg config.RedisConfig) (*Store, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	opts, err := redisOptions(cfg)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)
	if errPing := client.Ping(ctx).Err(); errPing != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", errPing)
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.KeyPrefix), ":")
	if prefix == "" {
		prefix = defaultKeyPrefix
	}
	return &Store{
		client:    client,
		keyPrefix: prefix,
		addr:      opts.Addr,
	}, nil
}

func redisOptions(cfg config.RedisConfig) (*redis.Options, error) {
	var opts *redis.Options
	if rawURL := strings.TrimSpace(cfg.URL); rawURL != "" {
		parsed, err := redis.ParseURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("parse redis url: %w", err)
		}
		opts = parsed
	} else {
		addr := strings.TrimSpace(cfg.Addr)
		if addr == "" {
			addr = defaultAddr
		}
		db := cfg.DB
		if db < 0 {
			db = 0
		}
		opts = &redis.Options{
			Addr:     addr,
			Username: strings.TrimSpace(cfg.Username),
			Password: cfg.Password,
			DB:       db,
		}
	}

	// Apply explicit pool / timeout settings on top of the parsed options so
	// that operators can override library defaults regardless of which form
	// (URL or addr) they used to configure Redis. Library defaults
	// (PoolSize=10×GOMAXPROCS, ReadTimeout=3s, etc.) work poorly in
	// CPU-quota'd containers; surface the knobs.
	if cfg.PoolSize > 0 {
		opts.PoolSize = cfg.PoolSize
	}
	if cfg.MinIdleConns > 0 {
		opts.MinIdleConns = cfg.MinIdleConns
	}
	if cfg.DialTimeoutMs > 0 {
		opts.DialTimeout = time.Duration(cfg.DialTimeoutMs) * time.Millisecond
	}
	if cfg.ReadTimeoutMs > 0 {
		opts.ReadTimeout = time.Duration(cfg.ReadTimeoutMs) * time.Millisecond
	}
	if cfg.WriteTimeoutMs > 0 {
		opts.WriteTimeout = time.Duration(cfg.WriteTimeoutMs) * time.Millisecond
	}
	if cfg.PoolTimeoutMs > 0 {
		opts.PoolTimeout = time.Duration(cfg.PoolTimeoutMs) * time.Millisecond
	}
	if cfg.MaxRetries != 0 {
		// MaxRetries=-1 disables retries in go-redis; preserve that semantic.
		opts.MaxRetries = cfg.MaxRetries
	}

	return opts, nil
}

func (s *Store) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

func (s *Store) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *Store) LoadUsageState(ctx context.Context) ([]byte, bool, error) {
	if s == nil || s.client == nil {
		return nil, false, nil
	}
	data, err := s.client.Get(ctx, s.key("usage", "statistics")).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (s *Store) SaveUsageState(ctx context.Context, data []byte) error {
	if s == nil || s.client == nil || len(data) == 0 {
		return nil
	}
	return s.client.Set(ctx, s.key("usage", "statistics"), data, 0).Err()
}

func (s *Store) LoadCache(ctx context.Context, namespace, cacheKey string) ([]byte, bool, error) {
	if s == nil || s.client == nil {
		return nil, false, nil
	}
	key := s.cacheKey(namespace, cacheKey)
	if key == "" {
		return nil, false, nil
	}
	data, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (s *Store) SaveCache(ctx context.Context, namespace, cacheKey string, data []byte, ttl time.Duration) error {
	if s == nil || s.client == nil || len(data) == 0 {
		return nil
	}
	key := s.cacheKey(namespace, cacheKey)
	if key == "" {
		return nil
	}
	if ttl < 0 {
		ttl = 0
	}
	return s.client.Set(ctx, key, data, ttl).Err()
}

func (s *Store) DeleteCache(ctx context.Context, namespace, cacheKey string) error {
	if s == nil || s.client == nil {
		return nil
	}
	key := s.cacheKey(namespace, cacheKey)
	if key == "" {
		return nil
	}
	return s.client.Del(ctx, key).Err()
}

func (s *Store) Load(ctx context.Context) (map[string]coreauth.AuthRuntimeState, error) {
	out := make(map[string]coreauth.AuthRuntimeState)
	if s == nil || s.client == nil {
		return out, nil
	}
	values, err := s.client.HGetAll(ctx, s.key("auth", "runtime")).Result()
	if err != nil {
		return nil, err
	}
	for authID, raw := range values {
		authID = strings.TrimSpace(authID)
		if authID == "" || raw == "" {
			continue
		}
		var state coreauth.AuthRuntimeState
		if errUnmarshal := json.Unmarshal([]byte(raw), &state); errUnmarshal != nil {
			return nil, fmt.Errorf("unmarshal auth runtime state %q: %w", authID, errUnmarshal)
		}
		out[authID] = state
	}
	return out, nil
}

func (s *Store) Save(ctx context.Context, authID string, state coreauth.AuthRuntimeState) error {
	if s == nil || s.client == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal auth runtime state: %w", err)
	}
	return s.client.HSet(ctx, s.key("auth", "runtime"), authID, data).Err()
}

func (s *Store) Delete(ctx context.Context, authID string) error {
	if s == nil || s.client == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	return s.client.HDel(ctx, s.key("auth", "runtime"), authID).Err()
}

func (s *Store) cacheKey(namespace, cacheKey string) string {
	namespace = strings.Trim(strings.TrimSpace(namespace), ":")
	cacheKey = strings.Trim(strings.TrimSpace(cacheKey), ":")
	if namespace == "" || cacheKey == "" {
		return ""
	}
	return s.key("cache", namespace, cacheKey)
}

func (s *Store) key(parts ...string) string {
	clean := make([]string, 0, len(parts)+1)
	prefix := strings.Trim(strings.TrimSpace(s.keyPrefix), ":")
	if prefix == "" {
		prefix = defaultKeyPrefix
	}
	clean = append(clean, prefix)
	for _, part := range parts {
		part = strings.Trim(strings.TrimSpace(part), ":")
		if part != "" {
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, ":")
}
