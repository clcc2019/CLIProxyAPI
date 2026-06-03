package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// RequestAuthPreparer lets an executor update missing auth metadata immediately
// before a request. Manager serializes and persists returned updates.
type RequestAuthPreparer interface {
	ShouldPrepareRequestAuth(auth *Auth) bool
	PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error)
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

// ExecutionSessionResetter allows executors to invalidate per-session runtime
// state so future requests establish a fresh upstream session.
type ExecutionSessionResetter interface {
	ResetExecutionSession(sessionID string)
}

const (
	homeAuthCountMetadataKey = "__cliproxy_home_auth_count"
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval   = 5 * time.Second
	refreshMaxConcurrency  = 16
	refreshPendingBackoff  = time.Minute
	refreshFailureBackoff  = 5 * time.Minute
	refreshBatchDrainDelay = time.Second
	quotaBackoffBase       = time.Second
	quotaBackoffMax        = 30 * time.Minute
	persistDebounceWindow  = time.Second
	// refreshIneffectiveBackoff throttles refresh attempts when an executor returns
	// success but the auth still evaluates as needing refresh (e.g. token expiry
	// wasn't updated). Without this guard, the auto-refresh loop can tight-loop and
	// burn CPU at idle.
	refreshIneffectiveBackoff = 30 * time.Second
)

var quotaCooldownDisabled atomic.Bool

var authIDSetPool = sync.Pool{
	New: func() any {
		return make(map[string]struct{}, 16)
	},
}

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	return quotaCooldownDisabled.Load()
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// Error describes the failure when Success is false.
	Error *Error
	// AuthScoped indicates the failure applies to the entire auth (all models),
	// not only the model that triggered it. Set when the originating error
	// implements AuthScopedFailure — canonical case is Kiro's AGENTIC_REQUEST
	// 429 where the quota is a single bucket shared across models. When true,
	// MarkResult suspends the auth as a whole instead of only Model.
	AuthScoped bool
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

// StoppableSelector is an optional interface for selectors that hold resources.
// Selectors that implement this interface will have Stop called during shutdown.
type StoppableSelector interface {
	Selector
	Stop()
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store             Store
	runtimeStateStore RuntimeStateStore
	proxyLeaseStore   ProxyLeaseStore
	executors         map[string]ProviderExecutor
	selector          Selector
	kiroSelector      Selector
	hook              Hook
	mu                sync.RWMutex
	auths             map[string]*Auth
	removedAuths      map[string]authRemovalTombstone
	runtimeStates     map[string]AuthRuntimeState
	scheduler         *authScheduler
	// homeRuntimeAuths caches auths returned by Home so websocket sessions can
	// reuse an established upstream credential without dispatching every turn.
	homeRuntimeAuths map[string]map[string]*Auth
	// routeAwareSingleCache stores provider/model pairs proven safe for scheduler single-provider fast path.
	routeAwareSingleCache sync.Map
	// routeAwareMixedCache stores provider/model combinations proven safe for scheduler mixed fast path.
	routeAwareMixedCache sync.Map
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets map[string]int

	// Retry controls request retry behavior.
	requestRetry        atomic.Int32
	maxRetryCredentials atomic.Int32
	maxRetryInterval    atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// modelPoolOffsets tracks per-auth alias pool rotation state.
	modelPoolOffsets map[string]int

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// Auto refresh state
	refreshCancel      context.CancelFunc
	refreshLoop        *authAutoRefreshLoop
	refreshGroup       singleflight.Group
	refreshSemaphore   chan struct{}
	refreshBatchCursor int

	requestPrepareLocks sync.Map

	// Deferred persistence state
	persistEnabled atomic.Bool
	persistMu      sync.Mutex
	persistWake    chan struct{}
	persistCancel  context.CancelFunc
	persistWG      sync.WaitGroup
	persistIDs     map[string]struct{}
}

type authRemovalTombstone struct {
	Path      string
	RemovedAt time.Time
}

const (
	proxyPoolAssignedAttribute  = "proxy_pool_assigned"
	proxyPoolAssignedValue      = "true"
	defaultProxyFailureCooldown = 10 * time.Minute
)

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:            store,
		executors:        make(map[string]ProviderExecutor),
		selector:         selector,
		kiroSelector:     newKiroSelector(time.Hour),
		hook:             hook,
		auths:            make(map[string]*Auth),
		removedAuths:     make(map[string]authRemovalTombstone),
		homeRuntimeAuths: make(map[string]map[string]*Auth),
		providerOffsets:  make(map[string]int),
		modelPoolOffsets: make(map[string]int),
		refreshSemaphore: make(chan struct{}, refreshMaxConcurrency),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.persistEnabled.Store(store != nil)
	manager.scheduler = newAuthScheduler(selector)
	return manager
}

func newKiroSelector(ttl time.Duration) Selector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      ttl,
	})
}

func sessionAffinityTTLFromConfig(cfg *internalconfig.Config) time.Duration {
	ttl := time.Hour
	if cfg == nil {
		return ttl
	}
	if ttlStr := strings.TrimSpace(cfg.Routing.SessionAffinityTTL); ttlStr != "" {
		if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
			return parsed
		}
	}
	return ttl
}

func (m *Manager) refreshKiroSelector(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	next := newKiroSelector(sessionAffinityTTLFromConfig(cfg))
	var previous Selector
	m.mu.Lock()
	previous = m.kiroSelector
	m.kiroSelector = next
	m.mu.Unlock()
	if previous == nil || previous == next {
		return
	}
	if stoppable, ok := previous.(StoppableSelector); ok {
		stoppable.Stop()
	}
}

func isBuiltInSelector(selector Selector) bool {
	switch selector.(type) {
	case *RoundRobinSelector, *FillFirstSelector:
		return true
	default:
		return false
	}
}

func (m *Manager) syncSchedulerFromSnapshot(auths []*Auth) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.clearRouteAwareCaches()
	m.scheduler.rebuild(auths)
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

// RefreshSchedulerEntry re-upserts a single auth into the scheduler so that its
// supportedModelSet is rebuilt from the current global model registry state.
// This must be called after models have been registered for a newly added auth,
// because the initial scheduler.upsertAuth during Register/Update runs before
// registerModelsForAuth and therefore snapshots an empty model set.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return
	}
	snapshot := auth.CloneForScheduler()
	m.mu.RUnlock()
	m.scheduler.upsertAuth(snapshot)
}

// ReconcileRegistryModelStates aligns per-model runtime state with the current
// registry snapshot for one auth.
//
// Active quota cooldowns for supported models are preserved because they may
// have been restored from runtime persistence before model registration.
// ModelStates for models that are no longer present in the registry are pruned
// entirely so renamed/removed models cannot keep auth-level status stale.
func (m *Manager) ReconcileRegistryModelStates(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}

	supportedModelIDs := registry.GetGlobalRegistry().GetModelIDsForClient(authID)
	supported := make(map[string]struct{}, len(supportedModelIDs))
	for _, modelID := range supportedModelIDs {
		modelKey := canonicalModelKey(modelID)
		if modelKey == "" {
			continue
		}
		supported[modelKey] = struct{}{}
	}

	var snapshot *Auth
	var runtimeSnapshot *Auth
	var quotaModelsToRestore []string
	var quotaModelsToClear []string
	now := time.Now()

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if ok && auth != nil && len(auth.ModelStates) > 0 {
		changed := false
		schedulerRefresh := false
		for modelKey, state := range auth.ModelStates {
			baseModel := canonicalModelKey(modelKey)
			if baseModel == "" {
				baseModel = strings.TrimSpace(modelKey)
			}
			if _, supportedModel := supported[baseModel]; !supportedModel {
				// Drop state for models that disappeared from the current registry
				// snapshot. Keeping them around leaks stale errors into auth-level
				// status, management output, and websocket fallback checks.
				delete(auth.ModelStates, modelKey)
				changed = true
				if state != nil && state.Quota.Exceeded {
					quotaModelsToClear = append(quotaModelsToClear, baseModel)
				}
				continue
			}
			if state == nil {
				continue
			}
			if modelStateIsClean(state) {
				continue
			}
			if shouldPreserveModelStateOnReconcile(state, now) {
				schedulerRefresh = true
				if state.Quota.Exceeded {
					quotaModelsToRestore = append(quotaModelsToRestore, baseModel)
				}
				continue
			}
			if state.Quota.Exceeded {
				quotaModelsToClear = append(quotaModelsToClear, baseModel)
			}
			resetModelState(state, now)
			changed = true
		}
		if len(auth.ModelStates) == 0 {
			auth.ModelStates = nil
		}
		if changed {
			updateAggregatedAvailability(auth, now)
			if !hasModelError(auth, now) {
				auth.LastError = nil
				auth.StatusMessage = ""
				auth.Status = StatusActive
			}
			auth.UpdatedAt = now
			if errPersist := m.persist(ctx, auth); errPersist != nil {
				logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth changes during model state reconciliation: %v", errPersist)
			}
			runtimeSnapshot = auth.Clone()
			snapshot = auth.CloneForScheduler()
		} else if schedulerRefresh {
			snapshot = auth.CloneForScheduler()
		}
	}
	m.mu.Unlock()

	if runtimeSnapshot != nil {
		if errPersist := m.persistRuntimeState(ctx, runtimeSnapshot); errPersist != nil {
			logEntryWithRequestID(ctx).WithField("auth_id", runtimeSnapshot.ID).Warnf("failed to persist auth runtime state during model state reconciliation: %v", errPersist)
		}
	}
	for _, model := range quotaModelsToClear {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(authID, model)
	}
	for _, model := range quotaModelsToRestore {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(authID, model)
	}
	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

func shouldPreserveModelStateOnReconcile(state *ModelState, now time.Time) bool {
	if state == nil || modelStateIsClean(state) {
		return false
	}
	if !state.Unavailable || state.NextRetryAfter.IsZero() || !state.NextRetryAfter.After(now) {
		return false
	}
	if state.Quota.Exceeded || strings.EqualFold(state.Quota.Reason, "quota") {
		return true
	}
	return statusCodeFromResult(state.LastError) == http.StatusTooManyRequests
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.mu.Lock()
	m.selector = selector
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
	m.persistEnabled.Store(m.store != nil || m.runtimeStateStore != nil)
}

// SetRuntimeStateStore swaps the optional persistence store for mutable runtime
// state such as request counters and quota cooldowns.
func (m *Manager) SetRuntimeStateStore(store RuntimeStateStore) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runtimeStateStore = store
	if store == nil {
		m.runtimeStates = nil
	}
	m.persistEnabled.Store(m.store != nil || m.runtimeStateStore != nil)
}

// SetProxyLeaseStore swaps the optional persistence store for proxy-pool leases.
func (m *Manager) SetProxyLeaseStore(store ProxyLeaseStore) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.proxyLeaseStore = store
	m.mu.Unlock()
}

// ReconcileProxyPoolLeases ensures already-loaded auths have stable pool leases
// after config or Redis state becomes available.
func (m *Manager) ReconcileProxyPoolLeases(ctx context.Context) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	m.mu.RLock()
	store := m.proxyLeaseStore
	auths := make([]*Auth, 0, len(m.auths))
	activeAuthIDs := make([]string, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth != nil {
			auths = append(auths, auth.Clone())
			if authID := strings.TrimSpace(auth.ID); authID != "" {
				activeAuthIDs = append(activeAuthIDs, authID)
			}
		}
	}
	m.mu.RUnlock()
	assignableAuths := proxyPoolSortedAssignableAuths(cfg, auths)
	leaseAuthIDs := activeAuthIDs
	selectedLeaseAuthIDs := make(map[string]struct{}, len(assignableAuths))
	if cfg != nil && cfg.ProxyPool.Enabled {
		leaseAuthIDs = proxyPoolSelectedLeaseAuthIDs(cfg, assignableAuths)
		for _, authID := range leaseAuthIDs {
			selectedLeaseAuthIDs[authID] = struct{}{}
		}
	}
	if store != nil {
		proxyURLs := []string(nil)
		if cfg != nil && cfg.ProxyPool.Enabled {
			proxyURLs = cfg.ProxyPool.Proxies
		}
		if len(leaseAuthIDs) > 0 || (cfg != nil && cfg.ProxyPool.Enabled && len(activeAuthIDs) > 0) {
			if err := store.ReconcileProxyLeases(ctx, leaseAuthIDs, proxyURLs); err != nil {
				log.Warnf("proxy-pool: failed to reconcile proxy leases: %v", err)
			}
		}
		if len(leaseAuthIDs) == 0 && cfg != nil && !cfg.ProxyPool.Enabled {
			if err := store.ReconcileProxyLeases(ctx, leaseAuthIDs, proxyURLs); err != nil {
				log.Warnf("proxy-pool: failed to reconcile proxy leases: %v", err)
			}
		}
	}
	for _, auth := range auths {
		if auth == nil || !authProxyPoolAssigned(auth) {
			continue
		}
		if _, selected := selectedLeaseAuthIDs[strings.TrimSpace(auth.ID)]; !selected {
			m.releaseProxyLease(ctx, auth.ID)
		}
	}
	for _, auth := range assignableAuths {
		if _, selected := selectedLeaseAuthIDs[strings.TrimSpace(auth.ID)]; !selected {
			continue
		}
		m.applyProxyPoolLease(ctx, auth)
	}
}

func (m *Manager) reconcileProxyPoolLeasesAfterAuthChange(ctx context.Context) {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.ProxyPool.Enabled {
		return
	}
	m.mu.RLock()
	store := m.proxyLeaseStore
	m.mu.RUnlock()
	if store == nil {
		return
	}
	m.ReconcileProxyPoolLeases(ctx)
}

// LoadRuntimeStates reads persisted runtime state and applies it to already
// loaded auths. Future Register calls also apply the loaded state by auth ID.
func (m *Manager) LoadRuntimeStates(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	store := m.runtimeStateStore
	m.mu.RUnlock()
	if store == nil {
		return nil
	}
	states, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if states == nil {
		states = make(map[string]AuthRuntimeState)
	}

	m.mu.Lock()
	m.runtimeStates = states
	var snapshots []*Auth
	for id, auth := range m.auths {
		if auth == nil {
			continue
		}
		if state, ok := states[id]; ok {
			auth.ApplyRuntimeState(state)
			snapshots = append(snapshots, auth.CloneForScheduler())
		}
	}
	m.mu.Unlock()

	if len(snapshots) > 0 && m.scheduler != nil {
		for _, snapshot := range snapshots {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	return nil
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.runtimeConfig.Store(cfg)
	if !cfg.Home.Enabled {
		m.clearHomeRuntimeAuths()
	}
	m.refreshKiroSelector(cfg)
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.ReconcileProxyPoolLeases(context.Background())
}

// HomeEnabled reports whether the home control plane integration is enabled in the runtime config.
func (m *Manager) HomeEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.Home.Enabled
}

func (m *Manager) oauthRefreshConfig() internalconfig.OAuthRefreshConfig {
	if m == nil {
		return internalconfig.OAuthRefreshConfig{}
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return internalconfig.OAuthRefreshConfig{}
	}
	return cfg.OAuthRefresh
}

func (m *Manager) autoRefreshEnabled() bool {
	if m.globalAutoRefreshEnabled() {
		return true
	}
	return HasDefaultAutoRefreshProviders()
}

func (m *Manager) globalAutoRefreshEnabled() bool {
	cfg := m.oauthRefreshConfig()
	if cfg.Enabled == nil {
		return false
	}
	return *cfg.Enabled
}

func (m *Manager) authAutoRefreshEnabled(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if m.globalAutoRefreshEnabled() {
		return true
	}
	return ProviderDefaultAutoRefresh(auth.Provider)
}

func (m *Manager) refreshOnStartupEnabled() bool {
	cfg := m.oauthRefreshConfig()
	if cfg.OnStartup == nil {
		return true
	}
	return *cfg.OnStartup
}

func (m *Manager) refreshBatchSize() int {
	cfg := m.oauthRefreshConfig()
	if cfg.BatchSize <= 0 {
		return 0
	}
	return cfg.BatchSize
}

func (m *Manager) lookupAPIKeyUpstreamModel(authID, requestedModel string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if table == nil {
		return ""
	}
	byAlias := table[authID]
	if len(byAlias) == 0 {
		return ""
	}
	key := strings.ToLower(thinking.ParseSuffix(requestedModel).ModelName)
	if key == "" {
		key = strings.ToLower(requestedModel)
	}
	resolved := strings.TrimSpace(byAlias[key])
	if resolved == "" {
		return ""
	}
	return preserveRequestedModelSuffix(requestedModel, resolved)
}

func isAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	kind, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(kind), "api_key")
}

func isOpenAICompatAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	// Scheduler snapshots intentionally omit api_key, so identify OpenAI-compatible
	// API-key auths from their stable provider/config markers as well.
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	if strings.TrimSpace(auth.Attributes["compat_name"]) != "" || strings.TrimSpace(auth.Attributes["provider_key"]) != "" {
		return true
	}
	return false
}

func openAICompatProviderKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if providerKey := strings.TrimSpace(auth.Attributes["provider_key"]); providerKey != "" {
			return strings.ToLower(providerKey)
		}
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			return strings.ToLower(compatName)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func openAICompatModelPoolKey(auth *Auth, requestedModel string) string {
	base := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if base == "" {
		base = strings.TrimSpace(requestedModel)
	}
	return strings.ToLower(strings.TrimSpace(auth.ID)) + "|" + openAICompatProviderKey(auth) + "|" + strings.ToLower(base)
}

func (m *Manager) nextModelPoolOffset(key string, size int) int {
	if m == nil || size <= 1 {
		return 0
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelPoolOffsets == nil {
		m.modelPoolOffsets = make(map[string]int)
	}
	offset := m.modelPoolOffsets[key]
	if offset >= 2_147_483_640 {
		offset = 0
	}
	m.modelPoolOffsets[key] = offset + 1
	if size <= 0 {
		return 0
	}
	return offset % size
}

func rotateStrings(values []string, offset int) []string {
	if len(values) <= 1 {
		return values
	}
	if offset <= 0 {
		out := make([]string, len(values))
		copy(out, values)
		return out
	}
	offset = offset % len(values)
	out := make([]string, 0, len(values))
	out = append(out, values[offset:]...)
	out = append(out, values[:offset]...)
	return out
}

func (m *Manager) resolveOpenAICompatUpstreamModelPool(auth *Auth, requestedModel string) []string {
	if m == nil || !isOpenAICompatAPIKeyAuth(auth) {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func preserveRequestedModelSuffix(requestedModel, resolved string) string {
	return preserveResolvedModelSuffix(resolved, thinking.ParseSuffix(requestedModel))
}

func (m *Manager) executionModelCandidates(auth *Auth, routeModel string) []string {
	if auth != nil && auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			return []string{homeModel}
		}
	}
	requestedModel := rewriteModelForAuth(routeModel, auth)
	requestedModel = m.applyOAuthModelAlias(auth, requestedModel)
	if pool := m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		if len(pool) == 1 {
			return pool
		}
		offset := m.nextModelPoolOffset(openAICompatModelPoolKey(auth, requestedModel), len(pool))
		return rotateStrings(pool, offset)
	}
	resolved := m.applyAPIKeyModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolved) == "" {
		resolved = requestedModel
	}
	return []string{resolved}
}

func (m *Manager) selectionModelForAuth(auth *Auth, routeModel string) string {
	requestedModel := rewriteModelForAuth(routeModel, auth)
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = strings.TrimSpace(routeModel)
	}
	resolvedModel := m.applyOAuthModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolvedModel) == "" {
		resolvedModel = requestedModel
	}
	return resolvedModel
}

func (m *Manager) selectionModelKeyForAuth(auth *Auth, routeModel string) string {
	return canonicalModelKey(m.selectionModelForAuth(auth, routeModel))
}

func (m *Manager) stateModelForExecution(auth *Auth, routeModel, upstreamModel string, pooled bool) string {
	if auth != nil && auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			if resolved := strings.TrimSpace(upstreamModel); resolved != "" {
				return resolved
			}
			return homeModel
		}
	}
	stateModel := executionResultModel(routeModel, upstreamModel, pooled)
	selectionModel := m.selectionModelForAuth(auth, routeModel)
	if canonicalModelKey(selectionModel) == canonicalModelKey(upstreamModel) && strings.TrimSpace(selectionModel) != "" {
		return strings.TrimSpace(upstreamModel)
	}
	return stateModel
}

func executionResultModel(routeModel, upstreamModel string, pooled bool) string {
	if pooled {
		if resolved := strings.TrimSpace(upstreamModel); resolved != "" {
			return resolved
		}
	}
	if requested := strings.TrimSpace(routeModel); requested != "" {
		return requested
	}
	return strings.TrimSpace(upstreamModel)
}

func (m *Manager) filterExecutionModels(auth *Auth, routeModel string, candidates []string, pooled bool) []string {
	if len(candidates) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]string, 0, len(candidates))
	for _, upstreamModel := range candidates {
		stateModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
		blocked, _, _ := isAuthBlockedForModel(auth, stateModel, now)
		if blocked {
			continue
		}
		out = append(out, upstreamModel)
	}
	return out
}

func (m *Manager) preparedExecutionModels(auth *Auth, routeModel string) ([]string, bool) {
	candidates := m.executionModelCandidates(auth, routeModel)
	pooled := len(candidates) > 1
	return m.filterExecutionModels(auth, routeModel, candidates, pooled), pooled
}

func cloneAuthForExecution(provider string, auth *Auth) *Auth {
	if auth == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return auth.CloneShallow()
	default:
		return auth.cloneForExecution()
	}
}

func (m *Manager) clonePickedAuthForExecution(provider string, selected *Auth) (auth *Auth, stale bool) {
	if selected == nil {
		return nil, false
	}
	if m == nil || selected.ID == "" {
		return cloneAuthForExecution(provider, selected), false
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	m.mu.RLock()
	current := m.auths[selected.ID]
	if current == nil || executionAuthIsStale(providerKey, current) {
		m.mu.RUnlock()
		return nil, true
	}
	if current.indexAssigned {
		cloned := cloneAuthForExecution(provider, current)
		m.mu.RUnlock()
		return cloned, false
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	current = m.auths[selected.ID]
	if current == nil || executionAuthIsStale(providerKey, current) {
		return nil, true
	}
	if !current.indexAssigned {
		current.EnsureIndex()
	}
	return cloneAuthForExecution(provider, current), false
}

func executionAuthIsStale(providerKey string, auth *Auth) bool {
	if auth == nil || providerKey == "" || auth.Disabled || auth.Status == StatusDisabled {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(auth.Provider), providerKey)
}

func (m *Manager) prepareExecutionModels(auth *Auth, routeModel string) []string {
	models, _ := m.preparedExecutionModels(auth, routeModel)
	return models
}

func (m *Manager) availableAuthsForRouteModel(auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority := make(map[int][]*Auth)
	cooldownCount := 0
	var earliest time.Time
	for _, candidate := range auths {
		checkModel := m.selectionModelForAuth(candidate, routeModel)
		blocked, reason, next := isAuthBlockedForModel(candidate, checkModel, now)
		if !blocked {
			priority := authPriority(candidate)
			availableByPriority[priority] = append(availableByPriority[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}

	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(routeModel, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	bestPriority := 0
	found := false
	for priority := range availableByPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}

	available := availableByPriority[bestPriority]
	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, nil
}

func selectionArgForSelector(selector Selector, routeModel string) string {
	if isBuiltInSelector(selector) {
		return ""
	}
	return routeModel
}

func (m *Manager) selectorForProvider(provider string) Selector {
	if m == nil {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(provider), "kiro") {
		m.mu.RLock()
		selector := m.kiroSelector
		m.mu.RUnlock()
		if selector != nil {
			return selector
		}
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	return selector
}

func (m *Manager) authSupportsRouteModel(registryRef *registry.ModelRegistry, auth *Auth, routeModel string) bool {
	if registryRef == nil || auth == nil {
		return true
	}
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return true
	}
	if registryRef.ClientSupportsModel(auth.ID, routeKey) {
		return true
	}
	selectionKey := m.selectionModelKeyForAuth(auth, routeModel)
	return selectionKey != "" && selectionKey != routeKey && registryRef.ClientSupportsModel(auth.ID, selectionKey)
}

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func (e *streamBootstrapError) StatusCode() int {
	if e == nil || e.cause == nil {
		return 0
	}
	if se, ok := e.cause.(interface{ StatusCode() int }); ok && se != nil {
		return se.StatusCode()
	}
	return 0
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func invalidStreamResultError(message string) *Error {
	return &Error{Code: "invalid_stream_result", Message: message, Retryable: true, HTTPStatus: http.StatusBadGateway}
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var failed bool
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				streamResult := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false}
				applyResultError(&streamResult, chunk.Err)
				m.MarkResult(ctx, streamResult)
			}
			if !forward {
				return false
			}
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if !failed {
			m.MarkResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: true})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel string, execModels []string, pooled bool) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	ctx = WithRefreshUpdateCallback(ctx, m.handleExecutionRefreshUpdate)
	ctx = WithAuthUpdateCallback(ctx, m.handleExecutionAuthUpdate)
	ctx = WithRefreshCoordinator(ctx, m.coordinatedRefreshForRequest)
	var lastErr error
	for idx, execModel := range execModels {
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		streamResult, errStream := executor.ExecuteStream(ctx, auth, execReq, opts)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false}
			applyResultError(&result, errStream)
			result.RetryAfter = retryAfterFromError(errStream)
			m.MarkResult(ctx, result)
			clearSelectedAuthMetadataForCredentialFailover(provider, opts.Metadata, auth.ID, errStream)
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}
		if streamResult == nil {
			invalidErr := invalidStreamResultError("upstream executor returned nil stream result")
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: invalidErr}
			m.MarkResult(ctx, result)
			if idx < len(execModels)-1 {
				lastErr = invalidErr
				continue
			}
			return nil, newStreamBootstrapError(invalidErr, nil)
		}
		if streamResult.Chunks == nil {
			invalidErr := invalidStreamResultError("upstream executor returned stream result without chunks")
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: invalidErr}
			m.MarkResult(ctx, result)
			if idx < len(execModels)-1 {
				lastErr = invalidErr
				continue
			}
			return nil, newStreamBootstrapError(invalidErr, streamResult.Headers)
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCtx
			}
			if isRequestInvalidError(bootstrapErr) {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			m.MarkResult(ctx, result)
			discardStreamChunks(streamResult.Chunks)
			return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true, HTTPStatus: http.StatusBadGateway}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr}
			m.MarkResult(ctx, result)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, newStreamBootstrapError(emptyErr, streamResult.Headers)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, streamResult.Headers, buffered, remaining), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, lastErr
}

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}

		byAlias := make(map[string]string)
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "gemini":
			if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "claude":
			if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "codex":
			if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "vertex":
			if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		default:
			// OpenAI-compat uses config selection from auth.Attributes.
			providerKey := ""
			compatName := ""
			if auth.Attributes != nil {
				providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			}
			if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
				if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
					compileAPIKeyModelAliasForModels(byAlias, entry.Models)
				}
			}
		}

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	m.apiKeyModelAlias.Store(out)
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	previousMaxRetryCredentials := m.maxRetryCredentials.Load()
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
	if maxRetryCredentials == 1 && previousMaxRetryCredentials != 1 {
		log.Warn("max-retry-credentials=1 allows only the initially selected credential; cross-credential failover will stop before trying another auth. Use 0 for unlimited failover or a value greater than 1 to cap it.")
	}
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	applyDefaultRefreshInterval(auth)
	auth.EnsureIndex()
	auth.ApplyRuntimeStateFromMetadata()
	m.applyProxyPoolLease(ctx, auth)
	m.mu.Lock()
	if m.shouldSuppressRemovedAuthUpsertLocked(auth) {
		m.mu.Unlock()
		return nil, nil
	}
	m.applyPersistedRuntimeStateLocked(auth)
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.clearRouteAwareCaches()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone.CloneForScheduler())
	}
	m.queueRefreshReschedule(auth.ID)
	_ = m.persist(ctx, auth)
	if errRuntime := m.persistRuntimeState(ctx, auth); errRuntime != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth runtime state: %v", errRuntime)
	}
	m.reconcileProxyPoolLeasesAfterAuthChange(ctx)
	if current, okCurrent := m.GetByID(auth.ID); okCurrent && current != nil {
		auth = current
	}
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	applyDefaultRefreshInterval(auth)
	m.applyProxyPoolLease(ctx, auth)
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	if isRefreshUpdate(ctx) && (!ok || authRefreshSuppressed(existing)) {
		var existingClone *Auth
		if existing != nil {
			existingClone = existing.Clone()
		}
		m.mu.Unlock()
		return existingClone, nil
	}
	if !ok && m.shouldSuppressRemovedAuthUpsertLocked(auth) {
		m.mu.Unlock()
		return nil, nil
	}
	if ok && existing != nil {
		if isRefreshUpdate(ctx) {
			preserveEditableAuthFileOptions(existing, auth)
		}
		if !auth.indexAssigned && auth.Index == "" {
			auth.Index = existing.Index
			auth.indexAssigned = existing.indexAssigned
		}
		preserveRuntimeState(existing, auth)
		if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
		}
	} else {
		auth.ApplyRuntimeStateFromMetadata()
		m.applyPersistedRuntimeStateLocked(auth)
	}
	auth.EnsureIndex()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.clearRouteAwareCaches()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone.CloneForScheduler())
	}
	m.queueRefreshReschedule(auth.ID)
	if m.shouldReleaseProxyLeaseForAuth(authClone) {
		m.releaseProxyLease(ctx, auth.ID)
		clearProxyPoolLease(auth)
	}
	persistErr := m.persist(ctx, auth)
	if errRuntime := m.persistRuntimeState(ctx, auth); errRuntime != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth runtime state: %v", errRuntime)
	}
	if !isRefreshUpdate(ctx) {
		m.reconcileProxyPoolLeasesAfterAuthChange(ctx)
		if current, okCurrent := m.GetByID(auth.ID); okCurrent && current != nil {
			auth = current
		}
	}
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	if persistErr != nil {
		return auth.Clone(), persistErr
	}
	return auth.Clone(), nil
}

