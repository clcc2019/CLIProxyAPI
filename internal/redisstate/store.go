//go:build !no_redis

// Package redisstate provides a Redis-backed state store. Redis is compiled
// into the default binary; use the no_redis build tag only for explicitly
// size-constrained builds that do not need Redis persistence.
package redisstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	defaultAddr      = "127.0.0.1:6379"
	defaultKeyPrefix = "cliproxyapi"
)

var acquireProxyLeaseScript = redis.NewScript(`
local now = tonumber(ARGV[2]) or 0
local function proxy_in_cooldown(proxy)
  local recover_at = tonumber(redis.call("HGET", KEYS[3], proxy) or "0")
  if recover_at and recover_at > 0 then
    if recover_at > now then
      return true
    end
    redis.call("HDEL", KEYS[3], proxy)
    redis.call("HDEL", KEYS[4], proxy)
  end
  return false
end
local existing = redis.call("HGET", KEYS[1], ARGV[1])
if existing and existing ~= "" then
  local existingAllowed = false
  for i = 3, #ARGV do
    if ARGV[i] == existing then
      existingAllowed = true
      if not proxy_in_cooldown(existing) then
        local owner = redis.call("HGET", KEYS[2], existing)
        if not owner or owner == "" or owner == ARGV[1] then
          redis.call("HSET", KEYS[2], existing, ARGV[1])
          return existing
        end
      end
    end
  end
  if not existingAllowed then
    local owner = redis.call("HGET", KEYS[2], existing)
    if owner == ARGV[1] then
      redis.call("HDEL", KEYS[2], existing)
    end
  end
  redis.call("HDEL", KEYS[1], ARGV[1])
end
for i = 3, #ARGV do
  local proxy = ARGV[i]
  if not proxy_in_cooldown(proxy) then
    local owner = redis.call("HGET", KEYS[2], proxy)
    if owner and owner ~= "" and owner ~= ARGV[1] then
      local ownerLease = redis.call("HGET", KEYS[1], owner)
      if ownerLease ~= proxy then
        redis.call("HDEL", KEYS[2], proxy)
        owner = nil
      end
    end
    if not owner or owner == "" or owner == ARGV[1] then
      redis.call("HSET", KEYS[1], ARGV[1], proxy)
      redis.call("HSET", KEYS[2], proxy, ARGV[1])
      return proxy
    end
  end
end
return ""
`)

var releaseProxyLeaseScript = redis.NewScript(`
local proxy = redis.call("HGET", KEYS[1], ARGV[1])
if proxy and proxy ~= "" then
  local owner = redis.call("HGET", KEYS[2], proxy)
  if owner == ARGV[1] then
    redis.call("HDEL", KEYS[2], proxy)
  end
end
redis.call("HDEL", KEYS[1], ARGV[1])
return proxy or ""
`)

var recordProxyLeaseFailureScript = redis.NewScript(`
local auth_id = ARGV[1]
local proxy = ARGV[2]
local threshold = tonumber(ARGV[3]) or 0
local recover_at = tonumber(ARGV[4]) or 0
if auth_id == "" or proxy == "" or threshold <= 0 then
  return {0, 0}
end
local lease = redis.call("HGET", KEYS[1], auth_id)
local owner = redis.call("HGET", KEYS[2], proxy)
if lease ~= proxy or owner ~= auth_id then
  return {0, 0}
end
local failures = redis.call("HINCRBY", KEYS[3], proxy, 1)
if failures >= threshold then
  redis.call("HDEL", KEYS[3], proxy)
  redis.call("HSET", KEYS[4], proxy, recover_at)
  redis.call("HDEL", KEYS[1], auth_id)
  redis.call("HDEL", KEYS[2], proxy)
  return {failures, recover_at}
end
return {failures, 0}
`)

var seedClientAPIKeyQuotaScript = redis.NewScript(`
local current = tonumber(redis.call("GET", KEYS[1]) or "0") or 0
local value = tonumber(ARGV[1]) or 0
local expire_at = tonumber(ARGV[2]) or 0
if value > current then
  redis.call("SET", KEYS[1], ARGV[1])
  if expire_at > 0 then
    redis.call("EXPIREAT", KEYS[1], expire_at)
  end
  return 1
end
if expire_at > 0 and redis.call("TTL", KEYS[1]) < 0 then
  redis.call("EXPIREAT", KEYS[1], expire_at)
end
return 0
`)

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

