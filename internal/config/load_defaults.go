package config

func newConfigWithDefaults() Config {
	var cfg Config
	cfg.Host = "" // Default empty: binds to all interfaces (IPv4 + IPv6)
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.UsageDetailRetentionLimit = 100
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.Redis.Addr = "127.0.0.1:6379"
	cfg.Redis.KeyPrefix = "cliproxyapi"
	cfg.ProxyPool.StateStore = "redis"
	cfg.ProxyPool.ReleaseOnAuthDisabled = true
	cfg.DisableCooling = false
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	return cfg
}