// Remove deletes an auth entry from the in-memory manager state. The backing
// credential store is intentionally not touched here; callers that delete an
// auth file or remote credential record must do that explicitly before or after
// removing the runtime entry.
func (m *Manager) Remove(ctx context.Context, id string) (*Auth, error) {
	if m == nil {
		return nil, nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var removed *Auth
	var loop *authAutoRefreshLoop
	var runtimeStore RuntimeStateStore
	now := time.Now()
	m.mu.Lock()
	if existing := m.auths[id]; existing != nil {
		removed = existing.Clone()
		delete(m.auths, id)
		if tombstone, ok := authRemovalTombstoneFor(removed, now); ok {
			if m.removedAuths == nil {
				m.removedAuths = make(map[string]authRemovalTombstone)
			}
			m.removedAuths[id] = tombstone
		}
	}
	if m.runtimeStates != nil {
		delete(m.runtimeStates, id)
	}
	poolAuthID := strings.ToLower(id)
	for key := range m.modelPoolOffsets {
		if authIDFromModelPoolOffsetKey(key) == poolAuthID {
			delete(m.modelPoolOffsets, key)
		}
	}
	loop = m.refreshLoop
	runtimeStore = m.runtimeStateStore
	m.mu.Unlock()

	if authProxyPoolAssigned(removed) {
		m.releaseProxyLease(ctx, id)
	}
	m.discardPendingPersistAuthID(id)
	if loop != nil {
		loop.remove(id)
	}
	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	m.clearRouteAwareCaches()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	registry.GetGlobalRegistry().UnregisterClient(id)

	var runtimeErr error
	if runtimeStore != nil && !shouldSkipPersist(ctx) {
		if err := runtimeStore.Delete(ctx, id); err != nil {
			runtimeErr = err
		}
	}

	if removed != nil {
		notification := removed.Clone()
		notification.Disabled = true
		notification.Status = StatusDisabled
		if strings.TrimSpace(notification.StatusMessage) == "" {
			notification.StatusMessage = "removed"
		}
		notification.UpdatedAt = now
		m.hook.OnAuthUpdated(ctx, notification)
	}
	return removed, runtimeErr
}

func authIDFromModelPoolOffsetKey(key string) string {
	if key == "" {
		return ""
	}
	if idx := strings.IndexByte(key, '|'); idx >= 0 {
		return key[:idx]
	}
	return key
}

func (m *Manager) applyProxyPoolLease(ctx context.Context, auth *Auth) {
	if m == nil || auth == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if !proxyPoolCanAssign(cfg, auth) {
		if authProxyPoolAssigned(auth) {
			m.releaseProxyLease(ctx, auth.ID)
			clearProxyPoolLease(auth)
			return
		}
		if strings.TrimSpace(auth.ID) != "" && strings.TrimSpace(auth.ProxyURL) != "" && !authProxyPoolAssigned(auth) {
			m.releaseProxyLease(ctx, auth.ID)
		}
		return
	}
	m.mu.RLock()
	store := m.proxyLeaseStore
	m.mu.RUnlock()
	if store == nil {
		return
	}
	lease, ok, err := store.AcquireProxyLease(ctx, auth.ID, cfg.ProxyPool.Proxies)
	if err != nil {
		log.WithField("auth_id", auth.ID).Warnf("proxy-pool: failed to acquire proxy lease: %v", err)
		return
	}
	if !ok || strings.TrimSpace(lease.ProxyURL) == "" {
		if authProxyPoolAssigned(auth) {
			clearProxyPoolLease(auth)
			m.clearRuntimeProxyPoolLease(auth.ID)
		}
		log.WithField("auth_id", auth.ID).Warn("proxy-pool: no available proxy lease")
		return
	}
	markProxyPoolLease(auth, lease.ProxyURL)
	var schedulerSnapshot *Auth
	m.mu.Lock()
	if current := m.auths[auth.ID]; current != nil {
		markProxyPoolLease(current, lease.ProxyURL)
		schedulerSnapshot = current.CloneForScheduler()
	}
	m.mu.Unlock()
	if m.scheduler != nil && schedulerSnapshot != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
}

func (m *Manager) releaseProxyLease(ctx context.Context, authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	store := m.proxyLeaseStore
	m.mu.RUnlock()
	if store != nil {
		if err := store.ReleaseProxyLease(ctx, authID); err != nil {
			log.WithField("auth_id", authID).Warnf("proxy-pool: failed to release proxy lease: %v", err)
		}
	}
	m.clearRuntimeProxyPoolLease(authID)
}

func (m *Manager) clearRuntimeProxyPoolLease(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	var schedulerSnapshot *Auth
	m.mu.Lock()
	if current := m.auths[authID]; current != nil && authProxyPoolAssigned(current) {
		clearProxyPoolLease(current)
		schedulerSnapshot = current.CloneForScheduler()
	}
	m.mu.Unlock()
	if m.scheduler != nil && schedulerSnapshot != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
}

func (m *Manager) shouldReleaseProxyLeaseForAuth(auth *Auth) bool {
	if m == nil || auth == nil || !authProxyPoolAssigned(auth) {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.ProxyPool.Enabled {
		return true
	}
	return cfg.ProxyPool.ReleaseOnAuthDisabled && (auth.Disabled || auth.Status == StatusDisabled)
}

func (m *Manager) shouldReleaseProxyLeaseForResult(result Result) bool {
	if m == nil || result.Success || strings.TrimSpace(result.AuthID) == "" {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.ProxyPool.Enabled {
		return false
	}
	statusCode := statusCodeFromResult(result.Error)
	if statusCode == http.StatusTooManyRequests {
		return cfg.ProxyPool.ReleaseOnAuthCooldown
	}
	return result.AuthScoped || isAuthWideResultError(result.Error) || statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden
}

func proxyPoolSortedAssignableAuths(cfg *internalconfig.Config, auths []*Auth) []*Auth {
	if cfg == nil || !cfg.ProxyPool.Enabled || len(cfg.ProxyPool.Proxies) == 0 || len(auths) == 0 {
		return nil
	}
	out := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if proxyPoolCanAssign(cfg, auth) {
			out = append(out, auth)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := out[i]
		right := out[j]
		leftPriority := authPriority(left)
		rightPriority := authPriority(right)
		if leftPriority != rightPriority {
			return leftPriority > rightPriority
		}
		leftID := ""
		if left != nil {
			leftID = strings.TrimSpace(left.ID)
		}
		rightID := ""
		if right != nil {
			rightID = strings.TrimSpace(right.ID)
		}
		return leftID < rightID
	})
	return out
}

func proxyPoolSelectedLeaseAuthIDs(cfg *internalconfig.Config, auths []*Auth) []string {
	if cfg == nil || !cfg.ProxyPool.Enabled || len(cfg.ProxyPool.Proxies) == 0 || len(auths) == 0 {
		return nil
	}
	limit := 0
	for _, proxyURL := range cfg.ProxyPool.Proxies {
		if strings.TrimSpace(proxyURL) != "" {
			limit++
		}
	}
	if limit <= 0 {
		return nil
	}
	out := make([]string, 0, min(len(auths), limit))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		authID := strings.TrimSpace(auth.ID)
		if authID == "" {
			continue
		}
		out = append(out, authID)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (m *Manager) recordProxyPoolResult(ctx context.Context, result Result) {
	if m == nil || strings.TrimSpace(result.AuthID) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.ProxyPool.Enabled {
		return
	}
	proxyURL := m.proxyPoolAssignedProxyURL(result.AuthID)
	if proxyURL == "" {
		return
	}
	m.mu.RLock()
	store := m.proxyLeaseStore
	m.mu.RUnlock()
	if store == nil {
		return
	}
	if result.Success {
		if err := store.ClearProxyLeaseFailure(ctx, proxyURL); err != nil {
			log.WithField("proxy_url", proxyURL).Warnf("proxy-pool: failed to clear proxy failure state: %v", err)
		}
		return
	}
	threshold := cfg.ProxyPool.ProxyFailureThreshold
	if threshold <= 0 || !isProxyPoolTransportFailure(result.Error) {
		return
	}
	failure, err := store.RecordProxyLeaseFailure(ctx, result.AuthID, proxyURL, threshold, proxyFailureCooldown(cfg))
	if err != nil {
		log.WithFields(log.Fields{
			"auth_id":   result.AuthID,
			"proxy_url": proxyURL,
		}).Warnf("proxy-pool: failed to record proxy failure: %v", err)
		return
	}
	if !failure.CooledDown {
		return
	}
	log.WithFields(log.Fields{
		"auth_id":    result.AuthID,
		"proxy_url":  proxyURL,
		"failures":   failure.Failures,
		"recover_at": failure.RecoverAt,
	}).Warn("proxy-pool: proxy entered cooldown after transport failures")
	m.clearRuntimeProxyPoolLease(result.AuthID)
}

func (m *Manager) proxyPoolAssignedProxyURL(authID string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth := m.auths[authID]
	if !authProxyPoolAssigned(auth) {
		return ""
	}
	return strings.TrimSpace(auth.ProxyURL)
}

func proxyFailureCooldown(cfg *internalconfig.Config) time.Duration {
	if cfg == nil {
		return defaultProxyFailureCooldown
	}
	raw := strings.TrimSpace(cfg.ProxyPool.ProxyFailureCooldown)
	if raw == "" {
		return defaultProxyFailureCooldown
	}
	cooldown, err := time.ParseDuration(raw)
	if err != nil || cooldown <= 0 {
		return defaultProxyFailureCooldown
	}
	return cooldown
}

func isProxyPoolTransportFailure(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != 0 {
		return false
	}
	raw := strings.ToLower(strings.TrimSpace(strings.Join([]string{err.Code, err.Message}, " ")))
	if raw == "" {
		return false
	}
	patterns := [...]string{
		"connection refused",
		"connection reset",
		"connection timed out",
		"context deadline exceeded",
		"dial tcp",
		"i/o timeout",
		"net/http: timeout",
		"network is unreachable",
		"no such host",
		"proxyconnect",
		"proxy connect",
		"proxy connection",
		"socks",
		"socks5",
		"temporary failure in name resolution",
		"tls handshake timeout",
		"use of closed network connection",
	}
	for _, pattern := range patterns {
		if strings.Contains(raw, pattern) {
			return true
		}
	}
	return false
}

func proxyPoolCanAssign(cfg *internalconfig.Config, auth *Auth) bool {
	if cfg == nil || !cfg.ProxyPool.Enabled || len(cfg.ProxyPool.Proxies) == 0 || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if strings.TrimSpace(auth.ProxyURL) != "" && !authProxyPoolAssigned(auth) {
		return false
	}
	kind, _ := auth.AccountInfo()
	if strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return false
	}
	return true
}

func markProxyPoolLease(auth *Auth, proxyURL string) {
	if auth == nil {
		return
	}
	auth.ProxyURL = strings.TrimSpace(proxyURL)
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string, 1)
	}
	auth.Attributes[proxyPoolAssignedAttribute] = proxyPoolAssignedValue
}

func clearProxyPoolLease(auth *Auth) {
	if auth == nil {
		return
	}
	auth.ProxyURL = ""
	if auth.Attributes != nil {
		delete(auth.Attributes, proxyPoolAssignedAttribute)
	}
}

func authProxyPoolAssigned(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes[proxyPoolAssignedAttribute]), proxyPoolAssignedValue)
}

func stripProxyPoolLeaseForPersist(auth *Auth) {
	if !authProxyPoolAssigned(auth) {
		return
	}
	clearProxyPoolLease(auth)
}

func (m *Manager) shouldSuppressRemovedAuthUpsertLocked(auth *Auth) bool {
	if m == nil || auth == nil || strings.TrimSpace(auth.ID) == "" || len(m.removedAuths) == 0 {
		return false
	}
	tombstone, ok := m.removedAuths[auth.ID]
	if !ok {
		return false
	}
	if authRestoresRemovedFile(auth, tombstone) {
		delete(m.removedAuths, auth.ID)
		return false
	}
	return true
}

func authRestoresRemovedFile(auth *Auth, tombstone authRemovalTombstone) bool {
	path := authFilePathForRemoval(auth)
	if path == "" {
		path = strings.TrimSpace(tombstone.Path)
	}
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if tombstone.RemovedAt.IsZero() {
		return true
	}
	if info.ModTime().After(tombstone.RemovedAt) {
		return true
	}
	return false
}

func authRemovalTombstoneFor(auth *Auth, now time.Time) (authRemovalTombstone, bool) {
	if auth == nil || auth.ID == "" || authRuntimeOnly(auth) {
		return authRemovalTombstone{}, false
	}
	path := authFilePathForRemoval(auth)
	if path == "" {
		return authRemovalTombstone{}, false
	}
	return authRemovalTombstone{Path: path, RemovedAt: now}, true
}

func authFilePathForRemoval(auth *Auth) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["path"])
}

func authRuntimeOnly(auth *Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	m.removedAuths = make(map[string]authRemovalTombstone)
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		auth.EnsureIndex()
		applyDefaultRefreshInterval(auth)
		auth.ApplyRuntimeStateFromMetadata()
		m.applyPersistedRuntimeStateLocked(auth)
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	opts = ensureRequestedModelMetadata(opts, req.Model)
	ensureOptionsMetadata(&opts)

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := m.executeMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		if hasAntigravityProvider(normalized) && shouldAttemptAntigravityCreditsFallback(m, lastErr, normalized) {
			if resp, ok := m.tryAntigravityCreditsExecute(ctx, req, opts); ok {
				return resp, nil
			}
		}
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	opts = ensureRequestedModelMetadata(opts, req.Model)
	ensureOptionsMetadata(&opts)

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := m.executeCountMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	opts = ensureRequestedModelMetadata(opts, req.Model)
	ensureOptionsMetadata(&opts)

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		result, errStream := m.executeStreamMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errStream == nil {
			return result, nil
		}
		lastErr = errStream
		wait, shouldRetry := m.shouldRetryAfterError(errStream, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return nil, errWait
		}
	}
	if lastErr != nil {
		if hasAntigravityProvider(normalized) && shouldAttemptAntigravityCreditsFallback(m, lastErr, normalized) {
			if result, ok := m.tryAntigravityCreditsExecuteStream(ctx, req, opts); ok {
				return result, nil
			}
		}
		var bootstrapErr *streamBootstrapError
		if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
			return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
		}
		return nil, lastErr
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
}

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	ensureOptionsMetadata(&opts)
	tried := borrowAuthIDSet()
	defer releaseAuthIDSet(tried)
	attempted := borrowAuthIDSet()
	defer releaseAuthIDSet(attempted)
	var lastErr error
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	for {
		if !homeMode && maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			return cliproxyexecutor.Response{}, m.credentialRetryLimitReachedError(ctx, "execute", providers, routeModel, opts, maxRetryCredentials, attempted, lastErr)
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		if log.IsLevelEnabled(log.DebugLevel) {
			debugLogAuthSelection(logEntryWithRequestID(ctx), auth, provider, req.Model)
		}
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)
		execCtx = WithRefreshUpdateCallback(execCtx, m.handleExecutionRefreshUpdate)
		execCtx = WithAuthUpdateCallback(execCtx, m.handleExecutionAuthUpdate)
		execCtx = WithRefreshCoordinator(execCtx, m.coordinatedRefreshForRequest)

		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: &Error{Message: errPrepare.Error()}}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errPrepare); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			m.MarkResult(execCtx, result)
			forceNewUpstreamSessionForNextCredential(&opts)
			lastErr = errPrepare
			continue
		}
		var authErr error
		for _, upstreamModel := range models {
			resultModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			resp, errExec := executor.Execute(execCtx, auth, execReq, opts)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil}
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					return cliproxyexecutor.Response{}, errCtx
				}
				applyResultError(&result, errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.MarkResult(execCtx, result)
				clearSelectedAuthMetadataForCredentialFailover(provider, opts.Metadata, auth.ID, errExec)
				if isRequestInvalidError(errExec) {
					return cliproxyexecutor.Response{}, errExec
				}
				authErr = errExec
				continue
			}
			m.MarkResult(execCtx, result)
			return resp, nil
		}
		if authErr != nil {
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr
			}
			forceNewUpstreamSessionForNextCredential(&opts)
			lastErr = authErr
			if homeMode {
				homeAuthCount++
			}
			continue
		}
	}
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	ensureOptionsMetadata(&opts)
	tried := borrowAuthIDSet()
	defer releaseAuthIDSet(tried)
	attempted := borrowAuthIDSet()
	defer releaseAuthIDSet(attempted)
	var lastErr error
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	for {
		if !homeMode && maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			return cliproxyexecutor.Response{}, m.credentialRetryLimitReachedError(ctx, "execute_count", providers, routeModel, opts, maxRetryCredentials, attempted, lastErr)
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		if log.IsLevelEnabled(log.DebugLevel) {
			debugLogAuthSelection(logEntryWithRequestID(ctx), auth, provider, req.Model)
		}
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)

		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: &Error{Message: errPrepare.Error()}}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errPrepare); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			m.MarkResult(execCtx, result)
			forceNewUpstreamSessionForNextCredential(&opts)
			lastErr = errPrepare
			continue
		}
		var authErr error
		for _, upstreamModel := range models {
			resultModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			resp, errExec := executor.CountTokens(execCtx, auth, execReq, opts)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil}
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					return cliproxyexecutor.Response{}, errCtx
				}
				applyResultError(&result, errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.MarkResult(execCtx, result)
				clearSelectedAuthMetadataForCredentialFailover(provider, opts.Metadata, auth.ID, errExec)
				if isRequestInvalidError(errExec) {
					return cliproxyexecutor.Response{}, errExec
				}
				authErr = errExec
				continue
			}
			m.MarkResult(execCtx, result)
			return resp, nil
		}
		if authErr != nil {
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr
			}
			forceNewUpstreamSessionForNextCredential(&opts)
			lastErr = authErr
			if homeMode {
				homeAuthCount++
			}
			continue
		}
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (*cliproxyexecutor.StreamResult, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	ensureOptionsMetadata(&opts)
	tried := borrowAuthIDSet()
	defer releaseAuthIDSet(tried)
	attempted := borrowAuthIDSet()
	defer releaseAuthIDSet(attempted)
	var lastErr error
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	for {
		if !homeMode && maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			return nil, m.credentialRetryLimitReachedError(ctx, "execute_stream", providers, routeModel, opts, maxRetryCredentials, attempted, lastErr)
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				return nil, lastErr
			}
			return nil, errPick
		}

		if log.IsLevelEnabled(log.DebugLevel) {
			debugLogAuthSelection(logEntryWithRequestID(ctx), auth, provider, req.Model)
		}
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: &Error{Message: errPrepare.Error()}}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errPrepare); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			m.MarkResult(execCtx, result)
			forceNewUpstreamSessionForNextCredential(&opts)
			lastErr = errPrepare
			continue
		}
		execReq := sanitizeDownstreamWebsocketFallbackRequest(execCtx, auth, req)
		streamResult, errStream := m.executeStreamWithModelPool(execCtx, executor, auth, provider, execReq, opts, routeModel, models, pooled)
		if errStream != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return nil, errCtx
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			forceNewUpstreamSessionForNextCredential(&opts)
			lastErr = errStream
			if homeMode {
				homeAuthCount++
			}
			continue
		}
		return streamResult, nil
	}
}

