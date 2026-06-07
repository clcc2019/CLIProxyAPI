package config

import (
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

func (cfg *Config) normalizeLoadedConfig(configFile string) error {
	if cfg == nil {
		return nil
	}

	// Hash remote management key if plaintext is detected (nested).
	// We consider a value to be already hashed if it looks like a bcrypt hash ($2a$, $2b$, or $2y$ prefix).
	if cfg.RemoteManagement.SecretKey != "" && !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
		hashed, errHash := hashSecret(cfg.RemoteManagement.SecretKey)
		if errHash != nil {
			return fmt.Errorf("failed to hash remote management key: %w", errHash)
		}
		cfg.RemoteManagement.SecretKey = hashed

		// Persist the hashed value back to the config file to avoid re-hashing on next startup.
		// Preserve YAML comments and ordering; update only the nested key.
		_ = SaveConfigPreserveCommentsUpdateNestedScalar(configFile, []string{"remote-management", "secret-key"}, hashed)
	}

	cfg.Pprof.Addr = strings.TrimSpace(cfg.Pprof.Addr)
	if cfg.Pprof.Addr == "" {
		cfg.Pprof.Addr = DefaultPprofAddr
	}

	if cfg.LogsMaxTotalSizeMB < 0 {
		cfg.LogsMaxTotalSizeMB = 0
	}

	if cfg.ErrorLogsMaxFiles < 0 {
		cfg.ErrorLogsMaxFiles = 10
	}

	if cfg.RedisUsageQueueRetentionSeconds <= 0 {
		cfg.RedisUsageQueueRetentionSeconds = 60
	} else if cfg.RedisUsageQueueRetentionSeconds > 3600 {
		log.WithField("value", cfg.RedisUsageQueueRetentionSeconds).Warn("redis-usage-queue-retention-seconds too large; clamping to 3600")
		cfg.RedisUsageQueueRetentionSeconds = 3600
	}
	cfg.Redis.URL = strings.TrimSpace(cfg.Redis.URL)
	cfg.Redis.Addr = strings.TrimSpace(cfg.Redis.Addr)
	if cfg.Redis.Addr == "" {
		cfg.Redis.Addr = "127.0.0.1:6379"
	}
	cfg.Redis.Username = strings.TrimSpace(cfg.Redis.Username)
	cfg.Redis.KeyPrefix = strings.Trim(strings.TrimSpace(cfg.Redis.KeyPrefix), ":")
	if cfg.Redis.KeyPrefix == "" {
		cfg.Redis.KeyPrefix = "cliproxyapi"
	}
	if cfg.Redis.DB < 0 {
		cfg.Redis.DB = 0
	}
	cfg.SanitizeProxyPool()

	if cfg.MaxRetryCredentials < 0 {
		cfg.MaxRetryCredentials = 0
	}

	// Sanitize client-facing API key configuration and keep legacy string entries compatible.
	cfg.SanitizeClientAPIKeys()

	cfg.ModelPrices = NormalizeModelPrices(cfg.ModelPrices)

	// Sanitize Codex keys: drop entries without base-url.
	cfg.SanitizeCodexKeys()

	// Sanitize Codex header defaults.
	cfg.SanitizeCodexHeaderDefaults()

	// Sanitize Claude header defaults.
	cfg.SanitizeClaudeHeaderDefaults()

	// Sanitize Claude key headers.
	cfg.SanitizeClaudeKeys()

	// Sanitize OpenAI compatibility providers: drop entries without base-url.
	cfg.SanitizeOpenAICompatibility()

	// Normalize OAuth provider model exclusion map.
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)

	// Normalize global OAuth model name aliases.
	cfg.SanitizeOAuthModelAlias()

	// Validate raw payload rules and drop invalid entries.
	cfg.SanitizePayloadRules()

	return nil
}
