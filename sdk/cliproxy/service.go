// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/redisstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/wsrelay"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

// Service wraps the proxy server lifecycle so external programs can embed the CLI proxy.
// It manages the complete lifecycle including authentication, file watching, HTTP server,
// and integration with various AI service providers.
type Service struct {
	// cfg holds the current application configuration.
	cfg *config.Config

	// cfgMu protects concurrent access to the configuration.
	cfgMu sync.RWMutex

	// configPath is the path to the configuration file.
	configPath string

	// tokenProvider handles loading token-based clients.
	tokenProvider TokenClientProvider

	// apiKeyProvider handles loading API key-based clients.
	apiKeyProvider APIKeyClientProvider

	// watcherFactory creates file watcher instances.
	watcherFactory WatcherFactory

	// hooks provides lifecycle callbacks.
	hooks Hooks

	// serverOptions contains additional server configuration options.
	serverOptions []api.ServerOption

	// server is the HTTP API server instance.
	server *api.Server

	// pprofServer manages the optional pprof HTTP debug server.
	pprofServer *pprofServer

	// serverErr channel for server startup/shutdown errors.
	serverErr chan error

	// watcher handles file system monitoring.
	watcher *WatcherWrapper

	// watcherCancel cancels the watcher context.
	watcherCancel context.CancelFunc

	// authUpdates channel for authentication updates.
	authUpdates chan watcher.AuthUpdate

	// authQueueStop cancels the auth update queue processing.
	authQueueStop context.CancelFunc

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// shutdownOnce ensures shutdown is called only once.
	shutdownOnce sync.Once

	// wsGateway manages websocket Gemini providers.
	wsGateway *wsrelay.Manager

	// usagePersistence periodically snapshots usage statistics to disk.
	usagePersistence *usagePersistenceRunner

	// usagePersistenceBackend optionally mirrors usage state to an external store.
	usagePersistenceBackend usagePersistenceBackend

	// redisState owns the optional Redis client used for durable runtime state.
	redisState *redisstate.Store
}

// RegisterUsagePlugin registers a usage plugin on the global usage manager.
// This allows external code to monitor API usage and token consumption.
//
// Parameters:
//   - plugin: The usage plugin to register
func (s *Service) RegisterUsagePlugin(plugin usage.Plugin) {
	usage.RegisterPlugin(plugin)
}

// newDefaultAuthManager creates a default authentication manager with all supported providers.
func newDefaultAuthManager() *sdkAuth.Manager {
	return sdkAuth.NewManager(
		sdkAuth.GetTokenStore(),
		sdkAuth.NewGeminiAuthenticator(),
		sdkAuth.NewCodexAuthenticator(),
		sdkAuth.NewClaudeAuthenticator(),
		sdkAuth.NewKiroAuthenticator(),
	)
}

func (s *Service) syncCoreAutoRefresh() {
	if s == nil || s.coreManager == nil {
		return
	}
	interval := 15 * time.Minute
	if s.coreManager.StartAutoRefresh(context.Background(), interval) {
		log.Infof("core auth auto-refresh started (interval=%s)", interval)
		return
	}
	log.Info("core auth auto-refresh disabled")
}

func (s *Service) ensureAuthUpdateQueue(ctx context.Context) {
	if s == nil {
		return
	}
	if s.authUpdates == nil {
		s.authUpdates = make(chan watcher.AuthUpdate, 256)
	}
	if s.authQueueStop != nil {
		return
	}
	queueCtx, cancel := context.WithCancel(ctx)
	s.authQueueStop = cancel
	go s.consumeAuthUpdates(queueCtx)
}

func (s *Service) consumeAuthUpdates(ctx context.Context) {
	ctx = coreauth.WithSkipPersist(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-s.authUpdates:
			if !ok {
				return
			}
			s.handleAuthUpdate(ctx, update)
		labelDrain:
			for {
				select {
				case nextUpdate := <-s.authUpdates:
					s.handleAuthUpdate(ctx, nextUpdate)
				default:
					break labelDrain
				}
			}
		}
	}
}

func (s *Service) emitAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.watcher != nil && s.watcher.DispatchRuntimeAuthUpdate(update) {
		return
	}
	if s.authUpdates != nil {
		select {
		case s.authUpdates <- update:
			return
		default:
			log.Debugf("auth update queue saturated, applying inline action=%v id=%s", update.Action, update.ID)
		}
	}
	s.handleAuthUpdate(ctx, update)
}

func (s *Service) handleAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || s.coreManager == nil {
		return
	}
	switch update.Action {
	case watcher.AuthUpdateActionAdd, watcher.AuthUpdateActionModify:
		if update.Auth == nil || update.Auth.ID == "" {
			return
		}
		s.applyCoreAuthAddOrUpdate(ctx, update.Auth)
	case watcher.AuthUpdateActionDelete:
		id := update.ID
		if id == "" && update.Auth != nil {
			id = update.Auth.ID
		}
		if id == "" {
			return
		}
		s.applyCoreAuthRemoval(ctx, id)
	default:
		log.Debugf("received unknown auth update action: %v", update.Action)
	}
}

func (s *Service) ensureWebsocketGateway() {
	if s == nil {
		return
	}
	if s.wsGateway != nil {
		return
	}
	opts := wsrelay.Options{
		Path:           "/v1/ws",
		OnConnected:    s.wsOnConnected,
		OnDisconnected: s.wsOnDisconnected,
		LogDebugf:      log.Debugf,
		LogInfof:       log.Infof,
		LogWarnf:       log.Warnf,
	}
	s.wsGateway = wsrelay.NewManager(opts)
}

func (s *Service) wsOnConnected(channelID string) {
	if s == nil || channelID == "" {
		return
	}
	if !strings.HasPrefix(strings.ToLower(channelID), "aistudio-") {
		return
	}
	if s.coreManager != nil {
		if existing, ok := s.coreManager.GetByID(channelID); ok && existing != nil {
			if !existing.Disabled && existing.Status == coreauth.StatusActive {
				return
			}
		}
	}
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:         channelID,  // keep channel identifier as ID
		Provider:   "aistudio", // logical provider for switch routing
		Label:      channelID,  // display original channel id
		Status:     coreauth.StatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"email": channelID}, // metadata drives logging and usage tracking
	}
	log.Infof("websocket provider connected: %s", channelID)
	s.emitAuthUpdate(context.Background(), watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionAdd,
		ID:     auth.ID,
		Auth:   auth,
	})
}