func sanitizeDownstreamWebsocketFallbackRequest(ctx context.Context, auth *Auth, req cliproxyexecutor.Request) cliproxyexecutor.Request {
	if !cliproxyexecutor.DownstreamWebsocket(ctx) || authWebsocketsEnabled(auth) || len(req.Payload) == 0 {
		return req
	}
	updated, errDelete := sjson.DeleteBytes(req.Payload, "generate")
	if errDelete != nil {
		return req
	}
	req.Payload = updated
	return req
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func withHomeAuthCount(opts cliproxyexecutor.Options, count int) cliproxyexecutor.Options {
	if count <= 0 {
		count = 1
	}
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any, 1)
	}
	opts.Metadata[homeAuthCountMetadataKey] = count
	return opts
}

func homeAuthCountFromMetadata(meta map[string]any) int {
	if len(meta) == 0 {
		return 1
	}
	switch value := meta[homeAuthCountMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	}
	return 1
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

type requestAuthPrepareLock struct {
	mu sync.Mutex
}

func (m *Manager) prepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	preparer, ok := executor.(RequestAuthPreparer)
	if !ok || preparer == nil || !preparer.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}

	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	lockValue, _ := m.requestPrepareLocks.LoadOrStore(id, &requestAuthPrepareLock{})
	lock, ok := lockValue.(*requestAuthPrepareLock)
	if !ok || lock == nil {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	lock.mu.Lock()
	defer lock.mu.Unlock()

	target := auth.Clone()
	m.mu.RLock()
	if current := m.auths[id]; current != nil {
		target = current.Clone()
	}
	m.mu.RUnlock()

	if !preparer.ShouldPrepareRequestAuth(target) {
		return target, nil
	}

	updated, errPrepare := preparer.PrepareRequestAuth(ctx, target)
	if errPrepare != nil {
		return auth, errPrepare
	}
	if updated == nil {
		return target, nil
	}

	saved, errUpdate := m.Update(ctx, updated)
	if errUpdate != nil {
		return updated, errUpdate
	}
	if saved != nil {
		return saved, nil
	}
	return updated, nil
}

func contextWithRequestedModelAlias(ctx context.Context, opts cliproxyexecutor.Options, fallback string) context.Context {
	alias := requestedModelAliasFromOptions(opts, fallback)
	ctx = coreusage.WithRequestedModelAlias(ctx, alias)
	effort := reasoningEffortFromOptions(opts)
	if effort != "" {
		ctx = coreusage.WithReasoningEffort(ctx, effort)
	}
	serviceTier := serviceTierFromOptions(opts)
	if serviceTier != "" {
		ctx = coreusage.WithServiceTier(ctx, serviceTier)
	}
	return ctx
}

func requestedModelAliasFromOptions(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		return strings.TrimSpace(value)
	case []byte:
		if len(value) == 0 {
			return fallback
		}
		return strings.TrimSpace(string(value))
	default:
		return fallback
	}
}

func reasoningEffortFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ReasoningEffortMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func serviceTierFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ServiceTierMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func withPinnedAuthMetadata(opts cliproxyexecutor.Options, authID string) cliproxyexecutor.Options {
	authID = strings.TrimSpace(authID)
	if authID == "" || pinnedAuthIDFromMetadata(opts.Metadata) == authID {
		return opts
	}
	metadata := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	metadata[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	opts.Metadata = metadata
	return opts
}

func ensureOptionsMetadata(opts *cliproxyexecutor.Options) {
	if opts == nil || opts.Metadata != nil {
		return
	}
	opts.Metadata = make(map[string]any, 1)
}

func forceNewUpstreamSessionForNextCredential(opts *cliproxyexecutor.Options) {
	if opts == nil {
		return
	}
	ensureOptionsMetadata(opts)
	opts.Metadata[cliproxyexecutor.ForcedUpstreamSessionMetadataKey] = uuid.NewString()
}

func isRecoverableAffinityPickError(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	switch authErr.Code {
	case "auth_not_found", "auth_unavailable", "model_cooldown":
		return true
	default:
		return false
	}
}

func recoverableAffinityPickErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return "model_cooldown"
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return strings.TrimSpace(authErr.Code)
	}
	return fmt.Sprintf("%T", err)
}

func logRecoverableAffinityPick(ctx context.Context, mode, providers, model, authID string, opts cliproxyexecutor.Options, err error) {
	if !log.IsLevelEnabled(log.InfoLevel) {
		return
	}
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	fields := log.Fields{
		"auth_id": authID,
		"mode":    mode,
	}
	if providers != "" {
		fields["provider"] = providers
	}
	if model != "" {
		fields["model"] = model
	}
	if reason := recoverableAffinityPickErrorCode(err); reason != "" {
		fields["reason"] = reason
	}
	if primaryID != "" {
		fields["session_id"] = truncateSessionID(primaryID)
	}
	if fallbackID != "" && fallbackID != primaryID {
		fields["fallback_session_id"] = truncateSessionID(fallbackID)
	}
	selectorLogEntry(ctx).WithFields(fields).Info("session-affinity: cached auth unavailable, reselecting")
}

func selectedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.SelectedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func pinSelectedKiroAuthForProvider(provider string, opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if !strings.EqualFold(strings.TrimSpace(provider), "kiro") {
		return opts
	}
	return pinSelectedKiroAuth(opts)
}

func pinSelectedKiroAuthForProviders(providers []string, opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if len(providers) != 1 || !strings.EqualFold(strings.TrimSpace(providers[0]), "kiro") {
		return opts
	}
	return pinSelectedKiroAuth(opts)
}

func pinSelectedKiroAuth(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return opts
	}
	selected := selectedAuthIDFromMetadata(opts.Metadata)
	if selected == "" {
		return opts
	}
	metadata := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	metadata[cliproxyexecutor.PinnedAuthMetadataKey] = selected
	opts.Metadata = metadata
	return opts
}

func withProviderScopeMetadata(opts cliproxyexecutor.Options, providers []string) cliproxyexecutor.Options {
	providers = normalizeProviderKeys(providers)
	if len(providers) == 0 {
		return opts
	}
	scope := "providers:" + strings.Join(providers, ",")
	if metadataStringValue(opts.Metadata, cliproxyexecutor.ProviderScopeMetadataKey) == scope {
		return opts
	}
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any, 1)
	}
	opts.Metadata[cliproxyexecutor.ProviderScopeMetadataKey] = scope
	return opts
}

func disallowFreeAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.DisallowFreeAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(val))
		return err == nil && parsed
	case []byte:
		parsed, err := strconv.ParseBool(strings.TrimSpace(string(val)))
		return err == nil && parsed
	default:
		return false
	}
}

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
}

func clearSelectedAuthMetadataForCredentialFailover(provider string, meta map[string]any, authID string, err error) {
	if len(meta) == 0 || !isCredentialFailoverFailure(err) {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "kiro") {
		return
	}
	if selectedAuthIDFromMetadata(meta) != strings.TrimSpace(authID) {
		return
	}
	delete(meta, cliproxyexecutor.SelectedAuthMetadataKey)
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return resolved
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return upstreamModel
	}
	return requestedModel
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
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
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(cfg *internalconfig.Config, providerKey, compatName, authProvider string) *internalconfig.OpenAICompatibility {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

func (m *Manager) retrySettings() (int, int, time.Duration) {
	if m == nil {
		return 0, 0, 0
	}
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load())
}

func borrowAuthIDSet() map[string]struct{} {
	raw := authIDSetPool.Get()
	if raw == nil {
		return make(map[string]struct{}, 16)
	}
	set, ok := raw.(map[string]struct{})
	if !ok || set == nil {
		return make(map[string]struct{}, 16)
	}
	return set
}

func releaseAuthIDSet(set map[string]struct{}) {
	if set == nil {
		return
	}
	if len(set) > 4096 {
		return
	}
	clear(set)
	authIDSetPool.Put(set)
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		blocked, reason, next := isAuthBlockedForModel(auth, checkModel, now)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) retryAllowed(attempt int, providers []string) bool {
	if m == nil || attempt < 0 || len(providers) == 0 {
		return false
	}
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt < effectiveRetry {
			return true
		}
	}
	return false
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	status := statusCodeFromError(err)
	if status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	wait, found := m.closestCooldownWait(providers, model, attempt)
	if found {
		if wait > maxWait {
			return 0, false
		}
		return wait, true
	}
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if !m.retryAllowed(attempt, providers) {
		return 0, false
	}
	retryAfter := retryAfterFromError(err)
	if retryAfter == nil || *retryAfter <= 0 || *retryAfter > maxWait {
		return 0, false
	}
	return *retryAfter, true
}

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	if shouldSkipFailureResultRecord(result) {
		return
	}
	if m.markCleanModelSuccessResult(ctx, result) {
		return
	}

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	schedulerDirty := false
	persistAuthID := ""
	var schedulerSnapshot *Auth
	invalidateAuthAffinity := false

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()
		auth.recordRuntimeResult(now, result.Success)
		persistAuthID = auth.ID

		if result.Success {
			if result.Model != "" {
				state := lookupModelState(auth, result.Model)
				if !authStateIsClean(auth) || (state != nil && !modelStateIsClean(state)) {
					state = ensureModelState(auth, result.Model)
					resetModelState(state, now)
					updateAggregatedAvailability(auth, now)
					if !hasModelError(auth, now) {
						auth.LastError = nil
						auth.StatusMessage = ""
						auth.Status = StatusActive
					}
					auth.UpdatedAt = now
					shouldResumeModel = true
					clearModelQuota = true
					schedulerDirty = true
				}
			} else {
				if !authStateIsClean(auth) {
					schedulerDirty = true
				}
				clearAuthStateOnSuccess(auth, now)
			}
		} else {
			authWideFailure := result.AuthScoped || isAuthWideResultError(result.Error)
			if result.Model != "" && !authWideFailure {
				if !isRequestScopedNotFoundResultError(result.Error) {
					disableCooling := quotaCooldownDisabledForAuth(auth)
					state := ensureModelState(auth, result.Model)
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
						auth.LastError = cloneError(result.Error)
						auth.StatusMessage = result.Error.Message
					}

					statusCode := statusCodeFromResult(result.Error)
					if isModelSupportResultError(result.Error) {
						next := now.Add(12 * time.Hour)
						state.NextRetryAfter = next
						suspendReason = "model_not_supported"
						shouldSuspendModel = true
					} else {
						switch statusCode {
						case 401:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "unauthorized"
								shouldSuspendModel = true
							}
						case 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "payment_required"
								shouldSuspendModel = true
							}
						case 404:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(12 * time.Hour)
								state.NextRetryAfter = next
								suspendReason = "not_found"
								shouldSuspendModel = true
							}
						case 429:
							var next time.Time
							backoffLevel := state.Quota.BackoffLevel
							if !disableCooling {
								if result.RetryAfter != nil {
									next = now.Add(*result.RetryAfter)
								} else {
									cooldown, nextLevel := nextQuotaCooldown(backoffLevel, disableCooling)
									if cooldown > 0 {
										next = now.Add(cooldown)
									}
									backoffLevel = nextLevel
								}
							}
							state.NextRetryAfter = next
							state.Quota = QuotaState{
								Exceeded:      true,
								Reason:        "quota",
								NextRecoverAt: next,
								BackoffLevel:  backoffLevel,
							}
							if !disableCooling {
								suspendReason = "quota"
								shouldSuspendModel = true
								setModelQuota = true
							}
						case 408, 500, 502, 503, 504:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(1 * time.Minute)
								state.NextRetryAfter = next
							}
						default:
							state.NextRetryAfter = time.Time{}
						}
					}

					auth.Status = StatusError
					auth.UpdatedAt = now
					updateAggregatedAvailability(auth, now)
					schedulerDirty = true
				}
			} else {
				// Auth-wide failures suspend the credential itself, not just the
				// triggering model. Kiro auth-scoped quota errors and generic 401s
				// both need future routing to move to a different auth file.
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
				// Mark auth-scoped failures so isAuthBlockedForModel treats every
				// model as unavailable, even ones with no per-model state.
				if result.AuthScoped || isAuthWideResultError(result.Error) {
					auth.Quota.AuthScope = true
				}
				if auth.Unavailable && !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
					invalidateAuthAffinity = true
				}
				if result.Model != "" {
					state := ensureModelState(auth, result.Model)
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
					}
					if !auth.NextRetryAfter.IsZero() {
						state.NextRetryAfter = auth.NextRetryAfter
					}
					if auth.Quota.Exceeded {
						state.Quota = auth.Quota
						state.Quota.AuthScope = false
						setModelQuota = true
					}
					suspendReason = authWideSuspendReason(result.Error, result.AuthScoped)
					shouldSuspendModel = true
				}
				schedulerDirty = true
			}
		}

		if schedulerDirty {
			schedulerSnapshot = auth.CloneForScheduler()
		}
	}
	m.mu.Unlock()
	if persistAuthID != "" {
		m.enqueuePersistAuthID(ctx, persistAuthID)
	}
	if m.scheduler != nil && schedulerSnapshot != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	if invalidateAuthAffinity {
		m.invalidateSessionAffinityForAuth(result.AuthID)
	}

	if clearModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if setModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, result.Model)
	}
	if shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, result.Model)
	} else if shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, result.Model, suspendReason)
	}

	m.recordProxyPoolResult(ctx, result)
	if m.shouldReleaseProxyLeaseForResult(result) {
		m.releaseProxyLease(ctx, result.AuthID)
	}
	m.hook.OnResult(ctx, result)
}

func (m *Manager) invalidateSessionAffinityForAuth(authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	seen := make(map[*SessionAffinitySelector]struct{}, 2)
	invalidate := func(selector Selector) {
		affinity, ok := selector.(*SessionAffinitySelector)
		if !ok || affinity == nil {
			return
		}
		if _, okSeen := seen[affinity]; okSeen {
			return
		}
		seen[affinity] = struct{}{}
		affinity.InvalidateAuth(authID)
	}

	m.mu.RLock()
	selector := m.selector
	kiroSelector := m.kiroSelector
	m.mu.RUnlock()

	invalidate(selector)
	invalidate(kiroSelector)
}

func (m *Manager) markCleanModelSuccessResult(ctx context.Context, result Result) bool {
	if m == nil || !result.Success || result.AuthID == "" || result.Model == "" {
		return false
	}

	now := time.Now()
	persistAuthID := ""
	m.mu.RLock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		state := lookupModelState(auth, result.Model)
		if authStateIsClean(auth) && (state == nil || modelStateIsClean(state)) {
			auth.recordRuntimeResult(now, true)
			persistAuthID = auth.ID
		}
	}
	m.mu.RUnlock()
	if persistAuthID == "" {
		return false
	}
	m.enqueuePersistAuthID(ctx, persistAuthID)
	m.recordProxyPoolResult(ctx, result)
	m.hook.OnResult(ctx, result)
	return true
}

func shouldSkipFailureResultRecord(result Result) bool {
	if result.Success || result.Error == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(result.Provider), "kiro") {
		return false
	}
	return isKiroTransientModelCapacityResultError(result.Error)
}

func isKiroTransientModelCapacityResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(err.Message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"insufficient_model_capacity",
		"insufficient model capacity",
		"experiencing high traffic",
		"high traffic",
		"please try again shortly",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func lookupModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" || len(auth.ModelStates) == 0 {
		return nil
	}
	if state := auth.ModelStates[model]; state != nil {
		return state
	}
	baseModel := canonicalModelKey(model)
	if baseModel == "" || baseModel == model {
		return nil
	}
	return auth.ModelStates[baseModel]
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func modelStateIsClean(state *ModelState) bool {
	if state == nil {
		return true
	}
	if state.Status != StatusActive {
		return false
	}
	if state.Unavailable || state.StatusMessage != "" || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		return false
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		return false
	}
	return true
}

