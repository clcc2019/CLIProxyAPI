package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
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
	refreshCheckInterval             = 5 * time.Second
	refreshMaxConcurrency            = 16
	refreshPendingBackoff            = time.Minute
	refreshFailureBackoff            = 5 * time.Minute
	refreshBatchDrainDelay           = time.Second
	quotaRefreshInterval             = 5 * time.Hour
	persistDebounceWindow            = time.Second
	proxyPoolReconcileDebounceWindow = 100 * time.Millisecond
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
	// implements AuthScopedFailure. When true, MarkResult suspends the auth as
	// a whole instead of only Model.
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
	store                 Store
	runtimeStateStore     RuntimeStateStore
	proxyLeaseStore       ProxyLeaseStore
	previousResponseStore PreviousResponseStore
	executors             map[string]ProviderExecutor
	selector              Selector
	hook                  Hook
	mu                    sync.RWMutex
	auths                 map[string]*Auth
	removedAuths          map[string]authRemovalTombstone
	runtimeStates         map[string]AuthRuntimeState
	scheduler             *authScheduler
	authInFlightCounts    sync.Map
	// homeRuntimeAuths caches auths returned by Home so websocket sessions can
	// reuse an established upstream credential without dispatching every turn.
	homeRuntimeAuths map[string]map[string]*Auth
	// previousResponseAuths keeps OpenAI/Codex response IDs sticky to the auth
	// that created them, preventing previous_response_id continuations from
	// being routed to a different credential.
	previousResponseAuths *previousResponseAuthCache
	// routeAwareSingleCache stores provider/model pairs proven safe for scheduler single-provider fast path.
	routeAwareSingleCache sync.Map
	// routeAwareMixedCache stores provider/model combinations proven safe for scheduler mixed fast path.
	routeAwareMixedCache sync.Map

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
	persistLocks   sync.Map

	// Debounced proxy-pool reconciliation after auth changes.
	proxyReconcileMu       sync.Mutex
	proxyReconcileWake     chan struct{}
	proxyReconcileCancel   context.CancelFunc
	proxyReconcileWG       sync.WaitGroup
	proxyReconcilePending  bool
	proxyReconcileStopping bool
	proxyReconcileStopDone chan struct{}
	proxyRecoveryTimers    map[string]*proxyPoolRecoveryTimer
	proxyRecoverySequence  uint64
	proxyRecoveryWG        sync.WaitGroup
}

type authRemovalTombstone struct {
	Path      string
	RemovedAt time.Time
}

const (
	proxyPoolAssignedAttribute  = "proxy_pool_assigned"
	proxyPoolAssignedValue      = "true"
	defaultProxyFailureCooldown = 10 * time.Minute
	apiKeyPoolModeRetryCount    = 3
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
		store:                 store,
		executors:             make(map[string]ProviderExecutor),
		selector:              selector,
		hook:                  hook,
		auths:                 make(map[string]*Auth),
		removedAuths:          make(map[string]authRemovalTombstone),
		homeRuntimeAuths:      make(map[string]map[string]*Auth),
		previousResponseAuths: newPreviousResponseAuthCache(previousResponseAuthTTL, previousResponseAuthMaxEntries),
		modelPoolOffsets:      make(map[string]int),
		refreshSemaphore:      make(chan struct{}, refreshMaxConcurrency),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.persistEnabled.Store(store != nil)
	manager.scheduler = newAuthScheduler(selector)
	manager.scheduler.authLoad = manager.authInFlightCount
	return manager
}

type authInFlightLease struct {
	manager *Manager
	authID  string
	counter *authInFlightCounter
	state   atomic.Int32
}

type authInFlightCounter struct {
	// value is the active lease count. -1 marks an entry retired so a racing
	// acquirer retries against a fresh map entry instead of resurrecting it.
	value atomic.Int64
}

func (m *Manager) beginAuthInFlight(authID string) *authInFlightLease {
	if m == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	for {
		value, _ := m.authInFlightCounts.LoadOrStore(authID, &authInFlightCounter{})
		counter, ok := value.(*authInFlightCounter)
		if !ok || counter == nil {
			m.authInFlightCounts.Delete(authID)
			continue
		}
		for {
			count := counter.value.Load()
			if count < 0 {
				m.authInFlightCounts.CompareAndDelete(authID, counter)
				break
			}
			if counter.value.CompareAndSwap(count, count+1) {
				return &authInFlightLease{manager: m, authID: authID, counter: counter}
			}
		}
	}
}

func (m *Manager) authInFlightCount(authID string) int64 {
	if m == nil {
		return 0
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}
	value, ok := m.authInFlightCounts.Load(authID)
	if !ok {
		return 0
	}
	counter, ok := value.(*authInFlightCounter)
	if !ok || counter == nil {
		return 0
	}
	if count := counter.value.Load(); count > 0 {
		return count
	}
	return 0
}

func (l *authInFlightLease) Close() {
	if l == nil || l.counter == nil {
		return
	}
	if !l.state.CompareAndSwap(0, 2) {
		return
	}
	l.release()
}

func (l *authInFlightLease) HandOff() {
	if l == nil {
		return
	}
	l.state.CompareAndSwap(0, 1)
}

func (l *authInFlightLease) Finish() {
	if l == nil || l.counter == nil {
		return
	}
	for {
		state := l.state.Load()
		switch state {
		case 2:
			return
		case 0, 1:
			if l.state.CompareAndSwap(state, 2) {
				l.release()
				return
			}
		default:
			return
		}
	}
}

func (l *authInFlightLease) release() {
	if l == nil || l.counter == nil {
		return
	}
	for {
		count := l.counter.value.Load()
		if count <= 0 {
			return
		}
		if count > 1 {
			if l.counter.value.CompareAndSwap(count, count-1) {
				return
			}
			continue
		}
		if !l.counter.value.CompareAndSwap(1, -1) {
			continue
		}
		if l.manager != nil && l.authID != "" {
			l.manager.authInFlightCounts.CompareAndDelete(l.authID, l.counter)
		}
		return
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
			runtimeSnapshot = auth.Clone()
			snapshot = auth.CloneForScheduler()
		} else if schedulerRefresh {
			snapshot = auth.CloneForScheduler()
		}
	}
	m.mu.Unlock()

	if runtimeSnapshot != nil {
		if errPersist := m.persist(ctx, runtimeSnapshot); errPersist != nil {
			logEntryWithRequestID(ctx).WithField("auth_id", runtimeSnapshot.ID).Warnf("failed to persist auth changes during model state reconciliation: %v", errPersist)
		}
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

// SetPreviousResponseStore swaps the optional persistence store used for
// response-to-auth bindings across proxy instances.
func (m *Manager) SetPreviousResponseStore(store PreviousResponseStore) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.previousResponseStore = store
	m.mu.Unlock()
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
	m.configurePreviousResponseAffinity(cfg)
	if !cfg.Home.Enabled {
		m.clearHomeRuntimeAuths()
	}
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