func (s *Service) wsOnDisconnected(channelID string, reason error) {
	if s == nil || channelID == "" {
		return
	}
	if reason != nil {
		if strings.Contains(reason.Error(), "replaced by new connection") {
			log.Infof("websocket provider replaced: %s", channelID)
			return
		}
		log.Warnf("websocket provider disconnected: %s (%v)", channelID, reason)
	} else {
		log.Infof("websocket provider disconnected: %s", channelID)
	}
	ctx := context.Background()
	s.emitAuthUpdate(ctx, watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionDelete,
		ID:     channelID,
	})
}

func (s *Service) applyCoreAuthAddOrUpdate(ctx context.Context, auth *coreauth.Auth) {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" {
		return
	}
	auth = auth.Clone()
	s.ensureExecutorsForAuth(auth)

	// IMPORTANT: Update coreManager FIRST, before model registration.
	// This ensures that configuration changes (proxy_url, prefix, etc.) take effect
	// immediately for API calls, rather than waiting for model registration to complete.
	op := "register"
	var err error
	if existing, ok := s.coreManager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		if !existing.Disabled && existing.Status != coreauth.StatusDisabled && !auth.Disabled && auth.Status != coreauth.StatusDisabled {
			auth.LastRefreshedAt = existing.LastRefreshedAt
			auth.NextRefreshAfter = existing.NextRefreshAfter
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
		}
		op = "update"
		_, err = s.coreManager.Update(ctx, auth)
	} else {
		_, err = s.coreManager.Register(ctx, auth)
	}
	if err != nil {
		log.Errorf("failed to %s auth %s: %v", op, auth.ID, err)
		current, ok := s.coreManager.GetByID(auth.ID)
		if !ok || current.Disabled {
			GlobalModelRegistry().UnregisterClient(auth.ID)
			return
		}
		auth = current
	}

	// Register models after auth is updated in coreManager.
	// This operation may block on network calls, but the auth configuration
	// is already effective at this point.
	s.registerModelsForAuth(auth)
	s.coreManager.ReconcileRegistryModelStates(ctx, auth.ID)

	// Refresh the scheduler entry so that the auth's supportedModelSet is rebuilt
	// from the now-populated global model registry. Without this, newly added auths
	// have an empty supportedModelSet (because Register/Update upserts into the
	// scheduler before registerModelsForAuth runs) and are invisible to the scheduler.
	s.coreManager.RefreshSchedulerEntry(auth.ID)
}

func (s *Service) applyCoreAuthRemoval(ctx context.Context, id string) {
	if s == nil || id == "" {
		return
	}
	if s.coreManager == nil {
		return
	}
	GlobalModelRegistry().UnregisterClient(id)
	if existing, ok := s.coreManager.GetByID(id); ok && existing != nil {
		existing.Disabled = true
		existing.Status = coreauth.StatusDisabled
		if _, err := s.coreManager.Update(ctx, existing); err != nil {
			log.Errorf("failed to disable auth %s: %v", id, err)
		}
		if strings.EqualFold(strings.TrimSpace(existing.Provider), "codex") {
			executor.CloseCodexWebsocketSessionsForAuthID(existing.ID, "auth_removed")
			s.ensureExecutorsForAuth(existing)
		}
	}
}

func (s *Service) applyRetryConfig(cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	maxInterval := time.Duration(cfg.MaxRetryInterval) * time.Second
	s.coreManager.SetRetryConfig(cfg.RequestRetry, maxInterval, cfg.MaxRetryCredentials)
}

func (s *Service) configureRedisState(ctx context.Context) error {
	if s == nil || s.cfg == nil || !s.cfg.Redis.Enabled {
		return nil
	}
	store, err := redisstate.New(ctx, s.cfg.Redis)
	if err != nil {
		return fmt.Errorf("redis state storage: %w", err)
	}
	s.redisState = store
	s.usagePersistenceBackend = store
	if s.coreManager != nil {
		s.coreManager.SetRuntimeStateStore(store)
		if errLoad := s.coreManager.LoadRuntimeStates(ctx); errLoad != nil {
			_ = store.Close()
			s.redisState = nil
			s.usagePersistenceBackend = nil
			return fmt.Errorf("load redis auth runtime state: %w", errLoad)
		}
	}
	log.Infof("redis state storage enabled: %s", store.Addr())
	return nil
}

func openAICompatInfoFromAuth(a *coreauth.Auth) (providerKey string, compatName string, ok bool) {
	if a == nil {
		return "", "", false
	}
	if len(a.Attributes) > 0 {
		providerKey = strings.TrimSpace(a.Attributes["provider_key"])
		compatName = strings.TrimSpace(a.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey), compatName, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "openai-compatibility") {
		return "openai-compatibility", strings.TrimSpace(a.Label), true
	}
	return "", "", false
}

func (s *Service) ensureExecutorsForAuth(a *coreauth.Auth) {
	s.ensureExecutorsForAuthWithMode(a, false)
}