func authStateIsClean(auth *Auth) bool {
	if auth == nil {
		return true
	}
	if auth.Status != StatusActive {
		return false
	}
	if auth.Unavailable || auth.StatusMessage != "" || !auth.NextRetryAfter.IsZero() || auth.LastError != nil {
		return false
	}
	if auth.Quota.Exceeded || auth.Quota.Reason != "" || !auth.Quota.NextRecoverAt.IsZero() || auth.Quota.BackoffLevel != 0 {
		return false
	}
	return true
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if len(auth.ModelStates) == 0 {
		clearAggregatedAvailability(auth)
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		hasState = true
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		clearAggregatedAvailability(auth)
		return
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func clearAggregatedAvailability(auth *Auth) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.Quota.AuthScope = false
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

func resultErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return cloneError(authErr)
	}
	resultErr := &Error{Message: err.Error()}
	if se, ok := errors.AsType[cliproxyexecutor.StatusError](err); ok && se != nil {
		resultErr.HTTPStatus = se.StatusCode()
	}
	return resultErr
}

// applyResultError populates both Error and AuthScoped on a Result from the
// underlying executor error. It preserves existing type information (status
// codes, retry hints) that would otherwise be lost when resultErrorFromError
// flattens the error, and it propagates the AuthScopedFailure marker so
// Kiro-style shared-bucket quota errors suspend the auth globally instead of
// only the triggering model.
func applyResultError(result *Result, err error) {
	if result == nil {
		return
	}
	result.Error = resultErrorFromError(err)
	if isAuthScopedFailure(err) {
		result.AuthScoped = true
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func isUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromError(err) == http.StatusUnauthorized {
		return true
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "status 401") || strings.Contains(raw, "401 unauthorized")
}

func hasUnauthorizedAuthFailure(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	return auth.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(auth.LastError.Code, "unauthorized")
}

func refreshErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		cloned := cloneError(authErr)
		if cloned.HTTPStatus == 0 {
			cloned.HTTPStatus = statusCodeFromError(err)
		}
		if cloned.HTTPStatus == 0 && isUnauthorizedError(err) {
			cloned.HTTPStatus = http.StatusUnauthorized
		}
		if cloned.HTTPStatus == http.StatusUnauthorized {
			if cloned.Code == "" {
				cloned.Code = "unauthorized"
			}
			cloned.Retryable = false
		}
		return cloned
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 && isUnauthorizedError(err) {
		statusCode = http.StatusUnauthorized
	}
	resultErr := &Error{Message: err.Error(), HTTPStatus: statusCode}
	if statusCode == http.StatusUnauthorized {
		resultErr.Code = "unauthorized"
		resultErr.Retryable = false
	}
	return resultErr
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	value := *retryAfter
	return &value
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func isAuthWideResultError(err *Error) bool {
	switch statusCodeFromResult(err) {
	case http.StatusUnauthorized:
		return true
	case http.StatusBadRequest:
		return !isModelSupportResultError(err) && !isRequestInvalidResultError(err)
	default:
		return false
	}
}

func authWideSuspendReason(err *Error, explicitAuthScoped bool) string {
	switch statusCodeFromResult(err) {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired, http.StatusForbidden:
		return "payment_required"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		if explicitAuthScoped {
			return "auth_scope_quota"
		}
		return "quota"
	default:
		return "auth_scope_failure"
	}
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isModelSupportError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Message)
}

func isRequestScopedNotFoundMessage(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return isRequestScopedNotFoundMessage(err.Message)
}

func isRequestInvalidResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if isModelSupportResultError(err) {
		return false
	}
	switch statusCodeFromResult(err) {
	case http.StatusBadRequest:
		msg := err.Error()
		return strings.Contains(msg, "invalid_request_error") ||
			strings.Contains(msg, "INVALID_ARGUMENT") ||
			strings.Contains(msg, "FAILED_PRECONDITION") ||
			isMalformedRequestErrorMessage(msg)
	case http.StatusNotFound:
		return isRequestScopedNotFoundMessage(err.Error())
	case http.StatusUnprocessableEntity:
		return true
	case http.StatusInternalServerError:
		msg := err.Error()
		return strings.Contains(msg, "\"status\":\"UNKNOWN\"") ||
			strings.Contains(msg, "\"status\": \"UNKNOWN\"")
	default:
		return false
	}
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried. Specifically, it treats 400 responses with
// "invalid_request_error", request-scoped 404 item misses caused by `store=false`,
// and all 422 responses as request-shape failures, where switching auths or
// pooled upstream models will not help. Model-support errors are excluded so
// routing can fall through to another auth or upstream.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	var resultErr *Error
	if errors.As(err, &resultErr) && resultErr != nil {
		return isRequestInvalidResultError(resultErr)
	}
	if isModelSupportError(err) {
		return false
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest:
		msg := err.Error()
		return strings.Contains(msg, "invalid_request_error") ||
			strings.Contains(msg, "INVALID_ARGUMENT") ||
			strings.Contains(msg, "FAILED_PRECONDITION") ||
			isMalformedRequestErrorMessage(msg)
	case http.StatusNotFound:
		return isRequestScopedNotFoundMessage(err.Error())
	case http.StatusUnprocessableEntity:
		return true
	case http.StatusInternalServerError:
		msg := err.Error()
		return strings.Contains(msg, "\"status\":\"UNKNOWN\"") ||
			strings.Contains(msg, "\"status\": \"UNKNOWN\"")
	default:
		return false
	}
}

func isMalformedRequestErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"improperly formed request",
		"malformed payload",
		"malformed request",
		"validationexception",
		"validation exception",
		"serializationexception",
		"deserializationexception",
		"schema validation",
		"extra inputs are not permitted",
		"unexpected field",
		"invalid json",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	if isRequestScopedNotFoundResultError(resultErr) {
		return
	}
	disableCooling := quotaCooldownDisabledForAuth(auth)
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	switch statusCode {
	case 400:
		auth.StatusMessage = "bad_request"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 401:
		auth.StatusMessage = "unauthorized"
		// disable_cooling is intended for quota/capacity retry loops. A 401 means
		// the credential itself is invalid, so keep it out of routing until a
		// refresh or re-login updates the auth state.
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 402, 403:
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 404:
		auth.StatusMessage = "not_found"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		var next time.Time
		if !disableCooling {
			if retryAfter != nil {
				next = now.Add(*retryAfter)
			} else {
				cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, disableCooling)
				if cooldown > 0 {
					next = now.Add(cooldown)
				}
				auth.Quota.BackoffLevel = nextLevel
			}
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// ListManagementSummary returns lightweight auth snapshots for management list
// views. It intentionally avoids copying full token-bearing metadata for every
// credential while preserving the fields needed for filtering, sorting, and cards.
func (m *Manager) ListManagementSummary() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.CloneForManagementSummary())
	}
	return list
}

// AnyAvailableAuthForModel reports whether any matching auth is currently usable.
func (m *Manager) AnyAvailableAuthForModel(providers []string, model string, predicate func(*Auth) bool) bool {
	if m == nil || predicate == nil {
		return false
	}

	providerSet := make(map[string]struct{}, len(providers))
	for i := 0; i < len(providers); i++ {
		providerKey := strings.TrimSpace(strings.ToLower(providers[i]))
		if providerKey == "" {
			continue
		}
		providerSet[providerKey] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	registryRef := registry.GetGlobalRegistry()
	now := time.Now()

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		if !AuthAvailableForModel(auth, model, now) {
			continue
		}
		if predicate(auth) {
			return true
		}
	}

	return false
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// GetExecutionSessionAuthByID retrieves a Home runtime auth scoped to an execution session.
func (m *Manager) GetExecutionSessionAuthByID(sessionID string, authID string) (*Auth, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	if auth == nil {
		return nil, false
	}
	return auth.Clone(), true
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	m.mu.RUnlock()

	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.Lock()
	if sessionID == CloseAllExecutionSessionsID {
		m.clearHomeRuntimeAuthsLocked()
	} else {
		m.clearHomeRuntimeAuthsForSessionLocked(sessionID)
	}
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.Unlock()

	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

// ResetExecutionSession asks all registered executors to invalidate the
// supplied execution session so the next request starts fresh.
func (m *Manager) ResetExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.RLock()
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.RUnlock()

	for i := range executors {
		if resetter, ok := executors[i].(ExecutionSessionResetter); ok && resetter != nil {
			resetter.ResetExecutionSession(sessionID)
		}
	}
}

func (m *Manager) useSchedulerFastPath() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	return isBuiltInSelector(m.selector)
}

func (m *Manager) useSchedulerFastPathForProvider(provider string) bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(provider), "kiro") {
		return false
	}
	return isBuiltInSelector(m.selector) || schedulerBackedSessionAffinitySelector(m.selector) != nil
}

func useSchedulerFastPathForProviders(m *Manager, providers []string) bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	if len(providers) == 1 && strings.EqualFold(strings.TrimSpace(providers[0]), "kiro") {
		return false
	}
	return isBuiltInSelector(m.selector) || schedulerBackedSessionAffinitySelector(m.selector) != nil
}

func schedulerBackedSessionAffinitySelector(selector Selector) *SessionAffinitySelector {
	affinity, ok := selector.(*SessionAffinitySelector)
	if !ok || affinity == nil || affinity.cache == nil || !isBuiltInSelector(affinity.fallback) {
		return nil
	}
	return affinity
}

func shouldRetrySchedulerPick(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable"
}

func (m *Manager) eligibleMixedProviders(providers []string) []string {
	if m == nil || len(providers) == 0 {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	normalized := true
	for idx, provider := range providers {
		providerKey := strings.ToLower(strings.TrimSpace(provider))
		if providerKey == "" || providerKey != provider {
			normalized = false
			break
		}
		if _, ok := m.executors[providerKey]; !ok {
			normalized = false
			break
		}
		for prev := 0; prev < idx; prev++ {
			if providers[prev] == providerKey {
				normalized = false
				break
			}
		}
		if !normalized {
			break
		}
	}
	if normalized {
		return providers
	}

	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		providerKey := strings.ToLower(strings.TrimSpace(provider))
		if providerKey == "" {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		duplicate := false
		for _, existing := range out {
			if existing == providerKey {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		out = append(out, providerKey)
	}
	return out
}

func (m *Manager) routeAwareSelectionRequired(auth *Auth, routeModel string) bool {
	routeModel = strings.TrimSpace(routeModel)
	if auth == nil || routeModel == "" {
		return false
	}
	return m.routeAwareSelectionRequiredWithKey(auth, routeModel, canonicalModelKey(routeModel))
}

type singleRouteAwareCacheKey struct {
	routeKey string
	provider string
}

type mixedRouteAwareCacheKey struct {
	routeKey      string
	providerCount uint8
	providers     [4]string
}

func (m *Manager) clearRouteAwareCaches() {
	if m == nil {
		return
	}
	m.routeAwareSingleCache.Clear()
	m.routeAwareMixedCache.Clear()
}

func makeSingleRouteAwareCacheKey(routeKey, provider string) (singleRouteAwareCacheKey, bool) {
	provider = strings.TrimSpace(provider)
	if routeKey == "" || provider == "" {
		return singleRouteAwareCacheKey{}, false
	}
	return singleRouteAwareCacheKey{routeKey: routeKey, provider: provider}, true
}

func makeMixedRouteAwareCacheKey(routeKey string, providers []string) (mixedRouteAwareCacheKey, bool) {
	if routeKey == "" || len(providers) == 0 || len(providers) > len((mixedRouteAwareCacheKey{}).providers) {
		return mixedRouteAwareCacheKey{}, false
	}
	key := mixedRouteAwareCacheKey{
		routeKey:      routeKey,
		providerCount: uint8(len(providers)),
	}
	copy(key.providers[:], providers)
	return key, true
}

func (m *Manager) routeAwareSelectionRequiredWithKey(auth *Auth, routeModel, routeKey string) bool {
	if auth == nil || routeModel == "" || routeKey == "" {
		return false
	}
	prefix := strings.TrimSpace(auth.Prefix)
	prefixRewrite := prefix != "" && strings.HasPrefix(routeModel, prefix+"/")
	channel := m.oauthModelAliasChannelForAuth(auth)
	if !prefixRewrite && channel == "" {
		return false
	}
	if !prefixRewrite && !m.oauthModelAliasConfigured(channel) {
		return false
	}
	return m.selectionModelKeyForAuth(auth, routeModel) != routeKey
}

func (m *Manager) oauthModelAliasConfigured(channel string) bool {
	if m == nil || channel == "" {
		return false
	}
	raw := m.oauthModelAlias.Load()
	table, _ := raw.(*oauthModelAliasTable)
	return table != nil && table.reverse != nil && table.reverse[channel] != nil
}

func (m *Manager) oauthModelAliasChannelForAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "gemini":
		return ""
	case "vertex", "claude", "codex":
		if auth.Attributes != nil {
			authKind := strings.ToLower(strings.TrimSpace(auth.Attributes["auth_kind"]))
			if authKind == "apikey" {
				return ""
			}
			if authKind != "" {
				return provider
			}
			if apiKey := strings.TrimSpace(auth.Attributes["api_key"]); apiKey != "" {
				return ""
			}
		}
		return provider
	case "gemini-cli", "aistudio", "antigravity", "kimi", "kiro":
		return provider
	default:
		return ""
	}
}

func (m *Manager) mixedLegacySelectionRequired(providers []string, routeModel string, tried map[string]struct{}) bool {
	if m == nil || routeModel == "" || len(providers) == 0 {
		return false
	}
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return false
	}
	cacheKey, cacheable := makeMixedRouteAwareCacheKey(routeKey, providers)
	if cacheable {
		if _, ok := m.routeAwareMixedCache.Load(cacheKey); ok {
			return false
		}
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, providerKey := range providers {
		providerSet[providerKey] = struct{}{}
	}
	required := false
	m.mu.RLock()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if _, ok := providerSet[strings.TrimSpace(strings.ToLower(candidate.Provider))]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if m.routeAwareSelectionRequiredWithKey(candidate, routeModel, routeKey) {
			required = true
			break
		}
	}
	m.mu.RUnlock()
	if !required && cacheable {
		m.routeAwareMixedCache.Store(cacheKey, struct{}{})
	}
	return required
}

