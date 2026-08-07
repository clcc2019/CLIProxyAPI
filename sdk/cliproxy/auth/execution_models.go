package auth

import (
	"sort"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

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
	requestResult := thinking.ParseSuffix(requestedModel)
	keys := []string{strings.ToLower(requestedModel)}
	baseKey := strings.ToLower(strings.TrimSpace(requestResult.ModelName))
	if baseKey != "" && baseKey != keys[0] {
		keys = append(keys, baseKey)
	}
	for _, key := range keys {
		resolved := strings.TrimSpace(byAlias[key])
		if resolved == "" {
			continue
		}
		return preserveRequestedModelSuffix(requestedModel, resolved)
	}
	return ""
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
	providerKey := ""
	compatName := ""
	baseURL := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	rawSnapshot := m.openAICompatRuntime.Load()
	snapshot, _ := rawSnapshot.(*openAICompatRuntimeSnapshot)
	entry := snapshot.resolve(providerKey, compatName, auth.Provider, baseURL)
	if entry == nil {
		return nil
	}
	return entry.resolveModelPool(requestedModel)
}

func (m *Manager) apiKeyPoolModeRetries(auth *Auth) int {
	if m == nil || auth == nil {
		return 0
	}
	if isOpenAICompatAPIKeyAuth(auth) {
		providerKey := ""
		compatName := ""
		baseURL := ""
		if auth.Attributes != nil {
			providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
			compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		}
		rawSnapshot := m.openAICompatRuntime.Load()
		snapshot, _ := rawSnapshot.(*openAICompatRuntimeSnapshot)
		entry := snapshot.resolve(providerKey, compatName, auth.Provider, baseURL)
		if entry != nil && entry.poolMode {
			return apiKeyPoolModeRetryCount
		}
	}

	if strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && isAPIKeyAuth(auth) {
		cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
		if cfg == nil {
			return 0
		}
		entry := resolveCodexAPIKeyConfig(cfg, auth)
		if entry != nil && entry.PoolMode {
			return apiKeyPoolModeRetryCount
		}
	}

	return 0
}

type poolModeRetryDecision uint8

const (
	poolModeRetryStop poolModeRetryDecision = iota
	poolModeRetryRetry
	poolModeRetryInvalidRequest
)

func poolModeRetryDecisionForError(err error, retryAttempt, poolModeRetries int) poolModeRetryDecision {
	if err == nil {
		return poolModeRetryStop
	}
	if isRequestInvalidError(err) {
		return poolModeRetryInvalidRequest
	}
	if retryAttempt < poolModeRetries {
		return poolModeRetryRetry
	}
	return poolModeRetryStop
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
	if strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return auth.CloneShallow()
	}
	return auth.cloneForExecution()
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
	if auth == nil || providerKey == "" || auth.IsDisabled() {
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
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	return selector
}

func (m *Manager) authSupportsRouteModel(registryRef *registry.ModelRegistry, auth *Auth, routeModel string) bool {
	if registryRef == nil || auth == nil {
		return true
	}
	routeModel = strings.TrimSpace(routeModel)
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return true
	}
	if registryRef.ClientSupportsModel(auth.ID, routeModel) {
		return true
	}
	if registryRef.ClientSupportsModel(auth.ID, routeKey) {
		return true
	}
	selectionModel := m.selectionModelForAuth(auth, routeModel)
	if selectionModel != "" && registryRef.ClientSupportsModel(auth.ID, selectionModel) {
		return true
	}
	selectionKey := canonicalModelKey(selectionModel)
	return selectionKey != "" && registryRef.ClientSupportsModel(auth.ID, selectionKey)
}