func (s *Service) ensureExecutorsForAuthWithMode(a *coreauth.Auth, forceReplace bool) {
	if s == nil || s.coreManager == nil || a == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		if !forceReplace {
			existingExecutor, hasExecutor := s.coreManager.Executor("codex")
			if hasExecutor {
				_, isCodexAutoExecutor := existingExecutor.(*executor.CodexAutoExecutor)
				if isCodexAutoExecutor {
					return
				}
			}
		}
		s.coreManager.RegisterExecutor(executor.NewCodexAutoExecutor(s.cfg))
		return
	}
	// Skip disabled auth entries when (re)binding executors.
	// Disabled auths can linger during config reloads (e.g., removed OpenAI-compat entries)
	// and must not override active provider executors.
	if a.Disabled {
		return
	}
	if compatProviderKey, _, isCompat := openAICompatInfoFromAuth(a); isCompat {
		if compatProviderKey == "" {
			compatProviderKey = strings.ToLower(strings.TrimSpace(a.Provider))
		}
		if compatProviderKey == "" {
			compatProviderKey = "openai-compatibility"
		}
		s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(compatProviderKey, s.cfg))
		return
	}
	switch strings.ToLower(a.Provider) {
	case "gemini":
		s.coreManager.RegisterExecutor(executor.NewGeminiExecutor(s.cfg))
	case "vertex":
		s.coreManager.RegisterExecutor(executor.NewGeminiVertexExecutor(s.cfg))
	case "gemini-cli":
		s.coreManager.RegisterExecutor(executor.NewGeminiCLIExecutor(s.cfg))
	case "aistudio":
		if s.wsGateway != nil {
			s.coreManager.RegisterExecutor(executor.NewAIStudioExecutor(s.cfg, a.ID, s.wsGateway))
		}
		return
	case "antigravity":
		s.coreManager.RegisterExecutor(executor.NewAntigravityExecutor(s.cfg))
	case "claude":
		s.coreManager.RegisterExecutor(executor.NewClaudeExecutor(s.cfg))
	case "kimi":
		s.coreManager.RegisterExecutor(executor.NewKimiExecutor(s.cfg))
	case "kiro":
		s.coreManager.RegisterExecutor(executor.NewKiroExecutor(s.cfg))
	default:
		providerKey := strings.ToLower(strings.TrimSpace(a.Provider))
		if providerKey == "" {
			providerKey = "openai-compatibility"
		}
		s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(providerKey, s.cfg))
	}
}

func (s *Service) registerResolvedModelsForAuth(a *coreauth.Auth, providerKey string, models []*ModelInfo) {
	if a == nil || a.ID == "" {
		return
	}
	if len(models) == 0 {
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	GlobalModelRegistry().RegisterClient(a.ID, providerKey, models)
}

// rebindExecutors refreshes provider executors so they observe the latest configuration.
func (s *Service) rebindExecutors() {
	if s == nil || s.coreManager == nil {
		return
	}
	auths := s.coreManager.List()
	reboundCodex := false
	for _, auth := range auths {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			if reboundCodex {
				continue
			}
			reboundCodex = true
		}
		s.ensureExecutorsForAuthWithMode(auth, true)
	}
}