func (m *Manager) singleLegacySelectionRequired(provider, routeModel string, tried map[string]struct{}) bool {
	if m == nil || routeModel == "" {
		return false
	}
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return false
	}
	cacheKey, cacheable := makeSingleRouteAwareCacheKey(routeKey, provider)
	if cacheable {
		if _, ok := m.routeAwareSingleCache.Load(cacheKey); ok {
			return false
		}
	}
	required := false
	provider = strings.TrimSpace(provider)
	m.mu.RLock()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled || candidate.Provider != provider {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if m.routeAwareSelectionRequiredWithKey(candidate, routeModel, routeKey) {
			required = true
			break
		}
	}
	m.mu.RUnlock()
	if !required && cacheable {
		m.routeAwareSingleCache.Store(cacheKey, struct{}{})
	}
	return required
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	opts = pinSelectedKiroAuthForProvider(provider, opts)
	selector := m.selectorForProvider(provider)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate.Provider != provider || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, errAvailable := m.availableAuthsForRouteModel(candidates, provider, model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		return nil, nil, errAvailable
	}
	selected, errPick := selector.Pick(ctx, provider, selectionArgForSelector(selector, model), opts, available)
	if errPick != nil {
		m.mu.RUnlock()
		return nil, nil, errPick
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	opts = pinSelectedKiroAuthForProvider(provider, opts)
	if m.HomeEnabled() {
		auth, executor, _, err := m.pickNextViaHome(ctx, model, opts, tried)
		return auth, executor, err
	}
	if !m.useSchedulerFastPathForProvider(provider) {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	model = strings.TrimSpace(model)
	if model != "" && m.singleLegacySelectionRequired(provider, model, tried) {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	if affinity := schedulerBackedSessionAffinitySelector(m.selectorForProvider(provider)); affinity != nil {
		return m.pickNextSingleWithSchedulerAffinity(ctx, affinity, provider, model, opts, tried)
	}
	return m.pickNextSingleWithScheduler(ctx, provider, model, opts, tried)
}

func (m *Manager) pickNextSingleWithSchedulerAffinity(ctx context.Context, affinity *SessionAffinitySelector, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		return m.pickNextSingleWithScheduler(ctx, provider, model, opts, tried)
	}

	cacheKey := sessionAffinityCacheKey(provider, primaryID, opts.Metadata)
	forceFreshUpstream := affinity.cache.ForceNewPending(cacheKey)
	if cachedAuthID, ok := affinity.cache.GetAndRefresh(cacheKey); ok {
		if auth, executor, okCached, errPick := m.pickCachedSingleWithScheduler(ctx, provider, model, opts, tried, cachedAuthID); errPick != nil || okCached {
			return auth, executor, errPick
		}
		affinity.cache.Invalidate(cacheKey)
		forceNewUpstreamSessionForNextCredential(&opts)
	}

	fallbackKey := ""
	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey = sessionAffinityCacheKey(provider, fallbackID, opts.Metadata)
		if affinity.cache.ForceNewPending(fallbackKey) {
			forceFreshUpstream = true
		}
		if cachedAuthID, ok := affinity.cache.Get(fallbackKey); ok {
			auth, executor, okCached, errPick := m.pickCachedSingleWithScheduler(ctx, provider, model, opts, tried, cachedAuthID)
			if errPick != nil {
				return auth, executor, errPick
			}
			if okCached {
				if forceFreshUpstream {
					forceNewUpstreamSessionForNextCredential(&opts)
					consumeForceNewMarkers(affinity.cache, cacheKey, fallbackKey)
				}
				affinity.cache.Set(cacheKey, auth.ID)
				return auth, executor, nil
			}
			affinity.cache.Invalidate(fallbackKey)
			forceNewUpstreamSessionForNextCredential(&opts)
		}
	}

	if forceFreshUpstream {
		forceNewUpstreamSessionForNextCredential(&opts)
	}

	auth, executor, errPick := m.pickNextSingleStableWithScheduler(ctx, provider, model, opts, tried, cacheKey)
	if errPick != nil {
		return nil, nil, errPick
	}
	if auth != nil {
		if forceFreshUpstream {
			consumeForceNewMarkers(affinity.cache, cacheKey, fallbackKey)
		}
		affinity.cache.Set(cacheKey, auth.ID)
	}
	return auth, executor, nil
}

func (m *Manager) pickCachedSingleWithScheduler(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, authID string) (*Auth, ProviderExecutor, bool, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil, false, nil
	}
	pinnedOpts := withPinnedAuthMetadata(opts, authID)
	auth, executor, errPick := m.pickNextSingleWithScheduler(ctx, provider, model, pinnedOpts, tried)
	if errPick == nil {
		return auth, executor, auth != nil, nil
	}
	if isRecoverableAffinityPickError(errPick) {
		logRecoverableAffinityPick(ctx, "single", provider, model, authID, opts, errPick)
		return nil, nil, false, nil
	}
	return nil, nil, false, errPick
}

func (m *Manager) pickNextSingleStableWithScheduler(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, affinityKey string) (*Auth, ProviderExecutor, error) {
	executor, okExecutor := m.Executor(provider)
	if !okExecutor {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, errPick := m.scheduler.pickSingleStable(ctx, provider, model, opts, tried, affinityKey)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, errPick = m.scheduler.pickSingleStable(ctx, provider, model, opts, tried, affinityKey)
		}
		if errPick != nil {
			return nil, nil, errPick
		}
		if selected == nil {
			return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		authCopy, staleSelection := m.clonePickedAuthForExecution(provider, selected)
		if staleSelection {
			m.syncScheduler()
			continue
		}
		if authCopy == nil {
			return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		return authCopy, executor, nil
	}
}

func (m *Manager) pickNextSingleWithScheduler(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	executor, okExecutor := m.Executor(provider)
	if !okExecutor {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, errPick := m.scheduler.pickSingle(ctx, provider, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, errPick = m.scheduler.pickSingle(ctx, provider, model, opts, tried)
		}
		if errPick != nil {
			return nil, nil, errPick
		}
		if selected == nil {
			return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		authCopy, staleSelection := m.clonePickedAuthForExecution(provider, selected)
		if staleSelection {
			m.syncScheduler()
			continue
		}
		if authCopy == nil {
			return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		return authCopy, executor, nil
	}
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	opts = pinSelectedKiroAuthForProviders(providers, opts)
	opts = withProviderScopeMetadata(opts, normalizeProviderKeys(providers))
	selector := m.selector
	if len(providers) == 1 && strings.EqualFold(strings.TrimSpace(providers[0]), "kiro") {
		selector = m.selectorForProvider("kiro")
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, errAvailable := m.availableAuthsForRouteModel(candidates, "mixed", model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		return nil, nil, "", errAvailable
	}
	selected, errPick := selector.Pick(ctx, "mixed", selectionArgForSelector(selector, model), opts, available)
	if errPick != nil {
		m.mu.RUnlock()
		return nil, nil, "", errPick
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	executor, okExecutor := m.executors[providerKey]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m.HomeEnabled() {
		return m.pickNextViaHome(ctx, model, opts, tried)
	}
	if !useSchedulerFastPathForProviders(m, providers) {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}

	eligibleProviders := m.eligibleMixedProviders(providers)
	if len(eligibleProviders) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	opts = pinSelectedKiroAuthForProviders(eligibleProviders, opts)
	opts = withProviderScopeMetadata(opts, eligibleProviders)
	model = strings.TrimSpace(model)
	if model != "" && m.mixedLegacySelectionRequired(eligibleProviders, model, tried) {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}
	if affinity := schedulerBackedSessionAffinitySelector(m.selector); affinity != nil {
		return m.pickNextMixedWithSchedulerAffinity(ctx, affinity, eligibleProviders, model, opts, tried)
	}
	return m.pickNextMixedWithScheduler(ctx, eligibleProviders, model, opts, tried)
}

func (m *Manager) pickNextMixedWithSchedulerAffinity(ctx context.Context, affinity *SessionAffinitySelector, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		return m.pickNextMixedWithScheduler(ctx, providers, model, opts, tried)
	}

	cacheKey := sessionAffinityCacheKey("mixed", primaryID, opts.Metadata)
	forceFreshUpstream := affinity.cache.ForceNewPending(cacheKey)
	if cachedAuthID, ok := affinity.cache.GetAndRefresh(cacheKey); ok {
		if auth, executor, provider, okCached, errPick := m.pickCachedMixedWithScheduler(ctx, providers, model, opts, tried, cachedAuthID); errPick != nil || okCached {
			return auth, executor, provider, errPick
		}
		affinity.cache.Invalidate(cacheKey)
		forceNewUpstreamSessionForNextCredential(&opts)
	}

	fallbackKey := ""
	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey = sessionAffinityCacheKey("mixed", fallbackID, opts.Metadata)
		if affinity.cache.ForceNewPending(fallbackKey) {
			forceFreshUpstream = true
		}
		if cachedAuthID, ok := affinity.cache.Get(fallbackKey); ok {
			auth, executor, provider, okCached, errPick := m.pickCachedMixedWithScheduler(ctx, providers, model, opts, tried, cachedAuthID)
			if errPick != nil {
				return auth, executor, provider, errPick
			}
			if okCached {
				if forceFreshUpstream {
					forceNewUpstreamSessionForNextCredential(&opts)
					consumeForceNewMarkers(affinity.cache, cacheKey, fallbackKey)
				}
				affinity.cache.Set(cacheKey, auth.ID)
				return auth, executor, provider, nil
			}
			affinity.cache.Invalidate(fallbackKey)
			forceNewUpstreamSessionForNextCredential(&opts)
		}
	}

	if forceFreshUpstream {
		forceNewUpstreamSessionForNextCredential(&opts)
	}

	auth, executor, provider, errPick := m.pickNextMixedStableWithScheduler(ctx, providers, model, opts, tried, cacheKey)
	if errPick != nil {
		return nil, nil, "", errPick
	}
	if auth != nil {
		if forceFreshUpstream {
			consumeForceNewMarkers(affinity.cache, cacheKey, fallbackKey)
		}
		affinity.cache.Set(cacheKey, auth.ID)
	}
	return auth, executor, provider, nil
}

func (m *Manager) pickCachedMixedWithScheduler(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, authID string) (*Auth, ProviderExecutor, string, bool, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil, "", false, nil
	}
	pinnedOpts := withPinnedAuthMetadata(opts, authID)
	auth, executor, provider, errPick := m.pickNextMixedWithScheduler(ctx, providers, model, pinnedOpts, tried)
	if errPick == nil {
		return auth, executor, provider, auth != nil, nil
	}
	if isRecoverableAffinityPickError(errPick) {
		logRecoverableAffinityPick(ctx, "mixed", strings.Join(providers, ","), model, authID, opts, errPick)
		return nil, nil, "", false, nil
	}
	return nil, nil, "", false, errPick
}

func (m *Manager) pickNextMixedStableWithScheduler(ctx context.Context, eligibleProviders []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, affinityKey string) (*Auth, ProviderExecutor, string, error) {
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, providerKey, errPick := m.scheduler.pickMixedStable(ctx, eligibleProviders, model, opts, tried, affinityKey)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, providerKey, errPick = m.scheduler.pickMixedStable(ctx, eligibleProviders, model, opts, tried, affinityKey)
		}
		if errPick != nil {
			return nil, nil, "", errPick
		}
		if selected == nil {
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		executor, okExecutor := m.Executor(providerKey)
		if !okExecutor {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		authCopy, staleSelection := m.clonePickedAuthForExecution(providerKey, selected)
		if staleSelection {
			m.syncScheduler()
			continue
		}
		if authCopy == nil {
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		return authCopy, executor, providerKey, nil
	}
}

func (m *Manager) pickNextMixedWithScheduler(ctx context.Context, eligibleProviders []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {

	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, providerKey, errPick := m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, providerKey, errPick = m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
		}
		if errPick != nil {
			return nil, nil, "", errPick
		}
		if selected == nil {
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		executor, okExecutor := m.Executor(providerKey)
		if !okExecutor {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		authCopy, staleSelection := m.clonePickedAuthForExecution(providerKey, selected)
		if staleSelection {
			m.syncScheduler()
			continue
		}
		if authCopy == nil {
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		return authCopy, executor, providerKey, nil
	}
}

type homeErrorEnvelope struct {
	Error *homeErrorDetail `json:"error"`
}

type homeErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

const (
	homeUpstreamModelAttributeKey     = "home_upstream_model"
	homeRequestRetryExceededErrorCode = "request_retry_exceeded"
)

func isHomeRequestRetryExceededError(err error) bool {
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(authErr.Code), homeRequestRetryExceededErrorCode)
}

func shouldReturnLastErrorOnPickFailure(homeMode bool, lastErr error, errPick error) bool {
	if lastErr == nil {
		return false
	}
	if !homeMode {
		return true
	}
	return isHomeRequestRetryExceededError(errPick)
}

func homeAuthAlreadyTried(tried map[string]struct{}, authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(tried) == 0 {
		return false
	}
	_, ok := tried[authID]
	return ok
}

func repeatedHomeAuthError() *Error {
	return &Error{
		Code:       homeRequestRetryExceededErrorCode,
		Message:    "home returned a previously tried auth",
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

type homeAuthDispatchResponse struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	AuthIndex  string `json:"auth_index"`
	UserAPIKey string `json:"user_api_key"`
	Auth       Auth   `json:"auth"`
}

type homeAuthDispatcher interface {
	HeartbeatOK() bool
	RPopAuth(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int) ([]byte, error)
}

var currentHomeDispatcher = func() homeAuthDispatcher {
	return home.Current()
}

func setHomeUserAPIKeyOnGinContext(ctx context.Context, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || ctx == nil {
		return
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Set(string, any) })
	if !ok || ginCtx == nil {
		return
	}
	ginCtx.Set("userApiKey", apiKey)
}

func homeDispatchHeaders(ctx context.Context, headers http.Header) http.Header {
	apiKey, ok := homeQueryCredentialFromContext(ctx)
	if !ok {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = http.Header{}
	}
	if out.Get("Authorization") != "" || out.Get("X-Goog-Api-Key") != "" || out.Get("X-Api-Key") != "" {
		return out
	}
	out.Set("X-Goog-Api-Key", apiKey)
	return out
}

func homeQueryCredentialFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if queryCtx, ok := ctx.Value("gin").(interface{ Query(string) string }); ok && queryCtx != nil {
		if apiKey := strings.TrimSpace(queryCtx.Query("key")); apiKey != "" {
			return apiKey, true
		}
		if apiKey := strings.TrimSpace(queryCtx.Query("auth_token")); apiKey != "" {
			return apiKey, true
		}
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return "", false
	}
	rawMetadata, ok := ginCtx.Get("accessMetadata")
	if !ok {
		return "", false
	}
	source := accessMetadataSource(rawMetadata)
	if source != "query-key" && source != "query-auth-token" {
		return "", false
	}
	rawAPIKey, ok := ginCtx.Get("userApiKey")
	if !ok {
		return "", false
	}
	apiKey := contextStringValue(rawAPIKey)
	if apiKey == "" {
		return "", false
	}
	return apiKey, true
}

func accessMetadataSource(raw any) string {
	switch v := raw.(type) {
	case map[string]string:
		return strings.TrimSpace(v["source"])
	case map[string]any:
		return contextStringValue(v["source"])
	default:
		return ""
	}
}

func contextStringValue(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func homeExecutionSessionIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func (m *Manager) clearHomeRuntimeAuths() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.clearHomeRuntimeAuthsLocked()
	m.mu.Unlock()
}

func (m *Manager) clearHomeRuntimeAuthsLocked() {
	if m == nil {
		return
	}
	m.homeRuntimeAuths = make(map[string]map[string]*Auth)
}

func (m *Manager) clearHomeRuntimeAuthsForSessionLocked(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}
	delete(m.homeRuntimeAuths, sessionID)
}

func (m *Manager) rememberHomeRuntimeAuth(sessionID string, auth *Auth) {
	sessionID = strings.TrimSpace(sessionID)
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	if m == nil || auth == nil || sessionID == "" || authID == "" || !authWebsocketsEnabled(auth) {
		return
	}
	m.mu.Lock()
	if m.homeRuntimeAuths == nil {
		m.homeRuntimeAuths = make(map[string]map[string]*Auth)
	}
	sessionAuths := m.homeRuntimeAuths[sessionID]
	if sessionAuths == nil {
		sessionAuths = make(map[string]*Auth)
		m.homeRuntimeAuths[sessionID] = sessionAuths
	}
	sessionAuths[authID] = auth.Clone()
	m.mu.Unlock()
}

func (m *Manager) homeRuntimeAuthByID(sessionID string, authID string) (*Auth, ProviderExecutor, string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, nil, "", false
	}
	m.mu.RLock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	m.mu.RUnlock()
	if auth == nil || !authWebsocketsEnabled(auth) {
		return nil, nil, "", false
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" {
		return nil, nil, "", false
	}
	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", false
	}
	return auth.Clone(), executor, providerKey, true
}

