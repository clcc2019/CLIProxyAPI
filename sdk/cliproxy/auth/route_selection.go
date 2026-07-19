package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

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
	return isBuiltInSelector(m.selector) || schedulerBackedSessionAffinitySelector(m.selector) != nil
}

func useSchedulerFastPathForProviders(m *Manager, providers []string) bool {
	if m == nil || m.scheduler == nil {
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
	case "claude", "codex":
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
	case "kimi", "xai":
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
	opts = withProviderScopeMetadata(opts, normalizeProviderKeys(providers))
	selector := m.selector
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
	model = strings.TrimSpace(model)
	if model != "" && m.mixedLegacySelectionRequired(eligibleProviders, model, tried) {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}
	if affinity := schedulerBackedSessionAffinitySelector(m.selector); affinity != nil {
		opts = withProviderScopeMetadata(opts, eligibleProviders)
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
		selected, providerKey, errPick := m.scheduler.pickMixedStableNormalized(ctx, eligibleProviders, model, opts, tried, affinityKey)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, providerKey, errPick = m.scheduler.pickMixedStableNormalized(ctx, eligibleProviders, model, opts, tried, affinityKey)
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
		selected, providerKey, errPick := m.scheduler.pickMixedNormalized(ctx, eligibleProviders, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, providerKey, errPick = m.scheduler.pickMixedNormalized(ctx, eligibleProviders, model, opts, tried)
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