// Run starts the service and blocks until the context is cancelled or the server stops.
// It initializes all components including authentication, file watching, HTTP server,
// and starts processing requests. The method blocks until the context is cancelled.
//
// Parameters:
//   - ctx: The context for controlling the service lifecycle
//
// Returns:
//   - error: An error if the service fails to start or run
func (s *Service) Run(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("cliproxy: service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	usage.StartDefault(ctx)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	defer func() {
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Errorf("service shutdown returned error: %v", err)
		}
	}()

	if err := s.ensureAuthDir(); err != nil {
		return err
	}
	if err := s.configureRedisState(ctx); err != nil {
		return err
	}
	if err := s.loadUsagePersistence(); err != nil {
		log.Warnf("failed to load persisted usage statistics: %v", err)
	}
	s.startUsagePersistence()

	s.applyRetryConfig(s.cfg)

	if s.coreManager != nil {
		if errLoad := s.coreManager.Load(ctx); errLoad != nil {
			log.Warnf("failed to load auth store: %v", errLoad)
		}
		s.rebindExecutors()
	}

	tokenResult, err := s.tokenProvider.Load(ctx, s.cfg)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if tokenResult == nil {
		tokenResult = &TokenClientResult{}
	}

	apiKeyResult, err := s.apiKeyProvider.Load(ctx, s.cfg)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if apiKeyResult == nil {
		apiKeyResult = &APIKeyClientResult{}
	}

	// legacy clients removed; no caches to refresh

	// handlers no longer depend on legacy clients; pass nil slice initially
	s.server = api.NewServer(s.cfg, s.coreManager, s.accessManager, s.configPath, s.serverOptions...)

	if s.authManager == nil {
		s.authManager = newDefaultAuthManager()
	}

	s.ensureWebsocketGateway()
	if s.server != nil && s.wsGateway != nil {
		s.server.AttachWebsocketRoute(s.wsGateway.Path(), s.wsGateway.Handler())
		s.server.SetWebsocketAuthChangeHandler(func(oldEnabled, newEnabled bool) {
			if oldEnabled == newEnabled {
				return
			}
			if !oldEnabled && newEnabled {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if errStop := s.wsGateway.Stop(ctx); errStop != nil {
					log.Warnf("failed to reset websocket connections after ws-auth change %t -> %t: %v", oldEnabled, newEnabled, errStop)
					return
				}
				log.Debugf("ws-auth enabled; existing websocket sessions terminated to enforce authentication")
				return
			}
			log.Debugf("ws-auth disabled; existing websocket sessions remain connected")
		})
	}

	if s.hooks.OnBeforeStart != nil {
		s.hooks.OnBeforeStart(s.cfg)
	}

	// Register callback for startup and periodic model catalog refresh.
	// When remote model definitions change, re-register models for affected providers.
	// This intentionally rebuilds per-auth model availability from the latest catalog
	// snapshot instead of preserving prior registry suppression state.
	registry.SetModelRefreshCallback(func(changedProviders []string) {
		if s == nil || s.coreManager == nil || len(changedProviders) == 0 {
			return
		}

		providerSet := make(map[string]bool, len(changedProviders))
		for _, p := range changedProviders {
			providerSet[strings.ToLower(strings.TrimSpace(p))] = true
		}

		auths := s.coreManager.List()
		refreshed := 0
		for _, item := range auths {
			if item == nil || item.ID == "" {
				continue
			}
			auth, ok := s.coreManager.GetByID(item.ID)
			if !ok || auth == nil || auth.Disabled {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(auth.Provider))
			if !providerSet[provider] {
				continue
			}
			if s.refreshModelRegistrationForAuth(auth) {
				refreshed++
			}
		}

		if refreshed > 0 {
			log.Infof("re-registered models for %d auth(s) due to model catalog changes: %v", refreshed, changedProviders)
		}
	})

	s.serverErr = make(chan error, 1)
	go func() {
		if errStart := s.server.Start(); errStart != nil {
			s.serverErr <- errStart
		} else {
			s.serverErr <- nil
		}
	}()

	time.Sleep(100 * time.Millisecond)
	fmt.Printf("API server started successfully on: %s:%d\n", s.cfg.Host, s.cfg.Port)

	s.applyPprofConfig(s.cfg)

	if s.hooks.OnAfterStart != nil {
		s.hooks.OnAfterStart(s)
	}

	var watcherWrapper *WatcherWrapper
	reloadCallback := func(newCfg *config.Config) {
		previousStrategy := ""
		var previousSessionAffinity bool
		var previousSessionAffinityTTL string
		s.cfgMu.RLock()
		if s.cfg != nil {
			previousStrategy = strings.ToLower(strings.TrimSpace(s.cfg.Routing.Strategy))
			previousSessionAffinity = s.cfg.Routing.ClaudeCodeSessionAffinity || s.cfg.Routing.SessionAffinity
			previousSessionAffinityTTL = s.cfg.Routing.SessionAffinityTTL
		}
		s.cfgMu.RUnlock()

		if newCfg == nil {
			s.cfgMu.RLock()
			newCfg = s.cfg
			s.cfgMu.RUnlock()
		}
		if newCfg == nil {
			return
		}

		nextStrategy := strings.ToLower(strings.TrimSpace(newCfg.Routing.Strategy))
		normalizeStrategy := func(strategy string) string {
			switch strategy {
			case "fill-first", "fillfirst", "ff":
				return "fill-first"
			default:
				return "round-robin"
			}
		}
		previousStrategy = normalizeStrategy(previousStrategy)
		nextStrategy = normalizeStrategy(nextStrategy)

		nextSessionAffinity := newCfg.Routing.ClaudeCodeSessionAffinity || newCfg.Routing.SessionAffinity
		nextSessionAffinityTTL := newCfg.Routing.SessionAffinityTTL

		selectorChanged := previousStrategy != nextStrategy ||
			previousSessionAffinity != nextSessionAffinity ||
			previousSessionAffinityTTL != nextSessionAffinityTTL

		if s.coreManager != nil && selectorChanged {
			var selector coreauth.Selector
			switch nextStrategy {
			case "fill-first":
				selector = &coreauth.FillFirstSelector{}
			default:
				selector = &coreauth.RoundRobinSelector{}
			}

			if nextSessionAffinity {
				ttl := time.Hour
				if ttlStr := strings.TrimSpace(nextSessionAffinityTTL); ttlStr != "" {
					if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
						ttl = parsed
					}
				}
				selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
					Fallback: selector,
					TTL:      ttl,
				})
			}

			s.coreManager.SetSelector(selector)
		}

		s.applyRetryConfig(newCfg)
		s.applyPprofConfig(newCfg)
		if s.server != nil {
			s.server.UpdateClients(newCfg)
		}
		s.cfgMu.Lock()
		s.cfg = newCfg
		s.cfgMu.Unlock()
		if s.coreManager != nil {
			s.coreManager.SetConfig(newCfg)
			s.coreManager.SetOAuthModelAlias(newCfg.OAuthModelAlias)
		}
		s.rebindExecutors()
		s.syncCoreAutoRefresh()
	}

	watcherWrapper, err = s.watcherFactory(s.configPath, s.cfg.AuthDir, reloadCallback)
	if err != nil {
		return fmt.Errorf("cliproxy: failed to create watcher: %w", err)
	}
	s.watcher = watcherWrapper
	s.ensureAuthUpdateQueue(ctx)
	if s.authUpdates != nil {
		watcherWrapper.SetAuthUpdateQueue(s.authUpdates)
	}
	watcherWrapper.SetConfig(s.cfg)

	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	s.watcherCancel = watcherCancel
	if err = watcherWrapper.Start(watcherCtx); err != nil {
		return fmt.Errorf("cliproxy: failed to start watcher: %w", err)
	}
	log.Info("file watcher started for config and auth directory changes")

	// Prefer core auth manager auto refresh if available.
	s.syncCoreAutoRefresh()

	select {
	case <-ctx.Done():
		log.Debug("service context cancelled, shutting down...")
		return ctx.Err()
	case err = <-s.serverErr:
		return err
	}
}

