package api

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type serverConfigUpdate struct {
	server *Server
	old    *config.Config
	next   *config.Config
}

func (update serverConfigUpdate) applyObservability() {
	s := update.server
	oldCfg := update.old
	cfg := update.next

	previousRequestLog := false
	if oldCfg != nil {
		previousRequestLog = oldCfg.RequestLog
	}
	if s.requestLogger != nil && (oldCfg == nil || previousRequestLog != cfg.RequestLog) {
		if s.loggerToggle != nil {
			s.loggerToggle(cfg.RequestLog)
		} else if toggler, ok := s.requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggler.SetEnabled(cfg.RequestLog)
		}
	}
	if s.requestAccessLogEnabled != nil && (oldCfg == nil || previousRequestLog != cfg.RequestLog) {
		s.requestAccessLogEnabled.Store(cfg.RequestLog)
	}
	if oldCfg == nil || oldCfg.Home.Enabled != cfg.Home.Enabled {
		if setter, ok := s.requestLogger.(interface{ SetHomeEnabled(bool) }); ok {
			setter.SetHomeEnabled(cfg.Home.Enabled)
		}
	}
	if oldCfg == nil || oldCfg.LoggingToFile != cfg.LoggingToFile || oldCfg.LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB {
		if err := logging.ConfigureLogOutput(cfg); err != nil {
			log.Errorf("failed to reconfigure log output: %v", err)
		}
	}
	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled {
		usage.SetStatisticsEnabled(cfg.UsageStatisticsEnabled)
	}
	if oldCfg == nil || oldCfg.RedisUsageQueueRetentionSeconds != cfg.RedisUsageQueueRetentionSeconds {
		redisqueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	}
	if oldCfg == nil || oldCfg.UsageDetailRetentionLimit != cfg.UsageDetailRetentionLimit {
		usage.SetDetailRetentionLimit(cfg.UsageDetailRetentionLimit)
	}
	if oldCfg == nil || !reflect.DeepEqual(oldCfg.ModelPrices, cfg.ModelPrices) {
		usage.SetClientAPIKeyQuotaModelPrices(cfg.ModelPrices)
	}
	if s.requestLogger != nil && (oldCfg == nil || oldCfg.ErrorLogsMaxFiles != cfg.ErrorLogsMaxFiles) {
		if setter, ok := s.requestLogger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
			setter.SetErrorLogsMaxFiles(cfg.ErrorLogsMaxFiles)
		}
	}
	if oldCfg == nil || oldCfg.Debug != cfg.Debug {
		util.SetLogLevel(cfg)
	}
}

func (update serverConfigUpdate) applyAuthRuntime() {
	s := update.server
	oldCfg := update.old
	cfg := update.next
	if oldCfg == nil || oldCfg.DisableCooling != cfg.DisableCooling {
		cliproxyauth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	}
	if s.handlers != nil && s.handlers.AuthManager != nil {
		s.handlers.AuthManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials)
	}
}

func (update serverConfigUpdate) applyManagementRoutes() {
	s := update.server
	oldCfg := update.old
	cfg := update.next
	prevSecretEmpty := oldCfg == nil || oldCfg.RemoteManagement.SecretKey == ""
	newSecretEmpty := cfg.RemoteManagement.SecretKey == ""
	localPasswordConfigured := strings.TrimSpace(s.localPassword) != ""
	if s.envManagementSecret || localPasswordConfigured {
		s.registerManagementRoutes()
		if s.managementRoutesEnabled.CompareAndSwap(false, true) {
			if s.envManagementSecret {
				log.Info("management routes enabled via MANAGEMENT_PASSWORD")
			} else {
				log.Info("management routes enabled via local management password")
			}
		} else {
			s.managementRoutesEnabled.Store(true)
		}
	} else {
		switch {
		case prevSecretEmpty && !newSecretEmpty:
			s.registerManagementRoutes()
			if s.managementRoutesEnabled.CompareAndSwap(false, true) {
				log.Info("management routes enabled after secret key update")
			} else {
				s.managementRoutesEnabled.Store(true)
			}
		case !prevSecretEmpty && newSecretEmpty:
			if s.managementRoutesEnabled.CompareAndSwap(true, false) {
				log.Info("management routes disabled after secret key removal")
			} else {
				s.managementRoutesEnabled.Store(false)
			}
		default:
			s.managementRoutesEnabled.Store(!newSecretEmpty)
		}
	}
	redisqueue.SetEnabled(s.managementRoutesEnabled.Load() || cfg.Home.Enabled)
}

func (update serverConfigUpdate) applyRuntimeSnapshot() {
	s := update.server
	oldCfg := update.old
	cfg := update.next
	if err := s.applyAccessConfig(oldCfg, cfg); err != nil {
		log.Errorf("failed to apply access provider config: %v", err)
	}
	s.cfg = cfg
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	if oldCfg != nil && s.wsAuthChanged != nil && oldCfg.WebsocketAuth != cfg.WebsocketAuth {
		s.wsAuthChanged(oldCfg.WebsocketAuth, cfg.WebsocketAuth)
	}
	s.oldConfigYaml, _ = yaml.Marshal(cfg)

	if s.handlers != nil {
		s.handlers.UpdateClients(&cfg.SDKConfig)
	}
	if s.mgmt != nil {
		s.mgmt.SetConfig(cfg)
		if s.handlers != nil {
			s.mgmt.SetAuthManager(s.handlers.AuthManager)
		}
	}
}

func (update serverConfigUpdate) logClientSummary() {
	cfg := update.next
	authEntries := 0
	if !cfg.Home.Enabled {
		tokenStore := sdkAuth.GetTokenStore()
		if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(cfg.AuthDir)
		}
		authEntries = util.CountAuthFiles(context.Background(), tokenStore)
	}
	claudeAPIKeyCount := len(cfg.ClaudeKey)
	codexAPIKeyCount := len(cfg.CodexKey)
	openAICompatCount := 0
	for i := range cfg.OpenAICompatibility {
		openAICompatCount += len(cfg.OpenAICompatibility[i].APIKeyEntries)
	}
	total := authEntries + claudeAPIKeyCount + codexAPIKeyCount + openAICompatCount
	fmt.Printf("server clients and configuration updated: %d clients (%d auth entries + %d Claude API keys + %d Codex keys + %d OpenAI-compat)\n",
		total,
		authEntries,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		openAICompatCount,
	)
}

func (s *Server) applyAccessConfig(oldCfg, newCfg *config.Config) error {
	if s == nil || s.accessManager == nil || newCfg == nil {
		return nil
	}
	_, err := access.ApplyAccessProviders(s.accessManager, oldCfg, newCfg)
	return err
}