func (m *Manager) pickNextViaHome(ctx context.Context, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	executionSessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	count := homeAuthCountFromMetadata(opts.Metadata)
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && count <= 1 {
		if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" {
			_, alreadyTried := tried[pinnedAuthID]
			if !alreadyTried {
				if auth, executor, providerKey, ok := m.homeRuntimeAuthByID(executionSessionID, pinnedAuthID); ok {
					return auth, executor, providerKey, nil
				}
			}
		}
	}

	client := currentHomeDispatcher()
	if client == nil || !client.HeartbeatOK() {
		return nil, nil, "", &Error{Code: "home_unavailable", Message: "home control center unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}

	requestedModel := requestedModelFromMetadata(opts.Metadata, model)
	sessionID := ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	dispatchHeaders := homeDispatchHeaders(ctx, opts.Headers)

	raw, err := client.RPopAuth(ctx, requestedModel, sessionID, dispatchHeaders, count)
	if err != nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: err.Error(), HTTPStatus: http.StatusServiceUnavailable}
	}

	var env homeErrorEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal == nil && env.Error != nil {
		code := strings.TrimSpace(env.Error.Type)
		if code == "" {
			code = strings.TrimSpace(env.Error.Code)
		}
		msg := strings.TrimSpace(env.Error.Message)
		if msg == "" {
			msg = "home returned error"
		}
		status := http.StatusBadGateway
		switch strings.ToLower(code) {
		case "model_not_found":
			status = http.StatusNotFound
		case "authentication_error", "unauthorized":
			status = http.StatusUnauthorized
		}
		return nil, nil, "", &Error{Code: code, Message: msg, HTTPStatus: status}
	}

	var dispatch homeAuthDispatchResponse
	if errUnmarshal := json.Unmarshal(raw, &dispatch); errUnmarshal != nil {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
	}
	setHomeUserAPIKeyOnGinContext(ctx, dispatch.UserAPIKey)
	auth := dispatch.Auth
	if strings.TrimSpace(auth.ID) == "" {
		// Backward compatibility: older home instances returned the auth directly.
		if errUnmarshal := json.Unmarshal(raw, &auth); errUnmarshal != nil {
			return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
		}
	}
	if upstreamModel := strings.TrimSpace(dispatch.Model); upstreamModel != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 1)
		}
		auth.Attributes[homeUpstreamModelAttributeKey] = upstreamModel
	}
	if strings.TrimSpace(auth.ID) == "" {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned auth without id", HTTPStatus: http.StatusBadGateway}
	}
	if homeAuthAlreadyTried(tried, auth.ID) {
		return nil, nil, "", repeatedHomeAuthError()
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned auth without provider", HTTPStatus: http.StatusBadGateway}
	}

	homeAuthIndex := strings.TrimSpace(dispatch.AuthIndex)
	if homeAuthIndex != "" {
		auth.Index = homeAuthIndex
		auth.indexAssigned = true
	} else {
		auth.EnsureIndex()
	}

	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered", HTTPStatus: http.StatusBadGateway}
	}

	authCopy := auth.Clone()
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && authWebsocketsEnabled(authCopy) {
		m.rememberHomeRuntimeAuth(executionSessionID, authCopy)
	}
	return authCopy, executor, providerKey, nil
}

func requestedModelFromMetadata(metadata map[string]any, fallback string) string {
	if metadata != nil {
		if v, ok := metadata[cliproxyexecutor.RequestedModelMetadataKey]; ok {
			switch typed := v.(type) {
			case string:
				if trimmed := strings.TrimSpace(typed); trimmed != "" {
					return trimmed
				}
			case []byte:
				if trimmed := strings.TrimSpace(string(typed)); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return "unknown"
	}
	return fallback
}

func (m *Manager) findAllAntigravityCreditsCandidateAuths(routeModel string, opts cliproxyexecutor.Options) []creditsCandidateEntry {
	if m == nil {
		return nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var known []creditsCandidateEntry
	var unknown []creditsCandidateEntry
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if pinnedAuthID != "" && auth.ID != pinnedAuthID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			continue
		}
		if !strings.Contains(strings.ToLower(strings.TrimSpace(routeModel)), "claude") {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		executor, ok := m.executors[providerKey]
		if !ok {
			continue
		}

		hint, okHint := GetAntigravityCreditsHint(auth.ID)
		if okHint && hint.Known {
			if !hint.Available {
				continue
			}
			known = append(known, creditsCandidateEntry{
				auth:     auth.Clone(),
				executor: executor,
				provider: providerKey,
			})
			continue
		}
		unknown = append(unknown, creditsCandidateEntry{
			auth:     auth.Clone(),
			executor: executor,
			provider: providerKey,
		})
	}
	sort.Slice(known, func(i, j int) bool {
		return known[i].auth.ID < known[j].auth.ID
	})
	sort.Slice(unknown, func(i, j int) bool {
		return unknown[i].auth.ID < unknown[j].auth.ID
	})
	return append(known, unknown...)
}

type creditsCandidateEntry struct {
	auth     *Auth
	executor ProviderExecutor
	provider string
}

func hasAntigravityProvider(providers []string) bool {
	for _, p := range providers {
		if strings.EqualFold(strings.TrimSpace(p), "antigravity") {
			return true
		}
	}
	return false
}

func shouldAttemptAntigravityCreditsFallback(m *Manager, lastErr error, providers []string) bool {
	status := statusCodeFromError(lastErr)
	log.WithFields(log.Fields{
		"lastErr":   errorString(lastErr),
		"status":    status,
		"providers": providers,
	}).Debug("shouldAttemptAntigravityCreditsFallback")
	if m == nil || lastErr == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.QuotaExceeded.AntigravityCredits {
		return false
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case 0:
		var authErr *Error
		if errors.As(lastErr, &authErr) && authErr != nil {
			return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable" || authErr.Code == "model_cooldown"
		}
		var cooldownErr *modelCooldownError
		if errors.As(lastErr, &cooldownErr) {
			return true
		}
		return false
	default:
		return false
	}
}

func (m *Manager) tryAntigravityCreditsExecute(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, bool) {
	routeModel := req.Model
	candidates := m.findAllAntigravityCreditsCandidateAuths(routeModel, opts)
	for _, c := range candidates {
		if ctx.Err() != nil {
			return cliproxyexecutor.Response{}, false
		}
		creditsCtx := WithAntigravityCredits(ctx)
		if rt := m.roundTripperFor(c.auth); rt != nil {
			creditsCtx = context.WithValue(creditsCtx, roundTripperContextKey{}, rt)
			creditsCtx = context.WithValue(creditsCtx, "cliproxy.roundtripper", rt)
		}
		creditsOpts := ensureRequestedModelMetadata(opts, routeModel)
		creditsCtx = contextWithRequestedModelAlias(creditsCtx, creditsOpts, routeModel)
		preparedAuth, errPrepare := m.prepareRequestAuth(creditsCtx, c.executor, c.auth)
		if errPrepare != nil {
			continue
		}
		c.auth = preparedAuth
		publishSelectedAuthMetadata(creditsOpts.Metadata, c.auth.ID)
		models := m.executionModelCandidates(c.auth, routeModel)
		if len(models) == 0 {
			continue
		}
		for _, upstreamModel := range models {
			resultModel := m.stateModelForExecution(c.auth, routeModel, upstreamModel, len(models) > 1)
			execReq := req
			execReq.Model = upstreamModel
			resp, errExec := c.executor.Execute(creditsCtx, c.auth, execReq, creditsOpts)
			result := Result{AuthID: c.auth.ID, Provider: c.provider, Model: resultModel, Success: errExec == nil}
			if errExec != nil {
				applyResultError(&result, errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.MarkResult(creditsCtx, result)
				continue
			}
			m.MarkResult(creditsCtx, result)
			return resp, true
		}
	}
	return cliproxyexecutor.Response{}, false
}

func (m *Manager) tryAntigravityCreditsExecuteStream(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, bool) {
	routeModel := req.Model
	candidates := m.findAllAntigravityCreditsCandidateAuths(routeModel, opts)
	for _, c := range candidates {
		if ctx.Err() != nil {
			return nil, false
		}
		creditsCtx := WithAntigravityCredits(ctx)
		if rt := m.roundTripperFor(c.auth); rt != nil {
			creditsCtx = context.WithValue(creditsCtx, roundTripperContextKey{}, rt)
			creditsCtx = context.WithValue(creditsCtx, "cliproxy.roundtripper", rt)
		}
		creditsOpts := ensureRequestedModelMetadata(opts, routeModel)
		preparedAuth, errPrepare := m.prepareRequestAuth(creditsCtx, c.executor, c.auth)
		if errPrepare != nil {
			continue
		}
		c.auth = preparedAuth
		publishSelectedAuthMetadata(creditsOpts.Metadata, c.auth.ID)
		models := m.executionModelCandidates(c.auth, routeModel)
		if len(models) == 0 {
			continue
		}
		result, errStream := m.executeStreamWithModelPool(creditsCtx, c.executor, c.auth, c.provider, req, creditsOpts, routeModel, models, len(models) > 1)
		if errStream != nil {
			continue
		}
		return result, true
	}
	return nil, false
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	persistAuth := auth.Clone()
	stripProxyPoolLeaseForPersist(persistAuth)
	persistAuth.SetRuntimeStateMetadata()
	_, err := m.store.Save(ctx, persistAuth)
	return err
}

func (m *Manager) handleExecutionRefreshUpdate(ctx context.Context, updated *Auth) {
	if m == nil || updated == nil || updated.ID == "" {
		return
	}
	updateCtx := context.Background()
	if ctx != nil {
		updateCtx = context.WithoutCancel(ctx)
	}
	if _, err := m.Update(withRefreshUpdate(updateCtx), updated); err != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", updated.ID).Warnf("failed to persist refreshed auth: %v", err)
	}
}

func (m *Manager) handleExecutionAuthUpdate(ctx context.Context, updated *Auth) {
	if m == nil || updated == nil || updated.ID == "" {
		return
	}
	updateCtx := context.Background()
	if ctx != nil {
		updateCtx = context.WithoutCancel(ctx)
	}
	if _, err := m.Update(updateCtx, updated); err != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", updated.ID).Warnf("failed to persist auth update: %v", err)
	}
}

func (m *Manager) persistRuntimeState(ctx context.Context, auth *Auth) error {
	if m == nil || m.runtimeStateStore == nil || auth == nil || auth.ID == "" {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	state := auth.RuntimeStateSnapshot()
	if err := m.runtimeStateStore.Save(ctx, auth.ID, state); err != nil {
		return err
	}
	m.mu.Lock()
	if m.runtimeStates != nil {
		m.runtimeStates[auth.ID] = state
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) applyPersistedRuntimeStateLocked(auth *Auth) {
	if m == nil || auth == nil || auth.ID == "" || len(m.runtimeStates) == 0 {
		return
	}
	state, ok := m.runtimeStates[auth.ID]
	if !ok {
		return
	}
	auth.ApplyRuntimeState(state)
}

func preserveRuntimeState(existing, auth *Auth) {
	if existing == nil || auth == nil {
		return
	}
	mu := existing.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()
	auth.Success = existing.Success
	auth.Failed = existing.Failed
	auth.recentRequests = cloneRecentRequestRing(existing.recentRequests)
}

var refreshPreservedMetadataKeys = []string{
	"prefix",
	"proxy_url",
	"proxy-url",
	"proxyUrl",
	"priority",
	"note",
	"headers",
	"excluded_models",
	"excluded-models",
	"excludedModels",
	"disable_cooling",
	"disable-cooling",
	"disableCooling",
	"user_agent",
	"user-agent",
	"userAgent",
	"websockets",
	"websocket",
	"service_tier_passthrough",
	"service-tier-passthrough",
	"serviceTierPassthrough",
	"fast",
	"websocket_handshake_debug",
}

var refreshPreservedAttributeKeys = []string{
	"path",
	"source",
	"priority",
	"note",
	"proxy_url",
	"excluded_models",
	"excluded_models_hash",
	"websockets",
	"service_tier_passthrough",
	"user_agent",
	"user-agent",
	"userAgent",
	proxyPoolAssignedAttribute,
}

func preserveEditableAuthFileOptions(existing, auth *Auth) {
	if existing == nil || auth == nil {
		return
	}
	auth.Prefix = existing.Prefix
	auth.ProxyURL = existing.ProxyURL

	if auth.Metadata == nil && len(existing.Metadata) > 0 {
		auth.Metadata = make(map[string]any, len(existing.Metadata))
	}
	for _, key := range refreshPreservedMetadataKeys {
		if existing.Metadata != nil {
			if value, ok := existing.Metadata[key]; ok {
				auth.Metadata[key] = value
				continue
			}
		}
		if auth.Metadata != nil {
			delete(auth.Metadata, key)
		}
	}

	if auth.Attributes == nil && len(existing.Attributes) > 0 {
		auth.Attributes = make(map[string]string, len(existing.Attributes))
	}
	for _, key := range refreshPreservedAttributeKeys {
		if existing.Attributes != nil {
			if value, ok := existing.Attributes[key]; ok {
				auth.Attributes[key] = value
				continue
			}
		}
		if auth.Attributes != nil {
			delete(auth.Attributes, key)
		}
	}
	for key := range auth.Attributes {
		if strings.HasPrefix(key, "header:") {
			delete(auth.Attributes, key)
		}
	}
	if len(existing.Attributes) > 0 {
		for key, value := range existing.Attributes {
			if strings.HasPrefix(key, "header:") && strings.TrimSpace(value) != "" {
				auth.Attributes[key] = value
			}
		}
	}
	ApplyAuthFileOptionsFromMetadata(auth)
	ApplyCodexMetadataFromMetadata(auth)
	ApplyCustomHeadersFromMetadata(auth)
}

func (m *Manager) enqueuePersistAuthID(ctx context.Context, authID string) {
	if m == nil || shouldSkipPersist(ctx) {
		return
	}
	if !m.persistEnabled.Load() {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}

	m.persistMu.Lock()
	if m.persistIDs == nil {
		m.persistIDs = make(map[string]struct{}, 32)
	}
	m.persistIDs[authID] = struct{}{}
	if m.persistWake == nil || m.persistCancel == nil {
		m.startPersistLoopLocked()
	}
	wake := m.persistWake
	m.persistMu.Unlock()

	select {
	case wake <- struct{}{}:
	default:
	}
}

func (m *Manager) discardPendingPersistAuthID(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	m.persistMu.Lock()
	if m.persistIDs != nil {
		delete(m.persistIDs, authID)
	}
	m.persistMu.Unlock()
}

func (m *Manager) startPersistLoopLocked() {
	if m == nil {
		return
	}
	if m.persistWake != nil && m.persistCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	m.persistWake = wake
	m.persistCancel = cancel
	m.persistWG.Add(1)
	go m.runPersistLoop(ctx, wake)
}

func (m *Manager) stopPersistLoop() {
	if m == nil {
		return
	}

	m.persistMu.Lock()
	cancel := m.persistCancel
	m.persistCancel = nil
	m.persistWake = nil
	m.persistMu.Unlock()

	if cancel != nil {
		cancel()
		m.persistWG.Wait()
	}
}

func (m *Manager) runPersistLoop(ctx context.Context, wake <-chan struct{}) {
	defer m.persistWG.Done()

	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)

	resetTimer := func() {
		if timer == nil {
			timer = time.NewTimer(persistDebounceWindow)
			timerC = timer.C
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(persistDebounceWindow)
	}

	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		timerC = nil
	}

	for {
		select {
		case <-ctx.Done():
			stopTimer()
			m.flushPersistQueue()
			return
		case <-wake:
			resetTimer()
		case <-timerC:
			stopTimer()
			m.flushPersistQueue()
		}
	}
}

func (m *Manager) flushPersistQueue() {
	if m == nil {
		return
	}

	m.persistMu.Lock()
	if len(m.persistIDs) == 0 {
		m.persistMu.Unlock()
		return
	}
	ids := make([]string, 0, len(m.persistIDs))
	for id := range m.persistIDs {
		ids = append(ids, id)
	}
	clear(m.persistIDs)
	m.persistMu.Unlock()

	snapshots := make([]*Auth, 0, len(ids))
	m.mu.RLock()
	for _, id := range ids {
		auth := m.auths[id]
		if auth == nil {
			continue
		}
		snapshots = append(snapshots, auth.Clone())
	}
	m.mu.RUnlock()

	for _, snapshot := range snapshots {
		if err := m.persist(context.Background(), snapshot); err != nil {
			logEntryWithRequestID(context.Background()).
				WithField("auth_id", snapshot.ID).
				Warnf("deferred auth persist failed: %v", err)
		}
		if err := m.persistRuntimeState(context.Background(), snapshot); err != nil {
			logEntryWithRequestID(context.Background()).
				WithField("auth_id", snapshot.ID).
				Warnf("deferred auth runtime state persist failed: %v", err)
		}
	}
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
// When oauth-refresh.enabled is false or unset, provider-level default
// auto-refresh registrations can still start the loop for those providers.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) bool {
	if interval <= 0 {
		interval = refreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}
	if !m.autoRefreshEnabled() {
		return false
	}

	ctx, cancelCtx := context.WithCancel(parent)
	workers := refreshMaxConcurrency
	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
		workers = cfg.AuthAutoRefreshWorkers
	}
	loop := newAuthAutoRefreshLoop(m, interval, workers)

	m.mu.Lock()
	m.refreshCancel = cancelCtx
	m.refreshLoop = loop
	m.refreshSemaphore = make(chan struct{}, workers)
	m.mu.Unlock()

	initialRebuildAt := time.Now()
	if !m.refreshOnStartupEnabled() {
		initialRebuildAt = initialRebuildAt.Add(interval)
	}
	loop.rebuild(initialRebuildAt)
	go loop.run(ctx)
	return true
}

// StopAutoRefresh cancels the background refresh loop, if running.
// It also stops the selector if it implements StoppableSelector.
func (m *Manager) StopAutoRefresh() {
	m.mu.Lock()
	cancel := m.refreshCancel
	mainSelector := m.selector
	kiroSelector := m.kiroSelector
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.stopPersistLoop()
	// Stop selector if it implements StoppableSelector (e.g., SessionAffinitySelector)
	if stoppable, ok := mainSelector.(StoppableSelector); ok {
		stoppable.Stop()
	}
	if kiroSelector != nil && kiroSelector != mainSelector {
		if stoppable, ok := kiroSelector.(StoppableSelector); ok {
			stoppable.Stop()
		}
	}
}

func (m *Manager) checkRefreshes(ctx context.Context) {
	// log.Debugf("checking refreshes")
	now := time.Now()
	snapshot := m.snapshotAuths()
	dueIDs := make([]string, 0, len(snapshot))
	for _, a := range snapshot {
		typ, _ := a.AccountInfo()
		if typ != "api_key" {
			if !m.authAutoRefreshEnabled(a) {
				continue
			}
			if !m.shouldRefresh(a, now) {
				continue
			}
			dueIDs = append(dueIDs, a.ID)
		}
	}
	if len(dueIDs) == 0 {
		m.mu.Lock()
		m.refreshBatchCursor = 0
		m.mu.Unlock()
		return
	}
	sort.Strings(dueIDs)
	limit := m.refreshBatchSize()
	selectedIDs := dueIDs
	if limit > 0 && len(dueIDs) > limit {
		m.mu.Lock()
		start := 0
		if len(dueIDs) > 0 {
			start = m.refreshBatchCursor % len(dueIDs)
		}
		selectedIDs = make([]string, 0, limit)
		for i := 0; i < limit; i++ {
			idx := (start + i) % len(dueIDs)
			selectedIDs = append(selectedIDs, dueIDs[idx])
		}
		m.refreshBatchCursor = (start + limit) % len(dueIDs)
		m.mu.Unlock()
	} else {
		m.mu.Lock()
		m.refreshBatchCursor = 0
		m.mu.Unlock()
	}
	for _, id := range selectedIDs {
		auth, ok := m.GetByID(id)
		if !ok || auth == nil {
			continue
		}
		typ, _ := auth.AccountInfo()
		log.Debugf("checking refresh for %s, %s, %s", auth.Provider, auth.ID, typ)
		if exec := m.executorFor(auth.Provider); exec == nil {
			continue
		}
		if !m.markRefreshPending(auth.ID, now) {
			continue
		}
		go m.refreshAuthWithLimit(ctx, auth.ID)
	}
}

func (m *Manager) refreshAuthWithLimit(ctx context.Context, id string) {
	m.refreshAuthShared(ctx, id, true)
}

func (m *Manager) queueRefreshReschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.queueReschedule(authID)
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if authRefreshSuppressed(a) {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authRefreshSuppressed(auth *Auth) bool {
	return auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth)
}

func authPreferredInterval(a *Auth) time.Duration {
	if d := authConfiguredInterval(a); d > 0 {
		return d
	}
	if a == nil {
		return 0
	}
	return ProviderDefaultRefreshInterval(a.Provider)
}

func authConfiguredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func applyDefaultRefreshInterval(auth *Auth) {
	if auth == nil || !ProviderDefaultAutoRefresh(auth.Provider) || authConfiguredInterval(auth) > 0 {
		return
	}
	interval := ProviderDefaultRefreshInterval(auth.Provider)
	if interval <= 0 {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	seconds := int(interval / time.Second)
	if seconds <= 0 {
		return
	}
	auth.Metadata["refresh_interval_seconds"] = seconds
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		m.mu.Unlock()
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		m.mu.Unlock()
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	m.mu.Unlock()

	m.queueRefreshReschedule(id)
	return true
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	if _, err := m.refreshAuthShared(ctx, id, false); err != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", id).Warnf("auth refresh failed: %v", err)
	}
}

type managerRefreshOutcome struct {
	auth *Auth
	err  error
}

type credentialRetryLimitError struct {
	cause        error
	mode         string
	model        string
	max          int
	attempted    int
	pinnedAuthID string
	sessionID    string
}

func (e *credentialRetryLimitError) Error() string {
	if e == nil {
		return ""
	}
	mode := strings.TrimSpace(e.mode)
	if mode == "" {
		mode = "request"
	}
	msg := fmt.Sprintf("credential failover stopped during %s after %d credential(s) (max-retry-credentials=%d", mode, e.attempted, e.max)
	if model := strings.TrimSpace(e.model); model != "" {
		msg += ", model=" + model
	}
	if authID := strings.TrimSpace(e.pinnedAuthID); authID != "" {
		msg += ", pinned_auth_id=" + authID
	}
	if sessionID := strings.TrimSpace(e.sessionID); sessionID != "" {
		msg += ", session_id=" + sessionID
	}
	msg += ")"
	if e.cause != nil {
		msg += ": " + e.cause.Error()
	}
	return msg
}

func (e *credentialRetryLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *credentialRetryLimitError) StatusCode() int {
	if e == nil || e.cause == nil {
		return 0
	}
	return statusCodeFromError(e.cause)
}

func (m *Manager) credentialRetryLimitReachedError(ctx context.Context, mode string, providers []string, model string, opts cliproxyexecutor.Options, maxRetryCredentials int, attempted map[string]struct{}, lastErr error) error {
	attemptedCount := len(attempted)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	sessionID, fallbackSessionID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)

	fields := log.Fields{
		"mode":                  mode,
		"model":                 model,
		"providers":             strings.Join(providers, ","),
		"attempted_credentials": attemptedCount,
		"max_retry_credentials": maxRetryCredentials,
	}
	if pinnedAuthID != "" {
		fields["pinned_auth_id"] = pinnedAuthID
	}
	if sessionID != "" {
		fields["session_id"] = sessionID
	}
	if fallbackSessionID != "" && fallbackSessionID != sessionID {
		fields["fallback_session_id"] = fallbackSessionID
	}
	entry := logEntryWithRequestID(ctx).WithFields(fields)
	if lastErr != nil {
		entry.Warnf("auth failover stopped by max-retry-credentials: %v", lastErr)
		return &credentialRetryLimitError{
			cause:        lastErr,
			mode:         mode,
			model:        model,
			max:          maxRetryCredentials,
			attempted:    attemptedCount,
			pinnedAuthID: pinnedAuthID,
			sessionID:    sessionID,
		}
	}
	entry.Warn("auth failover stopped by max-retry-credentials before another credential was selected")
	return &Error{
		Code:    "auth_not_found",
		Message: fmt.Sprintf("no auth available (max-retry-credentials=%d reached after %d credential(s))", maxRetryCredentials, attemptedCount),
	}
}