// Shutdown gracefully stops background workers and the HTTP server.
// It ensures all resources are properly cleaned up and connections are closed.
// The shutdown is idempotent and can be called multiple times safely.
//
// Parameters:
//   - ctx: The context for controlling the shutdown timeout
//
// Returns:
//   - error: An error if shutdown fails
func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var shutdownErr error
	s.shutdownOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}

		// legacy refresh loop removed; only stopping core auth manager below

		if s.watcherCancel != nil {
			s.watcherCancel()
		}
		if s.coreManager != nil {
			s.coreManager.StopAutoRefresh()
		}
		if s.watcher != nil {
			if err := s.watcher.Stop(); err != nil {
				log.Errorf("failed to stop file watcher: %v", err)
				shutdownErr = err
			}
		}
		if s.wsGateway != nil {
			if err := s.wsGateway.Stop(ctx); err != nil {
				log.Errorf("failed to stop websocket gateway: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}
		if s.authQueueStop != nil {
			s.authQueueStop()
			s.authQueueStop = nil
		}

		if errShutdownPprof := s.shutdownPprof(ctx); errShutdownPprof != nil {
			log.Errorf("failed to stop pprof server: %v", errShutdownPprof)
			if shutdownErr == nil {
				shutdownErr = errShutdownPprof
			}
		}

		// no legacy clients to persist

		if s.server != nil {
			shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := s.server.Stop(shutdownCtx); err != nil {
				log.Errorf("error stopping API server: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}

		flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
		if errFlush := usage.FlushDefault(flushCtx); errFlush != nil {
			log.Warnf("failed to flush usage statistics before persistence: %v", errFlush)
		}
		cancelFlush()
		if errPersist := s.stopUsagePersistence(); errPersist != nil {
			log.Errorf("failed to persist usage statistics: %v", errPersist)
			if shutdownErr == nil {
				shutdownErr = errPersist
			}
		}
		if s.redisState != nil {
			if errClose := s.redisState.Close(); errClose != nil {
				log.Errorf("failed to close redis state storage: %v", errClose)
				if shutdownErr == nil {
					shutdownErr = errClose
				}
			}
			s.redisState = nil
			s.usagePersistenceBackend = nil
		}

		usage.StopDefault()
	})
	return shutdownErr
}

func (s *Service) ensureAuthDir() error {
	info, err := os.Stat(s.cfg.AuthDir)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(s.cfg.AuthDir, 0o700); mkErr != nil {
				return fmt.Errorf("cliproxy: failed to create auth directory %s: %w", s.cfg.AuthDir, mkErr)
			}
			log.Infof("created missing auth directory: %s", s.cfg.AuthDir)
			return nil
		}
		return fmt.Errorf("cliproxy: error checking auth directory %s: %w", s.cfg.AuthDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cliproxy: auth path exists but is not a directory: %s", s.cfg.AuthDir)
	}
	return nil
}

// registerModelsForAuth (re)binds provider models in the global registry using the core auth ID as client identifier.
func (s *Service) registerModelsForAuth(a *coreauth.Auth) {
	if a == nil || a.ID == "" {
		return
	}
	if a.Disabled {
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	authKind := strings.ToLower(strings.TrimSpace(a.Attributes["auth_kind"]))
	if authKind == "" {
		if kind, _ := a.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = "apikey"
		}
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["gemini_virtual_primary"]); strings.EqualFold(v, "true") {
			GlobalModelRegistry().UnregisterClient(a.ID)
			return
		}
	}
	// Unregister legacy client ID (if present) to avoid double counting
	if a.Runtime != nil {
		if idGetter, ok := a.Runtime.(interface{ GetClientID() string }); ok {
			if rid := idGetter.GetClientID(); rid != "" && rid != a.ID {
				GlobalModelRegistry().UnregisterClient(rid)
			}
		}
	}
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	compatProviderKey, compatDisplayName, compatDetected := openAICompatInfoFromAuth(a)
	if compatDetected {
		provider = "openai-compatibility"
	}
	excluded := s.oauthExcludedModels(provider, authKind)
	// The synthesizer pre-merges per-account and global exclusions into the "excluded_models" attribute.
	// If this attribute is present, it represents the complete list of exclusions and overrides the global config.
	if a.Attributes != nil {
		if val, ok := a.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	var models []*ModelInfo
	switch provider {
	case "gemini":
		models = registry.GetGeminiModels()
		if entry := s.resolveConfigGeminiKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildGeminiConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "vertex":
		// Vertex AI Gemini supports the same model identifiers as Gemini.
		models = registry.GetGeminiVertexModels()
		if entry := s.resolveConfigVertexCompatKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildVertexCompatConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "gemini-cli":
		models = registry.GetGeminiCLIModels()
		models = applyExcludedModels(models, excluded)
	case "aistudio":
		models = registry.GetAIStudioModels()
		models = applyExcludedModels(models, excluded)
	case "antigravity":
		models = registry.GetAntigravityModels()
		models = applyExcludedModels(models, excluded)
	case "claude":
		models = registry.GetClaudeModels()
		if entry := s.resolveConfigClaudeKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildClaudeConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "codex":
		codexPlanType := ""
		if a.Attributes != nil {
			codexPlanType = strings.TrimSpace(a.Attributes["plan_type"])
		}
		switch strings.ToLower(codexPlanType) {
		case "pro":
			models = registry.GetCodexProModels()
		case "plus":
			models = registry.GetCodexPlusModels()
		case "team", "business", "go":
			models = registry.GetCodexTeamModels()
		case "free":
			models = registry.GetCodexFreeModels()
		default:
			models = registry.GetCodexProModels()
		}
		if entry := s.resolveConfigCodexKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildCodexConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "kimi":
		models = registry.GetKimiModels()
		models = applyExcludedModels(models, excluded)
	case "kiro":
		models = s.fetchKiroModels(a)
		models = applyExcludedModels(models, excluded)
	default:
		// Handle OpenAI-compatibility providers by name using config
		if s.cfg != nil {
			providerKey := provider
			compatName := strings.TrimSpace(a.Provider)
			isCompatAuth := false
			if compatDetected {
				if compatProviderKey != "" {
					providerKey = compatProviderKey
				}
				if compatDisplayName != "" {
					compatName = compatDisplayName
				}
				isCompatAuth = true
			}
			if strings.EqualFold(providerKey, "openai-compatibility") {
				isCompatAuth = true
				if a.Attributes != nil {
					if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
						compatName = v
					}
					if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
						providerKey = strings.ToLower(v)
						isCompatAuth = true
					}
				}
				if providerKey == "openai-compatibility" && compatName != "" {
					providerKey = strings.ToLower(compatName)
				}
			} else if a.Attributes != nil {
				if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
					compatName = v
					isCompatAuth = true
				}
				if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
					providerKey = strings.ToLower(v)
					isCompatAuth = true
				}
			}
			for i := range s.cfg.OpenAICompatibility {
				compat := &s.cfg.OpenAICompatibility[i]
				if compat.Disabled {
					continue
				}
				if strings.EqualFold(compat.Name, compatName) {
					isCompatAuth = true
					// Convert compatibility models to registry models
					ms := make([]*ModelInfo, 0, len(compat.Models))
					for j := range compat.Models {
						m := compat.Models[j]
						// Use alias as model ID, fallback to name if alias is empty
						modelID := m.Alias
						if modelID == "" {
							modelID = m.Name
						}
						thinking := m.Thinking
						if thinking == nil {
							thinking = &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
						}
						ms = append(ms, &ModelInfo{
							ID:          modelID,
							Object:      "model",
							Created:     time.Now().Unix(),
							OwnedBy:     compat.Name,
							Type:        "openai-compatibility",
							DisplayName: modelID,
							UserDefined: false,
							Thinking:    thinking,
						})
					}
					// Register and return
					if len(ms) > 0 {
						if providerKey == "" {
							providerKey = "openai-compatibility"
						}
						s.registerResolvedModelsForAuth(a, providerKey, applyModelPrefixes(ms, a.Prefix, s.cfg.ForceModelPrefix))
					} else {
						// Ensure stale registrations are cleared when model list becomes empty.
						GlobalModelRegistry().UnregisterClient(a.ID)
					}
					return
				}
			}
			if isCompatAuth {
				// No matching provider found or models removed entirely; drop any prior registration.
				GlobalModelRegistry().UnregisterClient(a.ID)
				return
			}
		}
	}
	models = applyOAuthModelAlias(s.cfg, provider, authKind, models)
	if len(models) > 0 {
		key := provider
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(a.Provider))
		}
		s.registerResolvedModelsForAuth(a, key, applyModelPrefixes(models, a.Prefix, s.cfg != nil && s.cfg.ForceModelPrefix))
		return
	}

	GlobalModelRegistry().UnregisterClient(a.ID)
}