func (s *Store) LoadClientAPIKeyQuotaUsage(ctx context.Context, apiKey string, now time.Time) (internalusage.ClientAPIKeyQuotaUsage, bool, error) {
	if s == nil || s.client == nil {
		return internalusage.ClientAPIKeyQuotaUsage{}, false, nil
	}
	apiKeyHash := clientAPIKeyQuotaHash(apiKey)
	if apiKeyHash == "" {
		return internalusage.ClientAPIKeyQuotaUsage{}, false, nil
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	keys := []string{
		s.clientAPIKeyQuotaKey(apiKeyHash, "total", ""),
		s.clientAPIKeyQuotaKey(apiKeyHash, "daily", now.Format("2006-01-02")),
		s.clientAPIKeyQuotaKey(apiKeyHash, "monthly", now.Format("2006-01")),
	}
	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return internalusage.ClientAPIKeyQuotaUsage{}, false, err
	}
	var usage internalusage.ClientAPIKeyQuotaUsage
	found := false
	if cost, ok := redisFloat(values[0]); ok {
		usage.TotalCost = cost
		found = true
	}
	if cost, ok := redisFloat(values[1]); ok {
		usage.DailyCost = cost
		found = true
	}
	if cost, ok := redisFloat(values[2]); ok {
		usage.MonthlyCost = cost
		found = true
	}
	return usage, found, nil
}