func (m *Manager) refreshAuthShared(ctx context.Context, id string, useLimit bool) (*Auth, error) {
	if id == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result, err, _ := m.refreshGroup.Do(id, func() (any, error) {
		if !m.acquireRefreshSlot(ctx, useLimit) {
			return managerRefreshOutcome{err: ctx.Err()}, nil
		}
		defer m.releaseRefreshSlot(useLimit)
		updated, refreshErr := m.refreshAuthOnce(ctx, id)
		return managerRefreshOutcome{auth: updated, err: refreshErr}, nil
	})
	if err != nil {
		return nil, err
	}
	outcome, ok := result.(managerRefreshOutcome)
	if !ok {
		return m.authCloneByID(id), nil
	}
	if outcome.auth == nil && outcome.err == nil {
		outcome.auth = m.authCloneByID(id)
	}
	return outcome.auth, outcome.err
}

func (m *Manager) RefreshAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	return m.coordinatedRefreshForRequest(ctx, auth)
}

func (m *Manager) RefreshAuthIfNeeded(ctx context.Context, auth *Auth) (*Auth, error) {
	if m == nil || auth == nil {
		return auth, nil
	}
	current := auth
	if auth.ID != "" {
		if snapshot := m.authCloneByID(auth.ID); snapshot != nil {
			current = snapshot
		}
	}
	if !m.authAutoRefreshEnabled(current) || !m.shouldRefresh(current, time.Now()) {
		return current.Clone(), nil
	}
	return m.coordinatedRefreshForRequest(ctx, current)
}

// coordinatedRefreshForRequest serializes request-time refreshes through the
// same singleflight group used by the auto-refresh loop. Without this, the
// executor's own Refresh call could race the background loop on the same
// auth, using the rotating refresh token twice and bricking credentials
// with invalid_grant on the second attempt.
//
// Returns the freshly refreshed auth after the Manager has persisted it,
// or the error the executor's Refresh produced. On permanent failure
// (invalid_grant / revoked token), the error implements PermanentAuthError
// so callers can stop retrying with the same credentials.
func (m *Manager) coordinatedRefreshForRequest(ctx context.Context, auth *Auth) (*Auth, error) {
	if m == nil || auth == nil || auth.ID == "" {
		return auth, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id := auth.ID
	result, err, _ := m.refreshGroup.Do(id, func() (any, error) {
		m.mu.RLock()
		current := m.auths[id]
		if current == nil {
			m.mu.RUnlock()
			return managerRefreshOutcome{auth: auth.Clone()}, nil
		}
		if current.Disabled || current.Status == StatusDisabled {
			currentClone := current.Clone()
			m.mu.RUnlock()
			return managerRefreshOutcome{auth: currentClone}, nil
		}
		exec := m.executors[current.Provider]
		cloned := current.Clone()
		m.mu.RUnlock()
		if exec == nil {
			return managerRefreshOutcome{auth: cloned}, nil
		}
		updated, refreshErr := exec.Refresh(ctx, cloned)
		if refreshErr != nil {
			// Bubble up permanent failures so the conductor can park the
			// auth; the auto-refresh loop will also see the error on its
			// next tick and apply the same treatment.
			if isPermanentRefreshError(refreshErr) {
				refreshErrInfo := refreshErrorFromError(refreshErr)
				unauthorized := refreshErrInfo != nil && refreshErrInfo.StatusCode() == http.StatusUnauthorized
				shouldPersist := false
				m.mu.Lock()
				if cur := m.auths[id]; !authRefreshSuppressed(cur) {
					now := time.Now()
					if unauthorized {
						cur.NextRefreshAfter = time.Time{}
					} else {
						cur.NextRefreshAfter = now.Add(refreshPermanentBackoff)
					}
					cur.LastError = refreshErrInfo
					cur.Status = StatusError
					cur.Unavailable = true
					cur.StatusMessage = "refresh token invalid — re-login required"
					m.auths[id] = cur
					shouldPersist = true
					if m.scheduler != nil {
						m.scheduler.upsertAuth(cur.CloneForScheduler())
					}
				}
				m.mu.Unlock()
				m.queueRefreshReschedule(id)
				if shouldPersist {
					m.enqueuePersistAuthID(ctx, id)
				}
			}
			return managerRefreshOutcome{err: refreshErr}, nil
		}
		if updated == nil {
			return managerRefreshOutcome{auth: cloned}, nil
		}
		// Persist the rotated credentials through Manager.Update so the new
		// refresh token lands on disk synchronously before the caller uses
		// the returned auth. Update also resets NextRefreshAfter using the
		// executor's intended interval, and rebroadcasts to the scheduler.
		persisted, updateErr := m.Update(withRefreshUpdate(ctx), updated)
		if updateErr != nil {
			logEntryWithRequestID(ctx).WithField("auth_id", id).Warnf("coordinated refresh: persist failed: %v", updateErr)
			return managerRefreshOutcome{auth: updated, err: updateErr}, nil
		}
		if persisted != nil {
			return managerRefreshOutcome{auth: persisted}, nil
		}
		return managerRefreshOutcome{auth: updated}, nil
	})
	if err != nil {
		return nil, err
	}
	outcome, ok := result.(managerRefreshOutcome)
	if !ok {
		return m.authCloneByID(id), nil
	}
	if outcome.auth == nil && outcome.err == nil {
		outcome.auth = m.authCloneByID(id)
	}
	return outcome.auth, outcome.err
}

func (m *Manager) authCloneByID(id string) *Auth {
	if m == nil || id == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if auth := m.auths[id]; auth != nil {
		return auth.Clone()
	}
	return nil
}

func (m *Manager) acquireRefreshSlot(ctx context.Context, useLimit bool) bool {
	if !useLimit || m.refreshSemaphore == nil {
		return true
	}
	select {
	case m.refreshSemaphore <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *Manager) releaseRefreshSlot(useLimit bool) {
	if !useLimit || m.refreshSemaphore == nil {
		return
	}
	<-m.refreshSemaphore
}

func (m *Manager) refreshAuthOnce(ctx context.Context, id string) (*Auth, error) {
	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	var cloned *Auth
	if !authRefreshSuppressed(auth) {
		exec = m.executors[auth.Provider]
		cloned = auth.Clone()
	}
	m.mu.RUnlock()
	if cloned == nil || exec == nil {
		return nil, nil
	}
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return cloned, err
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		// Permanent auth errors (invalid_grant / invalid_client / revoked
		// refresh token) must not be retried on the background cadence —
		// doing so burns upstream quota and keeps the auth permanently in a
		// failing state. Park it until an operator re-logs in.
		permanent := isPermanentRefreshError(err)
		refreshErrInfo := refreshErrorFromError(err)
		unauthorized := refreshErrInfo != nil && refreshErrInfo.StatusCode() == http.StatusUnauthorized
		shouldReschedule := false
		shouldPersist := false
		m.mu.Lock()
		if current := m.auths[id]; !authRefreshSuppressed(current) {
			if permanent {
				if unauthorized {
					current.NextRefreshAfter = time.Time{}
				} else {
					current.NextRefreshAfter = now.Add(refreshPermanentBackoff)
				}
				current.Status = StatusError
				current.Unavailable = true
				current.StatusMessage = "refresh token invalid — re-login required"
			} else {
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			}
			current.LastError = refreshErrInfo
			m.auths[id] = current
			shouldReschedule = true
			shouldPersist = true
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.CloneForScheduler())
			}
		}
		m.mu.Unlock()
		if permanent {
			log.Warnf("auth %s (%s) parked for re-login: %v", auth.ID, auth.Provider, err)
		}
		if shouldReschedule {
			m.queueRefreshReschedule(id)
		}
		if shouldPersist {
			m.enqueuePersistAuthID(ctx, id)
		}
		return m.authCloneByID(id), err
	}
	if updated == nil {
		updated = cloned
	}
	executorNextRefreshAfter := updated.NextRefreshAfter
	inheritedNextRefreshAfter := cloned.NextRefreshAfter
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	if !executorNextRefreshAfter.IsZero() &&
		executorNextRefreshAfter.After(now) &&
		!executorNextRefreshAfter.Equal(inheritedNextRefreshAfter) {
		updated.NextRefreshAfter = executorNextRefreshAfter
	} else if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	persisted, updateErr := m.Update(withRefreshUpdate(ctx), updated)
	if updateErr != nil {
		return updated, updateErr
	}
	if persisted != nil {
		return persisted, nil
	}
	return updated, nil
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[provider]
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec = m.executors[executorKeyFromAuth(a)]
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(ctx context.Context, auth *Auth, method, targetURL string, body []byte, headers http.Header) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	execCtx := ctx
	if execCtx == nil {
		execCtx = req.Context()
	}
	if rt := m.roundTripperFor(auth); rt != nil {
		execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
		execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
	}
	execCtx = WithRefreshUpdateCallback(execCtx, m.handleExecutionRefreshUpdate)
	execCtx = WithAuthUpdateCallback(execCtx, m.handleExecutionAuthUpdate)
	execCtx = WithRefreshCoordinator(execCtx, m.coordinatedRefreshForRequest)
	return exec.HttpRequest(execCtx, auth, req)
}