// refreshModelRegistrationForAuth re-applies the latest model registration for
// one auth and reconciles any concurrent auth changes that race with the
// refresh. Callers are expected to pre-filter provider membership.
//
// Re-registration is deliberate: registry cooldown/suspension state is treated
// as part of the previous registration snapshot and is cleared when the auth is
// rebound to the refreshed model catalog.
func (s *Service) refreshModelRegistrationForAuth(current *coreauth.Auth) bool {
	if s == nil || s.coreManager == nil || current == nil || current.ID == "" {
		return false
	}

	if !current.Disabled {
		s.ensureExecutorsForAuth(current)
	}
	s.registerModelsForAuth(current)
	s.coreManager.ReconcileRegistryModelStates(context.Background(), current.ID)

	latest, ok := s.latestAuthForModelRegistration(current.ID)
	if !ok || latest.Disabled {
		GlobalModelRegistry().UnregisterClient(current.ID)
		s.coreManager.RefreshSchedulerEntry(current.ID)
		return false
	}

	// Re-apply the latest auth snapshot so concurrent auth updates cannot leave
	// stale model registrations behind. This may duplicate registration work when
	// no auth fields changed, but keeps the refresh path simple and correct.
	s.ensureExecutorsForAuth(latest)
	s.registerModelsForAuth(latest)
	s.coreManager.ReconcileRegistryModelStates(context.Background(), latest.ID)
	s.coreManager.RefreshSchedulerEntry(current.ID)
	return true
}

// latestAuthForModelRegistration returns the latest auth snapshot regardless of
// provider membership. Callers use this after a registration attempt to restore
// whichever state currently owns the client ID in the global registry.
func (s *Service) latestAuthForModelRegistration(authID string) (*coreauth.Auth, bool) {
	if s == nil || s.coreManager == nil || authID == "" {
		return nil, false
	}
	auth, ok := s.coreManager.GetByID(authID)
	if !ok || auth == nil || auth.ID == "" {
		return nil, false
	}
	return auth, true
}