func (s *Store) AddClientAPIKeyQuotaUsage(ctx context.Context, apiKey string, timestamp time.Time, cost float64) error {
	if s == nil || s.client == nil || cost <= 0 {
		return nil
	}
	apiKeyHash := clientAPIKeyQuotaHash(apiKey)
	if apiKeyHash == "" {
		return nil
	}
	timestamp = timestamp.UTC()
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	day := timestamp.Format("2006-01-02")
	month := timestamp.Format("2006-01")
	dailyExpireAt := clientAPIKeyQuotaDailyExpireAt(timestamp)
	monthlyExpireAt := clientAPIKeyQuotaMonthlyExpireAt(timestamp)

	pipe := s.client.Pipeline()
	pipe.IncrByFloat(ctx, s.clientAPIKeyQuotaKey(apiKeyHash, "total", ""), cost)
	daily := s.clientAPIKeyQuotaKey(apiKeyHash, "daily", day)
	monthly := s.clientAPIKeyQuotaKey(apiKeyHash, "monthly", month)
	pipe.IncrByFloat(ctx, daily, cost)
	pipe.ExpireAt(ctx, daily, dailyExpireAt)
	pipe.IncrByFloat(ctx, monthly, cost)
	pipe.ExpireAt(ctx, monthly, monthlyExpireAt)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) SeedClientAPIKeyQuotaState(ctx context.Context, state internalusage.ClientAPIKeyQuotaState) error {
	if s == nil || s.client == nil || state.IsZero() {
		return nil
	}
	ops := 0
	seed := func(key string, cost float64, expireAt time.Time) error {
		if key == "" || cost <= 0 {
			return nil
		}
		expireUnix := int64(0)
		if !expireAt.IsZero() {
			expireUnix = expireAt.UTC().Unix()
		}
		if err := seedClientAPIKeyQuotaScript.Run(ctx, s.client, []string{key}, strconv.FormatFloat(cost, 'f', -1, 64), expireUnix).Err(); err != nil {
			return err
		}
		ops++
		return nil
	}
	for apiKey, cost := range state.Total {
		apiKeyHash := clientAPIKeyQuotaHash(apiKey)
		if err := seed(s.clientAPIKeyQuotaKey(apiKeyHash, "total", ""), cost, time.Time{}); err != nil {
			return err
		}
	}
	for apiKey, buckets := range state.Daily {
		apiKeyHash := clientAPIKeyQuotaHash(apiKey)
		for bucket, cost := range buckets {
			if expireAt, ok := clientAPIKeyQuotaDailyBucketExpireAt(bucket); ok {
				if err := seed(s.clientAPIKeyQuotaKey(apiKeyHash, "daily", bucket), cost, expireAt); err != nil {
					return err
				}
			}
		}
	}
	for apiKey, buckets := range state.Monthly {
		apiKeyHash := clientAPIKeyQuotaHash(apiKey)
		for bucket, cost := range buckets {
			if expireAt, ok := clientAPIKeyQuotaMonthlyBucketExpireAt(bucket); ok {
				if err := seed(s.clientAPIKeyQuotaKey(apiKeyHash, "monthly", bucket), cost, expireAt); err != nil {
					return err
				}
			}
		}
	}
	if ops == 0 {
		return nil
	}
	return nil
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

func (s *Store) AcquireProxyLease(ctx context.Context, authID string, proxyURLs []string) (coreauth.ProxyLease, bool, error) {
	if s == nil || s.client == nil {
		return coreauth.ProxyLease{}, false, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" || len(proxyURLs) == 0 {
		return coreauth.ProxyLease{}, false, nil
	}
	args := make([]any, 0, len(proxyURLs)+2)
	args = append(args, authID)
	args = append(args, time.Now().UTC().UnixMilli())
	for _, proxyURL := range proxyURLs {
		proxyURL = strings.TrimSpace(proxyURL)
		if proxyURL != "" {
			args = append(args, proxyURL)
		}
	}
	if len(args) == 2 {
		return coreauth.ProxyLease{}, false, nil
	}
	result, err := acquireProxyLeaseScript.Run(ctx, s.client, []string{
		s.key("proxy-pool", "leases"),
		s.key("proxy-pool", "reverse"),
		s.key("proxy-pool", "cooldown"),
		s.key("proxy-pool", "failures"),
	}, args...).Text()
	if err != nil {
		return coreauth.ProxyLease{}, false, err
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return coreauth.ProxyLease{}, false, nil
	}
	now := time.Now().UTC()
	return coreauth.ProxyLease{AuthID: authID, ProxyURL: result, AssignedAt: now, UpdatedAt: now}, true, nil
}

func (s *Store) ReleaseProxyLease(ctx context.Context, authID string) error {
	if s == nil || s.client == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	return releaseProxyLeaseScript.Run(ctx, s.client, []string{
		s.key("proxy-pool", "leases"),
		s.key("proxy-pool", "reverse"),
	}, authID).Err()
}

func (s *Store) ReconcileProxyLeases(ctx context.Context, activeAuthIDs []string, proxyURLs []string) error {
	if s == nil || s.client == nil {
		return nil
	}
	leaseKey := s.key("proxy-pool", "leases")
	reverseKey := s.key("proxy-pool", "reverse")
	failureKey := s.key("proxy-pool", "failures")
	cooldownKey := s.key("proxy-pool", "cooldown")
	leases, err := s.client.HGetAll(ctx, leaseKey).Result()
	if err != nil {
		return err
	}
	reverse, err := s.client.HGetAll(ctx, reverseKey).Result()
	if err != nil {
		return err
	}
	failures, err := s.client.HGetAll(ctx, failureKey).Result()
	if err != nil {
		return err
	}
	cooldowns, err := s.client.HGetAll(ctx, cooldownKey).Result()
	if err != nil {
		return err
	}
	active := make(map[string]struct{}, len(activeAuthIDs))
	for _, authID := range activeAuthIDs {
		authID = strings.TrimSpace(authID)
		if authID != "" {
			active[authID] = struct{}{}
		}
	}
	allowed := make(map[string]struct{}, len(proxyURLs))
	for _, proxyURL := range proxyURLs {
		proxyURL = strings.TrimSpace(proxyURL)
		if proxyURL != "" {
			allowed[proxyURL] = struct{}{}
		}
	}
	pipe := s.client.Pipeline()
	ops := 0
	for authID, proxyURL := range leases {
		authID = strings.TrimSpace(authID)
		proxyURL = strings.TrimSpace(proxyURL)
		_, activeOK := active[authID]
		_, allowedOK := allowed[proxyURL]
		if authID == "" || proxyURL == "" || !activeOK || !allowedOK {
			pipe.HDel(ctx, leaseKey, authID)
			ops++
			if reverse[proxyURL] == authID {
				pipe.HDel(ctx, reverseKey, proxyURL)
				ops++
			}
		}
	}
	for proxyURL, authID := range reverse {
		proxyURL = strings.TrimSpace(proxyURL)
		authID = strings.TrimSpace(authID)
		_, activeOK := active[authID]
		_, allowedOK := allowed[proxyURL]
		if proxyURL == "" || authID == "" || !activeOK || !allowedOK || strings.TrimSpace(leases[authID]) != proxyURL {
			pipe.HDel(ctx, reverseKey, proxyURL)
			ops++
		}
	}
	for proxyURL := range failures {
		proxyURL = strings.TrimSpace(proxyURL)
		if _, allowedOK := allowed[proxyURL]; proxyURL == "" || !allowedOK {
			pipe.HDel(ctx, failureKey, proxyURL)
			ops++
		}
	}
	for proxyURL := range cooldowns {
		proxyURL = strings.TrimSpace(proxyURL)
		if _, allowedOK := allowed[proxyURL]; proxyURL == "" || !allowedOK {
			pipe.HDel(ctx, cooldownKey, proxyURL)
			ops++
		}
	}
	if ops == 0 {
		return nil
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *Store) RecordProxyLeaseFailure(ctx context.Context, authID, proxyURL string, threshold int, cooldown time.Duration) (coreauth.ProxyLeaseFailure, error) {
	if s == nil || s.client == nil {
		return coreauth.ProxyLeaseFailure{}, nil
	}
	authID = strings.TrimSpace(authID)
	proxyURL = strings.TrimSpace(proxyURL)
	if authID == "" || proxyURL == "" || threshold <= 0 {
		return coreauth.ProxyLeaseFailure{}, nil
	}
	if cooldown <= 0 {
		cooldown = time.Minute
	}
	recoverAt := time.Now().UTC().Add(cooldown)
	raw, err := recordProxyLeaseFailureScript.Run(ctx, s.client, []string{
		s.key("proxy-pool", "leases"),
		s.key("proxy-pool", "reverse"),
		s.key("proxy-pool", "failures"),
		s.key("proxy-pool", "cooldown"),
	}, authID, proxyURL, threshold, recoverAt.UnixMilli()).Result()
	if err != nil {
		return coreauth.ProxyLeaseFailure{}, err
	}
	values, ok := raw.([]any)
	if !ok || len(values) < 2 {
		return coreauth.ProxyLeaseFailure{}, nil
	}
	failures, _ := redisInt(values[0])
	recoverAtMillis, _ := redisInt64(values[1])
	result := coreauth.ProxyLeaseFailure{
		ProxyURL: proxyURL,
		Failures: failures,
	}
	if recoverAtMillis > 0 {
		result.CooledDown = true
		result.RecoverAt = time.UnixMilli(recoverAtMillis).UTC()
	}
	return result, nil
}

func (s *Store) ClearProxyLeaseFailure(ctx context.Context, proxyURL string) error {
	if s == nil || s.client == nil {
		return nil
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	pipe := s.client.Pipeline()
	pipe.HDel(ctx, s.key("proxy-pool", "failures"), proxyURL)
	pipe.HDel(ctx, s.key("proxy-pool", "cooldown"), proxyURL)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) cacheKey(namespace, cacheKey string) string {
	namespace = strings.Trim(strings.TrimSpace(namespace), ":")
	cacheKey = strings.Trim(strings.TrimSpace(cacheKey), ":")
	if namespace == "" || cacheKey == "" {
		return ""
	}
	return s.key("cache", namespace, cacheKey)
}

func (s *Store) clientAPIKeyQuotaKey(apiKeyHash, scope, bucket string) string {
	apiKeyHash = strings.TrimSpace(apiKeyHash)
	scope = strings.TrimSpace(scope)
	bucket = strings.TrimSpace(bucket)
	if apiKeyHash == "" || scope == "" {
		return ""
	}
	if bucket == "" {
		return s.key("quota", "client-api-key", apiKeyHash, scope)
	}
	return s.key("quota", "client-api-key", apiKeyHash, scope, bucket)
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

func redisInt(value any) (int, bool) {
	out, ok := redisInt64(value)
	return int(out), ok
}

func redisInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case string:
		var out int64
		if _, err := fmt.Sscanf(v, "%d", &out); err != nil {
			return 0, false
		}
		return out, true
	default:
		return 0, false
	}
}

func redisFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		out, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return out, err == nil
	case []byte:
		out, err := strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
		return out, err == nil
	default:
		return 0, false
	}
}

func clientAPIKeyQuotaHash(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

func clientAPIKeyQuotaDailyExpireAt(timestamp time.Time) time.Time {
	timestamp = timestamp.UTC()
	dayStart := time.Date(timestamp.Year(), timestamp.Month(), timestamp.Day(), 0, 0, 0, 0, time.UTC)
	return dayStart.AddDate(0, 0, 3)
}

func clientAPIKeyQuotaMonthlyExpireAt(timestamp time.Time) time.Time {
	timestamp = timestamp.UTC()
	monthStart := time.Date(timestamp.Year(), timestamp.Month(), 1, 0, 0, 0, 0, time.UTC)
	return monthStart.AddDate(0, 3, 0)
}

func clientAPIKeyQuotaDailyBucketExpireAt(bucket string) (time.Time, bool) {
	ts, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(bucket), time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return ts.AddDate(0, 0, 3), true
}

func clientAPIKeyQuotaMonthlyBucketExpireAt(bucket string) (time.Time, bool) {
	ts, err := time.ParseInLocation("2006-01", strings.TrimSpace(bucket), time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return ts.AddDate(0, 3, 0), true
}