func (s *Service) resolveConfigClaudeKey(auth *coreauth.Auth) *config.ClaudeKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.ClaudeKey {
		entry := &s.cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range s.cfg.ClaudeKey {
			entry := &s.cfg.ClaudeKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func (s *Service) resolveConfigGeminiKey(auth *coreauth.Auth) *config.GeminiKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.GeminiKey {
		entry := &s.cfg.GeminiKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) resolveConfigVertexCompatKey(auth *coreauth.Auth) *config.VertexCompatKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.VertexCompatAPIKey {
		entry := &s.cfg.VertexCompatAPIKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range s.cfg.VertexCompatAPIKey {
			entry := &s.cfg.VertexCompatAPIKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func (s *Service) resolveConfigCodexKey(auth *coreauth.Auth) *config.CodexKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.CodexKey {
		entry := &s.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) oauthExcludedModels(provider, authKind string) []string {
	cfg := s.cfg
	if cfg == nil {
		return nil
	}
	authKindKey := strings.ToLower(strings.TrimSpace(authKind))
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if authKindKey == "apikey" {
		return nil
	}
	return cfg.OAuthExcludedModels[providerKey]
}

func applyExcludedModels(models []*ModelInfo, excluded []string) []*ModelInfo {
	if len(models) == 0 || len(excluded) == 0 {
		return models
	}

	patterns := make([]string, 0, len(excluded))
	for _, item := range excluded {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			patterns = append(patterns, strings.ToLower(trimmed))
		}
	}
	if len(patterns) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.ToLower(strings.TrimSpace(model.ID))
		blocked := false
		for _, pattern := range patterns {
			if matchWildcard(pattern, modelID) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// fetchKiroModels returns the dynamic kiro model catalog pulled from the
// upstream listAvailableModels API using the auth's access token.
//
// When the token is missing or the API call fails this returns nil, which
// causes registerResolvedModelsForAuth to unregister the auth from the model
// registry rather than serving stale hard-coded entries. Previous behaviour
// merged the dynamic list with a hard-coded static fallback; that fallback
// was removed so the advertised catalog always matches what the upstream
// will actually accept for this account.
func (s *Service) fetchKiroModels(a *coreauth.Auth) []*ModelInfo {
	if s != nil && s.coreManager != nil && a != nil && a.ID != "" {
		if current, ok := s.coreManager.GetByID(a.ID); ok && current != nil {
			a = current
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if refreshed, err := s.refreshKiroAuthForModelFetch(ctx, a, false); err != nil {
		log.Warnf("kiro: failed to refresh auth before fetching model list: %v", err)
	} else if refreshed != nil {
		a = refreshed
	}

	tokenData := extractKiroTokenData(a)
	if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		log.Debug("kiro: no access token available; no models will be registered for this auth")
		return nil
	}

	apiModels, err := kiroauth.NewKiroAuth(s.cfg).ListAvailableModels(ctx, tokenData)
	if err != nil && kiroauth.IsUnauthorizedStatusError(err) {
		if refreshed, refreshErr := s.refreshKiroAuthForModelFetch(ctx, a, true); refreshErr != nil {
			log.Warnf("kiro: model list auth failed and token refresh failed: %v", refreshErr)
		} else if refreshed != nil {
			a = refreshed
			tokenData = extractKiroTokenData(a)
			if tokenData != nil && strings.TrimSpace(tokenData.AccessToken) != "" {
				apiModels, err = kiroauth.NewKiroAuth(s.cfg).ListAvailableModels(ctx, tokenData)
			}
		}
	}
	if err != nil {
		log.Warnf("kiro: failed to fetch dynamic model list: %v; no models will be registered for this auth", err)
		return nil
	}
	if len(apiModels) == 0 {
		log.Debug("kiro: dynamic model list was empty; no models will be registered for this auth")
		return nil
	}

	dynamicModels := convertKiroAPIModels(apiModels)
	if len(dynamicModels) == 0 {
		return nil
	}
	s.persistKiroResolvedProfileArn(ctx, a, tokenData.ProfileArn)
	log.Infof("kiro: fetched %d dynamic models from API", len(dynamicModels))
	return dynamicModels
}

func (s *Service) refreshKiroAuthForModelFetch(ctx context.Context, auth *coreauth.Auth, force bool) (*coreauth.Auth, error) {
	if s == nil || s.coreManager == nil || auth == nil {
		return auth, nil
	}
	if force {
		return s.coreManager.RefreshAuth(ctx, auth)
	}
	return s.coreManager.RefreshAuthIfNeeded(ctx, auth)
}

func (s *Service) persistKiroResolvedProfileArn(ctx context.Context, auth *coreauth.Auth, profileArn string) {
	profileArn = strings.TrimSpace(profileArn)
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" || profileArn == "" {
		return
	}
	current, ok := s.coreManager.GetByID(auth.ID)
	if ok && current != nil {
		auth = current
	}
	if existing := kiroAuthMetadataString(auth.Metadata, "profile_arn", "profileArn"); existing == profileArn {
		if auth.Attributes == nil || strings.TrimSpace(auth.Attributes["profile_arn"]) == profileArn {
			return
		}
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	updated.Metadata["profile_arn"] = profileArn
	if updated.Attributes == nil {
		updated.Attributes = map[string]string{}
	}
	updated.Attributes["profile_arn"] = profileArn
	if _, err := s.coreManager.Update(ctx, updated); err != nil {
		log.Warnf("kiro: failed to persist resolved profile arn for model list: %v", err)
	}
}

func extractKiroTokenData(a *coreauth.Auth) *kiroauth.TokenData {
	if a == nil {
		return nil
	}

	tokenData := &kiroauth.TokenData{}
	if a.Metadata != nil {
		tokenData.AccessToken = kiroAuthMetadataString(a.Metadata, "access_token", "accessToken")
		tokenData.RefreshToken = kiroAuthMetadataString(a.Metadata, "refresh_token", "refreshToken")
		tokenData.ProfileArn = kiroAuthMetadataString(a.Metadata, "profile_arn", "profileArn")
		tokenData.ClientID = kiroAuthMetadataString(a.Metadata, "client_id", "clientId")
		tokenData.ClientSecret = kiroAuthMetadataString(a.Metadata, "client_secret", "clientSecret")
		tokenData.Region = kiroAuthMetadataString(a.Metadata, "region")
	}
	if a.Attributes != nil {
		if tokenData.AccessToken == "" {
			tokenData.AccessToken = strings.TrimSpace(a.Attributes["access_token"])
		}
		if tokenData.RefreshToken == "" {
			tokenData.RefreshToken = strings.TrimSpace(a.Attributes["refresh_token"])
		}
		if tokenData.ProfileArn == "" {
			tokenData.ProfileArn = strings.TrimSpace(a.Attributes["profile_arn"])
		}
	}
	if tokenData.AccessToken == "" {
		return nil
	}
	return tokenData
}

func convertKiroAPIModels(apiModels []*kiroauth.KiroModel) []*ModelInfo {
	if len(apiModels) == 0 {
		return nil
	}

	now := time.Now().Unix()
	models := make([]*ModelInfo, 0, len(apiModels)*2)
	seen := make(map[string]struct{}, len(apiModels)*2)
	for _, apiModel := range apiModels {
		if apiModel == nil || strings.TrimSpace(apiModel.ModelID) == "" {
			continue
		}

		contextLength := apiModel.MaxInputTokens
		if contextLength <= 0 {
			contextLength = 200000
		}
		model := &ModelInfo{
			ID:                  normalizeKiroModelID(apiModel.ModelID),
			Object:              "model",
			Created:             now,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         formatKiroDisplayName(apiModel.ModelName, apiModel.RateMultiplier),
			Description:         apiModel.Description,
			ContextLength:       contextLength,
			MaxCompletionTokens: 64000,
			Thinking:            &registry.ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		}
		models = appendKiroModelWithAliases(models, seen, model)
	}
	return models
}

func appendKiroModelWithAliases(models []*ModelInfo, seen map[string]struct{}, model *ModelInfo) []*ModelInfo {
	if model == nil || strings.TrimSpace(model.ID) == "" {
		return models
	}
	models = appendKiroModelIfNew(models, seen, model)
	aliasID := kiroHyphenModelAlias(model.ID)
	if aliasID == model.ID {
		return models
	}
	alias := cloneKiroModelInfo(model)
	alias.ID = aliasID
	return appendKiroModelIfNew(models, seen, alias)
}

func appendKiroModelIfNew(models []*ModelInfo, seen map[string]struct{}, model *ModelInfo) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(model.ID))
	if key == "" {
		return models
	}
	if _, ok := seen[key]; ok {
		return models
	}
	seen[key] = struct{}{}
	return append(models, model)
}

func kiroHyphenModelAlias(id string) string {
	return strings.ReplaceAll(strings.TrimSpace(id), ".", "-")
}

func normalizeKiroModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	modelID = strings.TrimPrefix(modelID, "kiro-")
	modelID = strings.TrimPrefix(modelID, "anthropic.")
	modelID = strings.TrimPrefix(modelID, "amazon.")
	modelID = strings.ReplaceAll(modelID, "_", "-")
	return modelID
}

func formatKiroDisplayName(modelName string, rateMultiplier float64) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	displayName := "Kiro " + modelName
	if rateMultiplier > 0 && rateMultiplier != 1 {
		displayName += fmt.Sprintf(" (%sx credit)", strconv.FormatFloat(rateMultiplier, 'f', -1, 64))
	}
	return displayName
}

func generateKiroAgenticVariants(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models)*2)
	for _, model := range models {
		if model == nil {
			continue
		}
		out = append(out, model)
		if strings.HasSuffix(model.ID, "-agentic") || model.ID == "auto" || strings.HasSuffix(model.ID, "-auto") {
			continue
		}
		agentic := cloneKiroModelInfo(model)
		agentic.ID = model.ID + "-agentic"
		if model.DisplayName != "" {
			agentic.DisplayName = model.DisplayName + " (Agentic)"
		}
		if model.Description != "" {
			agentic.Description = model.Description + " - Optimized for coding agents"
		} else {
			agentic.Description = "Optimized for coding agents"
		}
		out = append(out, agentic)
	}
	return out
}

func cloneKiroModelInfo(model *ModelInfo) *ModelInfo {
	if model == nil {
		return nil
	}
	cloned := *model
	if model.Thinking != nil {
		thinking := *model.Thinking
		if len(model.Thinking.Levels) > 0 {
			thinking.Levels = append([]string(nil), model.Thinking.Levels...)
		}
		cloned.Thinking = &thinking
	}
	if len(model.SupportedParameters) > 0 {
		cloned.SupportedParameters = append([]string(nil), model.SupportedParameters...)
	}
	if len(model.SupportedInputModalities) > 0 {
		cloned.SupportedInputModalities = append([]string(nil), model.SupportedInputModalities...)
	}
	if len(model.SupportedOutputModalities) > 0 {
		cloned.SupportedOutputModalities = append([]string(nil), model.SupportedOutputModalities...)
	}
	return &cloned
}

func kiroAuthMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func applyModelPrefixes(models []*ModelInfo, prefix string, forceModelPrefix bool) []*ModelInfo {
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPrefix == "" || len(models) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)

	addModel := func(model *ModelInfo) {
		if model == nil {
			return
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		baseID := strings.TrimSpace(model.ID)
		if baseID == "" {
			continue
		}
		if !forceModelPrefix || trimmedPrefix == baseID {
			addModel(model)
		}
		clone := *model
		clone.ID = trimmedPrefix + "/" + baseID
		addModel(&clone)
	}
	return out
}

// matchWildcard performs case-insensitive wildcard matching where '*' matches any substring.
func matchWildcard(pattern, value string) bool {
	if pattern == "" {
		return false
	}

	// Fast path for exact match (no wildcard present).
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	// Handle prefix.
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}

	// Handle suffix.
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}

	// Handle middle segments in order.
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}

	return true
}

type modelEntry interface {
	GetName() string
	GetAlias() string
}

func buildConfigModels[T modelEntry](models []T, ownedBy, modelType string) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for i := range models {
		model := models[i]
		name := strings.TrimSpace(model.GetName())
		alias := strings.TrimSpace(model.GetAlias())
		if alias == "" {
			alias = name
		}
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		display := name
		if display == "" {
			display = alias
		}
		info := &ModelInfo{
			ID:          alias,
			Object:      "model",
			Created:     now,
			OwnedBy:     ownedBy,
			Type:        modelType,
			DisplayName: display,
			UserDefined: true,
		}
		if name != "" {
			if upstream := registry.LookupStaticModelInfo(name); upstream != nil && upstream.Thinking != nil {
				info.Thinking = upstream.Thinking
			}
		}
		out = append(out, info)
	}
	return out
}

func buildVertexCompatConfigModels(entry *config.VertexCompatKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "vertex")
}

func buildGeminiConfigModels(entry *config.GeminiKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "gemini")
}

func buildClaudeConfigModels(entry *config.ClaudeKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "anthropic", "claude")
}

func buildCodexConfigModels(entry *config.CodexKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return registry.WithCodexBuiltins(buildConfigModels(entry.Models, "openai", "openai"))
}

func rewriteModelInfoName(name, oldID, newID string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return name
	}
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	if oldID == "" || newID == "" {
		return name
	}
	if strings.EqualFold(oldID, newID) {
		return name
	}
	if strings.EqualFold(trimmed, oldID) {
		return newID
	}
	if strings.HasSuffix(trimmed, "/"+oldID) {
		prefix := strings.TrimSuffix(trimmed, oldID)
		return prefix + newID
	}
	if trimmed == "models/"+oldID {
		return "models/" + newID
	}
	return name
}

func applyOAuthModelAlias(cfg *config.Config, provider, authKind string, models []*ModelInfo) []*ModelInfo {
	if cfg == nil || len(models) == 0 {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, authKind)
	if channel == "" || len(cfg.OAuthModelAlias) == 0 {
		return models
	}
	aliases := cfg.OAuthModelAlias[channel]
	if len(aliases) == 0 {
		return models
	}

	type aliasEntry struct {
		alias string
		fork  bool
	}

	forward := make(map[string][]aliasEntry, len(aliases))
	for i := range aliases {
		name := strings.TrimSpace(aliases[i].Name)
		alias := strings.TrimSpace(aliases[i].Alias)
		if name == "" || alias == "" {
			continue
		}
		if strings.EqualFold(name, alias) {
			continue
		}
		key := strings.ToLower(name)
		forward[key] = append(forward[key], aliasEntry{alias: alias, fork: aliases[i].Fork})
	}
	if len(forward) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		entries := forward[key]
		if len(entries) == 0 {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
			continue
		}

		keepOriginal := false
		for _, entry := range entries {
			if entry.fork {
				keepOriginal = true
				break
			}
		}
		if keepOriginal {
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				out = append(out, model)
			}
		}

		addedAlias := false
		for _, entry := range entries {
			mappedID := strings.TrimSpace(entry.alias)
			if mappedID == "" {
				continue
			}
			if strings.EqualFold(mappedID, id) {
				continue
			}
			aliasKey := strings.ToLower(mappedID)
			if _, exists := seen[aliasKey]; exists {
				continue
			}
			seen[aliasKey] = struct{}{}
			clone := *model
			clone.ID = mappedID
			if clone.Name != "" {
				clone.Name = rewriteModelInfoName(clone.Name, id, mappedID)
			}
			out = append(out, &clone)
			addedAlias = true
		}

		if !keepOriginal && !addedAlias {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
		}
	}
	return out
}
