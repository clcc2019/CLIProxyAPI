package auth

import (
	"context"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// schedulerStrategy identifies which built-in routing semantics the scheduler should apply.
type schedulerStrategy int

const (
	schedulerStrategyCustom schedulerStrategy = iota
	schedulerStrategyRoundRobin
	schedulerStrategyFillFirst
	schedulerStrategyWeightedRoundRobin
)

// scheduledState describes how an auth currently participates in a model shard.
type scheduledState int

const (
	scheduledStateReady scheduledState = iota
	scheduledStateCooldown
	scheduledStateBlocked
	scheduledStateDisabled
)

type authLoadFunc func(authID string) int64

const (
	codexQuotaSchedulingSnapshotTTL = 45 * time.Minute
	codexQuotaSchedulingClockSkew   = 5 * time.Minute
	// schedulerModelShardTTL bounds how long an inactive provider/model view is
	// retained. Shards are derived from current credential metadata, so they can
	// be rebuilt safely after eviction.
	schedulerModelShardTTL = 30 * time.Minute
	// schedulerMaxModelShardsPerProvider prevents untrusted, high-cardinality
	// model names from permanently growing scheduler state.
	schedulerMaxModelShardsPerProvider = 256
	// schedulerMaxMixedCursors bounds round-robin cursor state for mixed
	// provider routing, whose keys also contain the externally supplied model.
	schedulerMaxMixedCursors = 4096
)

type codexQuotaSchedulingRank uint8

const (
	codexQuotaSchedulingDepleted codexQuotaSchedulingRank = iota
	codexQuotaSchedulingUnknown
	codexQuotaSchedulingUsable
)

type codexQuotaSchedulingScore struct {
	rank    codexQuotaSchedulingRank
	urgency float64
}

// authScheduler keeps the incremental provider/model scheduling state used by Manager.
type authScheduler struct {
	mu            sync.RWMutex
	strategy      schedulerStrategy
	authLoad      authLoadFunc
	providers     map[string]*providerScheduler
	authProviders map[string]string
	mixedCursorMu sync.RWMutex
	mixedCursors  map[mixedCursorKey]*atomic.Uint64
	largeCursors  map[string]*atomic.Uint64

	mixedWeightedMu     sync.Mutex
	mixedWeightedStates map[string]*smoothWeightedState
}

type mixedProviderCandidate struct {
	shard           *modelScheduler
	priority        int
	readyCount      int
	minLoad         int64
	minLoadCount    int
	preferWebsocket bool
	weight          int
	segmentStart    int
	segmentEnd      int
}

type mixedCursorKey struct {
	modelKey      string
	providerCount uint8
	providers     [4]string
}

// providerScheduler stores auth metadata and model shards for a single provider.
type providerScheduler struct {
	mu          sync.RWMutex
	providerKey string
	auths       map[string]*scheduledAuthMeta
	modelShards map[string]*modelScheduler
}

// scheduledAuthMeta stores the immutable scheduling fields derived from an auth snapshot.
type scheduledAuthMeta struct {
	auth              *Auth
	providerKey       string
	priority          int
	weight            int64
	websocketEnabled  bool
	supportedModelSet map[string]struct{}
}

// modelScheduler tracks ready and blocked auths for one provider/model combination.
type modelScheduler struct {
	mu               sync.RWMutex
	modelKey         string
	lastUsedUnixNano atomic.Int64
	entries          map[string]*scheduledAuth
	priorityOrder    []int
	readyByPriority  map[int]*readyBucket
	blocked          cooldownQueue
	nextBlockedRetry time.Time
}

// scheduledAuth stores the runtime scheduling state for a single auth inside a model shard.
type scheduledAuth struct {
	meta        *scheduledAuthMeta
	auth        *Auth
	state       scheduledState
	nextRetryAt time.Time
}

// authFilter carries request-scoped auth constraints without allocating closures.
type authFilter struct {
	pinnedAuthID string
	tried        map[string]struct{}
	hasPinned    bool
	hasTried     bool
	positiveOnly bool
}

func newAuthFilter(pinnedAuthID string, tried map[string]struct{}) authFilter {
	return authFilter{
		pinnedAuthID: pinnedAuthID,
		tried:        tried,
		hasPinned:    pinnedAuthID != "",
		hasTried:     len(tried) > 0,
	}
}

func (f authFilter) empty() bool {
	return !f.hasPinned && !f.hasTried && !f.positiveOnly
}

func (f authFilter) matchesAuthID(authID string) bool {
	if f.hasPinned && authID != f.pinnedAuthID {
		return false
	}
	if !f.hasTried {
		return true
	}
	_, tried := f.tried[authID]
	return !tried
}

func (f authFilter) matches(entry *scheduledAuth) bool {
	if entry == nil || entry.auth == nil {
		return false
	}
	if f.positiveOnly && (entry.meta == nil || entry.meta.weight <= 0) {
		return false
	}
	return f.matchesAuthID(entry.auth.ID)
}

func authNotFoundErrorForFilter(filter authFilter) *Error {
	return &Error{Code: "auth_not_found", Message: authUnavailableMessageForFilter(filter)}
}

func authUnavailableErrorForFilter(filter authFilter) *Error {
	return &Error{Code: "auth_unavailable", Message: authUnavailableMessageForFilter(filter)}
}

func authUnavailableMessageForFilter(filter authFilter) string {
	if filter.hasPinned {
		return "no auth available for pinned auth " + filter.pinnedAuthID
	}
	return "no auth available"
}

// readyBucket keeps the ready views for one priority level.
type readyBucket struct {
	all readyView
	ws  readyView
}

// readyView holds the selection order and per-view state for built-in strategies.
type readyView struct {
	cursor        int
	flat          []*scheduledAuth
	weightedState smoothWeightedState
}

// cooldownQueue is the blocked auth collection ordered by next retry time during rebuilds.
type cooldownQueue []*scheduledAuth

type readyViewCursorState struct {
	cursor        int
	weightedState smoothWeightedState
}

type readyBucketCursorState struct {
	all readyViewCursorState
	ws  readyViewCursorState
}

func snapshotReadyViewCursors(view readyView) readyViewCursorState {
	state := readyViewCursorState{cursor: view.cursor}
	if len(view.weightedState.current) > 0 {
		state.weightedState.current = make(map[string]int64, len(view.weightedState.current))
		for authID, current := range view.weightedState.current {
			state.weightedState.current[authID] = current
		}
	}
	if len(view.weightedState.weights) > 0 {
		state.weightedState.weights = make(map[string]int64, len(view.weightedState.weights))
		for authID, weight := range view.weightedState.weights {
			state.weightedState.weights[authID] = weight
		}
	}
	return state
}

func restoreReadyViewCursors(view *readyView, state readyViewCursorState) {
	if view == nil {
		return
	}
	if len(view.flat) > 0 {
		view.cursor = normalizeCursor(state.cursor, len(view.flat))
	}
	view.weightedState = state.weightedState
}

func normalizeCursor(cursor, size int) int {
	if size <= 0 || cursor <= 0 {
		return 0
	}
	cursor = cursor % size
	if cursor < 0 {
		cursor += size
	}
	return cursor
}

// newAuthScheduler constructs an empty scheduler configured for the supplied selector strategy.
func newAuthScheduler(selector Selector) *authScheduler {
	return &authScheduler{
		strategy:            selectorStrategy(selector),
		providers:           make(map[string]*providerScheduler),
		authProviders:       make(map[string]string),
		mixedCursors:        make(map[mixedCursorKey]*atomic.Uint64),
		largeCursors:        make(map[string]*atomic.Uint64),
		mixedWeightedStates: make(map[string]*smoothWeightedState),
	}
}

// selectorStrategy maps a selector implementation to the scheduler semantics it should emulate.
func selectorStrategy(selector Selector) schedulerStrategy {
	switch typed := selector.(type) {
	case *FillFirstSelector:
		return schedulerStrategyFillFirst
	case *WeightedRoundRobinSelector:
		return schedulerStrategyWeightedRoundRobin
	case nil, *RoundRobinSelector:
		return schedulerStrategyRoundRobin
	case *SessionAffinitySelector:
		return selectorStrategy(typed.fallback)
	default:
		return schedulerStrategyCustom
	}
}

// setSelector updates the active built-in strategy and resets mixed-provider cursors.
func (s *authScheduler) setSelector(selector Selector) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.strategy = selectorStrategy(selector)
	s.mu.Unlock()
	s.clearMixedCursors()
}

// rebuild recreates the complete scheduler state from an auth snapshot.
func (s *authScheduler) rebuild(auths []*Auth) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.providers = make(map[string]*providerScheduler)
	s.authProviders = make(map[string]string)
	now := time.Now()
	for _, auth := range auths {
		s.upsertAuthLocked(auth, now)
	}
	s.mu.Unlock()
	s.clearMixedCursors()
}

// upsertAuth incrementally synchronizes one auth into the scheduler.
func (s *authScheduler) upsertAuth(auth *Auth) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertAuthLocked(auth, time.Now())
}

// removeAuth deletes one auth from every scheduler shard that references it.
func (s *authScheduler) removeAuth(authID string) {
	if s == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeAuthLocked(authID)
}

// pickSingle returns the next auth for a single provider/model request using scheduler state.
func (s *authScheduler) pickSingle(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, error) {
	if s == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	modelKey := canonicalModelKey(model)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	preferWebsocket := cliproxyexecutor.PreferUpstreamWebsocket(ctx) && providerKey == "codex" && pinnedAuthID == ""

	s.mu.RLock()
	defer s.mu.RUnlock()
	providerState := s.providers[providerKey]
	filter := newAuthFilter(pinnedAuthID, tried)
	filter.positiveOnly = s.strategy == schedulerStrategyWeightedRoundRobin
	if providerState == nil {
		return nil, authNotFoundErrorForFilter(filter)
	}
	shard := providerState.ensureModel(modelKey, time.Now())
	if shard == nil {
		return nil, authNotFoundErrorForFilter(filter)
	}
	if picked := shard.pickReady(preferWebsocket, s.strategy, filter, s.authLoad); picked != nil {
		return picked, nil
	}
	return nil, shard.unavailableError(provider, model, filter)
}

// pickSingleStable returns a ready auth using rendezvous hashing over the session key.
// It keeps session-affinity cache misses stable across process restarts while still
// respecting priority, websocket preference, pins, tried auths, and cooldown state.
func (s *authScheduler) pickSingleStable(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, affinityKey string) (*Auth, error) {
	if strings.TrimSpace(affinityKey) == "" {
		return s.pickSingle(ctx, provider, model, opts, tried)
	}
	if s == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if s.usesWeightedRoundRobin() {
		// See pickMixedStableNormalized: session affinity supplies stickiness after
		// the first selection, while the binding itself must respect credential share.
		return s.pickSingle(ctx, provider, model, opts, tried)
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	modelKey := canonicalModelKey(model)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	preferWebsocket := cliproxyexecutor.PreferUpstreamWebsocket(ctx) && providerKey == "codex" && pinnedAuthID == ""

	s.mu.RLock()
	defer s.mu.RUnlock()
	providerState := s.providers[providerKey]
	filter := newAuthFilter(pinnedAuthID, tried)
	if s.strategy == schedulerStrategyWeightedRoundRobin {
		filter.positiveOnly = true
	}
	if providerState == nil {
		return nil, authNotFoundErrorForFilter(filter)
	}
	shard := providerState.ensureModel(modelKey, time.Now())
	if shard == nil {
		return nil, authNotFoundErrorForFilter(filter)
	}
	if picked := shard.pickReadyStable(preferWebsocket, filter, affinityKey); picked != nil {
		return picked, nil
	}
	return nil, shard.unavailableError(provider, model, filter)
}

// pickMixed returns the next auth and provider for a mixed-provider request.
func (s *authScheduler) pickMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, string, error) {
	return s.pickMixedNormalized(ctx, normalizeProviderKeys(providers), model, opts, tried)
}

// pickMixedNormalized is pickMixed for callers that already normalized and
// validated the provider list.
func (s *authScheduler) pickMixedNormalized(ctx context.Context, normalized []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, string, error) {
	if s == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if len(normalized) == 0 {
		return nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if len(normalized) == 1 {
		// When a single provider is eligible, reuse pickSingle so provider-specific preferences
		// (for example Codex websocket transport) are applied consistently.
		providerKey := normalized[0]
		picked, errPick := s.pickSingle(ctx, providerKey, model, opts, tried)
		if errPick != nil {
			return nil, "", errPick
		}
		if picked == nil {
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		return picked, providerKey, nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	modelKey := canonicalModelKey(model)
	preferUpstreamWebsocket := cliproxyexecutor.PreferUpstreamWebsocket(ctx)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if pinnedAuthID != "" {
		filter := newAuthFilter(pinnedAuthID, tried)
		filter.positiveOnly = s.strategy == schedulerStrategyWeightedRoundRobin
		providerKey := s.authProviders[pinnedAuthID]
		if providerKey == "" || !containsProvider(normalized, providerKey) {
			return nil, "", authNotFoundErrorForFilter(filter)
		}
		providerState := s.providers[providerKey]
		if providerState == nil {
			return nil, "", authNotFoundErrorForFilter(filter)
		}
		shard := providerState.ensureModel(modelKey, time.Now())
		if picked := shard.pickReady(false, s.strategy, filter, s.authLoad); picked != nil {
			return picked, providerKey, nil
		}
		return nil, "", shard.unavailableError("mixed", model, filter)
	}

	filter := newAuthFilter("", tried)
	filter.positiveOnly = s.strategy == schedulerStrategyWeightedRoundRobin
	var smallCandidates [4]mixedProviderCandidate
	var candidates []mixedProviderCandidate
	if len(normalized) <= len(smallCandidates) {
		candidates = smallCandidates[:len(normalized)]
	} else {
		candidates = make([]mixedProviderCandidate, len(normalized))
	}
	bestPriority := 0
	hasCandidate := false
	now := time.Now()
	for providerIndex, providerKey := range normalized {
		providerState := s.providers[providerKey]
		if providerState == nil {
			continue
		}
		shard := providerState.ensureModel(modelKey, now)
		candidates[providerIndex].shard = shard
		candidates[providerIndex].preferWebsocket = preferWebsocketForProvider(preferUpstreamWebsocket, providerKey)
		if shard == nil {
			continue
		}
		shard.promoteExpired(now)
		priorityReady, readyCount, minLoad, minLoadCount, okPriority := shard.highestReadyPriorityAndLoadStats(candidates[providerIndex].preferWebsocket, filter, s.authLoad)
		if !okPriority {
			continue
		}
		candidates[providerIndex].priority = priorityReady
		candidates[providerIndex].readyCount = readyCount
		candidates[providerIndex].minLoad = minLoad
		candidates[providerIndex].minLoadCount = minLoadCount
		if !hasCandidate || priorityReady > bestPriority {
			bestPriority = priorityReady
			hasCandidate = true
		}
	}
	if !hasCandidate {
		return nil, "", s.mixedUnavailableError(normalized, model, filter)
	}

	if s.strategy == schedulerStrategyFillFirst {
		for providerIndex, providerKey := range normalized {
			shard := candidates[providerIndex].shard
			if shard == nil {
				continue
			}
			picked := shard.pickReadyAtPriority(candidates[providerIndex].preferWebsocket, bestPriority, s.strategy, filter, s.authLoad)
			if picked != nil {
				return picked, providerKey, nil
			}
		}
		return nil, "", s.mixedUnavailableError(normalized, model, filter)
	}

	bestLoad := int64(0)
	hasBestLoad := false
	for providerIndex := range normalized {
		candidate := candidates[providerIndex]
		if candidate.shard == nil || candidate.priority != bestPriority {
			continue
		}
		if !hasBestLoad || candidate.minLoad < bestLoad {
			bestLoad = candidate.minLoad
			hasBestLoad = true
		}
	}
	if !hasBestLoad {
		return nil, "", s.mixedUnavailableError(normalized, model, filter)
	}
	if s.strategy == schedulerStrategyWeightedRoundRobin {
		entries := make([]*scheduledAuth, 0)
		for providerIndex := range normalized {
			candidate := candidates[providerIndex]
			if candidate.shard == nil || candidate.priority != bestPriority || candidate.minLoad != bestLoad {
				continue
			}
			entries = append(entries, candidate.shard.weightedEntriesAtPriority(candidate.preferWebsocket, bestPriority, filter, s.authLoad, bestLoad)...)
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i] == nil || entries[i].auth == nil {
				return false
			}
			if entries[j] == nil || entries[j].auth == nil {
				return true
			}
			return entries[i].auth.ID < entries[j].auth.ID
		})
		if picked := s.pickMixedWeighted(strings.Join(normalized, ",")+":"+modelKey, entries); picked != nil && picked.meta != nil {
			return picked.auth, picked.meta.providerKey, nil
		}
		return nil, "", s.mixedUnavailableError(normalized, model, filter)
	}

	totalWeight := 0
	for providerIndex := range normalized {
		candidates[providerIndex].segmentStart = totalWeight
		if candidates[providerIndex].shard != nil && candidates[providerIndex].priority == bestPriority && candidates[providerIndex].minLoad == bestLoad {
			weight := candidates[providerIndex].minLoadCount
			if weight <= 0 {
				weight = candidates[providerIndex].readyCount
			}
			candidates[providerIndex].weight = weight
			totalWeight += weight
		}
		candidates[providerIndex].segmentEnd = totalWeight
	}
	if totalWeight == 0 {
		return nil, "", s.mixedUnavailableError(normalized, model, filter)
	}

	cursor := s.ensureMixedCursor(normalized, modelKey)
	startSlot := int(cursor.Add(1)-1) % totalWeight
	startProviderIndex := -1
	for providerIndex := range normalized {
		if candidates[providerIndex].weight == 0 || candidates[providerIndex].priority != bestPriority {
			continue
		}
		if startSlot < candidates[providerIndex].segmentEnd {
			startProviderIndex = providerIndex
			break
		}
	}
	if startProviderIndex < 0 {
		return nil, "", s.mixedUnavailableError(normalized, model, filter)
	}

	localOffset := startSlot - candidates[startProviderIndex].segmentStart
	selectedShard := candidates[startProviderIndex].shard
	if selectedShard != nil {
		if picked := selectedShard.pickReadyAtPriorityOffset(candidates[startProviderIndex].preferWebsocket, bestPriority, localOffset, filter, s.authLoad); picked != nil {
			return picked, normalized[startProviderIndex], nil
		}
	}

	for offset := 0; offset < len(normalized); offset++ {
		providerIndex := (startProviderIndex + offset) % len(normalized)
		if candidates[providerIndex].weight == 0 || candidates[providerIndex].priority != bestPriority {
			continue
		}
		providerKey := normalized[providerIndex]
		shard := candidates[providerIndex].shard
		if shard == nil {
			continue
		}
		picked := shard.pickReadyAtPriorityOffset(candidates[providerIndex].preferWebsocket, bestPriority, 0, filter, s.authLoad)
		if picked == nil {
			continue
		}
		return picked, providerKey, nil
	}
	return nil, "", s.mixedUnavailableError(normalized, model, filter)
}

func (s *authScheduler) pickMixedStable(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, affinityKey string) (*Auth, string, error) {
	return s.pickMixedStableNormalized(ctx, normalizeProviderKeys(providers), model, opts, tried, affinityKey)
}

func (s *authScheduler) pickMixedStableNormalized(ctx context.Context, normalized []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, affinityKey string) (*Auth, string, error) {
	if strings.TrimSpace(affinityKey) == "" {
		return s.pickMixedNormalized(ctx, normalized, model, opts, tried)
	}
	if s == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if s.usesWeightedRoundRobin() {
		// A weighted session-affinity miss must establish its binding with smooth
		// WRR, rather than rendezvous hashing, so new sessions retain the configured
		// share. Subsequent requests remain sticky through the affinity cache.
		return s.pickMixedNormalized(ctx, normalized, model, opts, tried)
	}
	if len(normalized) == 0 {
		return nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if len(normalized) == 1 {
		providerKey := normalized[0]
		picked, errPick := s.pickSingleStable(ctx, providerKey, model, opts, tried, affinityKey)
		if errPick != nil {
			return nil, "", errPick
		}
		if picked == nil {
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		return picked, providerKey, nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	modelKey := canonicalModelKey(model)
	preferUpstreamWebsocket := cliproxyexecutor.PreferUpstreamWebsocket(ctx)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if pinnedAuthID != "" {
		filter := newAuthFilter(pinnedAuthID, tried)
		providerKey := s.authProviders[pinnedAuthID]
		if providerKey == "" || !containsProvider(normalized, providerKey) {
			return nil, "", authNotFoundErrorForFilter(filter)
		}
		providerState := s.providers[providerKey]
		if providerState == nil {
			return nil, "", authNotFoundErrorForFilter(filter)
		}
		shard := providerState.ensureModel(modelKey, time.Now())
		if picked := shard.pickReadyStable(false, filter, affinityKey); picked != nil {
			return picked, providerKey, nil
		}
		return nil, "", shard.unavailableError("mixed", model, filter)
	}

	filter := newAuthFilter("", tried)
	bestPriority := 0
	hasCandidate := false
	now := time.Now()
	for _, providerKey := range normalized {
		providerState := s.providers[providerKey]
		if providerState == nil {
			continue
		}
		shard := providerState.ensureModel(modelKey, now)
		if shard == nil {
			continue
		}
		shard.promoteExpired(now)
		priorityReady, okPriority := shard.highestReadyPriority(preferWebsocketForProvider(preferUpstreamWebsocket, providerKey), filter)
		if !okPriority {
			continue
		}
		if !hasCandidate || priorityReady > bestPriority {
			bestPriority = priorityReady
			hasCandidate = true
		}
	}
	if !hasCandidate {
		return nil, "", s.mixedUnavailableError(normalized, model, filter)
	}

	var picked *Auth
	pickedProvider := ""
	bestScore := uint64(0)
	for _, providerKey := range normalized {
		providerState := s.providers[providerKey]
		if providerState == nil {
			continue
		}
		shard := providerState.ensureModel(modelKey, now)
		if shard == nil {
			continue
		}
		candidate := shard.pickReadyStableAtPriority(preferWebsocketForProvider(preferUpstreamWebsocket, providerKey), bestPriority, filter, affinityKey)
		if candidate == nil {
			continue
		}
		score := stableAffinityScore(affinityKey, candidate.ID)
		if picked == nil || score > bestScore || (score == bestScore && candidate.ID < picked.ID) {
			picked = candidate
			pickedProvider = providerKey
			bestScore = score
		}
	}
	if picked == nil {
		return nil, "", s.mixedUnavailableError(normalized, model, filter)
	}
	return picked, pickedProvider, nil
}

func (s *authScheduler) clearMixedCursors() {
	if s == nil {
		return
	}
	s.mixedCursorMu.Lock()
	s.mixedCursors = make(map[mixedCursorKey]*atomic.Uint64)
	s.largeCursors = make(map[string]*atomic.Uint64)
	s.mixedCursorMu.Unlock()
	s.mixedWeightedMu.Lock()
	s.mixedWeightedStates = make(map[string]*smoothWeightedState)
	s.mixedWeightedMu.Unlock()
}

func (s *authScheduler) usesWeightedRoundRobin() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	weighted := s.strategy == schedulerStrategyWeightedRoundRobin
	s.mu.RUnlock()
	return weighted
}

func (s *authScheduler) pickMixedWeighted(key string, entries []*scheduledAuth) *scheduledAuth {
	entries = filterWeightedScheduledByQuota(entries)
	if len(entries) == 0 {
		return nil
	}
	s.mixedWeightedMu.Lock()
	defer s.mixedWeightedMu.Unlock()
	if s.mixedWeightedStates == nil {
		s.mixedWeightedStates = make(map[string]*smoothWeightedState)
	}
	if _, ok := s.mixedWeightedStates[key]; !ok && len(s.mixedWeightedStates) >= 4096 {
		s.mixedWeightedStates = make(map[string]*smoothWeightedState)
	}
	state := s.mixedWeightedStates[key]
	if state == nil {
		state = &smoothWeightedState{}
		s.mixedWeightedStates[key] = state
	}
	state.prepare(scheduledWeightVector(entries))
	return pickSmoothWeightedScheduled(entries, state.current)
}

func (s *authScheduler) ensureMixedCursor(providers []string, modelKey string) *atomic.Uint64 {
	if s == nil {
		return nil
	}
	cursorKey, okCursorKey := makeMixedCursorKey(providers, modelKey)
	if okCursorKey {
		return s.ensureSmallMixedCursor(cursorKey)
	}
	return s.ensureLargeMixedCursor(strings.Join(providers, ",") + ":" + modelKey)
}

func (s *authScheduler) ensureSmallMixedCursor(cursorKey mixedCursorKey) *atomic.Uint64 {
	s.mixedCursorMu.RLock()
	if counter := s.mixedCursors[cursorKey]; counter != nil {
		s.mixedCursorMu.RUnlock()
		return counter
	}
	s.mixedCursorMu.RUnlock()

	counter := &atomic.Uint64{}
	s.mixedCursorMu.Lock()
	defer s.mixedCursorMu.Unlock()
	if existing := s.mixedCursors[cursorKey]; existing != nil {
		return existing
	}
	if s.mixedCursors == nil {
		s.mixedCursors = make(map[mixedCursorKey]*atomic.Uint64)
	}
	if len(s.mixedCursors) >= schedulerMaxMixedCursors {
		s.mixedCursors = make(map[mixedCursorKey]*atomic.Uint64)
	}
	s.mixedCursors[cursorKey] = counter
	return counter
}

func (s *authScheduler) ensureLargeMixedCursor(cursorKey string) *atomic.Uint64 {
	s.mixedCursorMu.RLock()
	if counter := s.largeCursors[cursorKey]; counter != nil {
		s.mixedCursorMu.RUnlock()
		return counter
	}
	s.mixedCursorMu.RUnlock()

	counter := &atomic.Uint64{}
	s.mixedCursorMu.Lock()
	defer s.mixedCursorMu.Unlock()
	if existing := s.largeCursors[cursorKey]; existing != nil {
		return existing
	}
	if s.largeCursors == nil {
		s.largeCursors = make(map[string]*atomic.Uint64)
	}
	if len(s.largeCursors) >= schedulerMaxMixedCursors {
		s.largeCursors = make(map[string]*atomic.Uint64)
	}
	s.largeCursors[cursorKey] = counter
	return counter
}

func makeMixedCursorKey(providers []string, modelKey string) (mixedCursorKey, bool) {
	if len(providers) == 0 || len(providers) > len((mixedCursorKey{}).providers) {
		return mixedCursorKey{}, false
	}
	key := mixedCursorKey{
		modelKey:      modelKey,
		providerCount: uint8(len(providers)),
	}
	copy(key.providers[:], providers)
	return key, true
}

// mixedUnavailableError synthesizes the mixed-provider cooldown or unavailable error.
func (s *authScheduler) mixedUnavailableError(providers []string, model string, filter authFilter) error {
	now := time.Now()
	modelKey := canonicalModelKey(model)
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	for _, providerKey := range providers {
		providerState := s.providers[providerKey]
		if providerState == nil {
			continue
		}
		shard := providerState.ensureModel(modelKey, now)
		if shard == nil {
			continue
		}
		shard.promoteExpired(now)
		localTotal, localCooldownCount, localEarliest := shard.availabilitySummary(filter)
		total += localTotal
		cooldownCount += localCooldownCount
		if !localEarliest.IsZero() && (earliest.IsZero() || localEarliest.Before(earliest)) {
			earliest = localEarliest
		}
	}
	if total == 0 {
		return authNotFoundErrorForFilter(filter)
	}
	if cooldownCount == total && !earliest.IsZero() {
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return newModelCooldownError(model, "", resetIn)
	}
	return authUnavailableErrorForFilter(filter)
}

// normalizeProviderKeys lowercases, trims, and de-duplicates provider keys while preserving order.
func normalizeProviderKeys(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	normalized := true
	var seen map[string]struct{}
	for idx, provider := range providers {
		providerKey := strings.ToLower(strings.TrimSpace(provider))
		if providerKey == "" || providerKey != provider {
			normalized = false
			break
		}
		if len(providers) > 4 {
			if seen == nil {
				seen = make(map[string]struct{}, len(providers))
			}
			if _, exists := seen[providerKey]; exists {
				normalized = false
				break
			}
			seen[providerKey] = struct{}{}
		} else {
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
	}
	if normalized {
		return providers
	}
	seen = make(map[string]struct{}, len(providers))
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		providerKey := strings.ToLower(strings.TrimSpace(provider))
		if providerKey == "" {
			continue
		}
		if _, ok := seen[providerKey]; ok {
			continue
		}
		seen[providerKey] = struct{}{}
		out = append(out, providerKey)
	}
	return out
}

// containsProvider reports whether provider is present in the normalized provider list.
func containsProvider(providers []string, provider string) bool {
	for _, candidate := range providers {
		if candidate == provider {
			return true
		}
	}
	return false
}

// upsertAuthLocked updates one auth in-place while the scheduler mutex is held.
func (s *authScheduler) upsertAuthLocked(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if authID == "" || providerKey == "" || auth.IsDisabled() {
		s.removeAuthLocked(authID)
		return
	}
	if previousProvider := s.authProviders[authID]; previousProvider != "" && previousProvider != providerKey {
		if previousState := s.providers[previousProvider]; previousState != nil {
			previousState.removeAuthLocked(authID)
		}
	}
	meta := buildScheduledAuthMeta(auth)
	s.authProviders[authID] = providerKey
	s.ensureProviderLocked(providerKey).upsertAuthLocked(meta, now)
}

// removeAuthLocked removes one auth from the scheduler while the scheduler mutex is held.
func (s *authScheduler) removeAuthLocked(authID string) {
	if authID == "" {
		return
	}
	if providerKey := s.authProviders[authID]; providerKey != "" {
		if providerState := s.providers[providerKey]; providerState != nil {
			providerState.removeAuthLocked(authID)
		}
		delete(s.authProviders, authID)
	}
}

// ensureProviderLocked returns the provider scheduler for providerKey, creating it when needed.
func (s *authScheduler) ensureProviderLocked(providerKey string) *providerScheduler {
	if s.providers == nil {
		s.providers = make(map[string]*providerScheduler)
	}
	providerState := s.providers[providerKey]
	if providerState == nil {
		providerState = &providerScheduler{
			providerKey: providerKey,
			auths:       make(map[string]*scheduledAuthMeta),
			modelShards: make(map[string]*modelScheduler),
		}
		s.providers[providerKey] = providerState
	}
	return providerState
}

// buildScheduledAuthMeta extracts the scheduling metadata needed for shard bookkeeping.
func buildScheduledAuthMeta(auth *Auth) *scheduledAuthMeta {
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	return &scheduledAuthMeta{
		auth:              auth,
		providerKey:       providerKey,
		priority:          authPriority(auth),
		weight:            authWeight(auth),
		websocketEnabled:  authWebsocketsEnabled(auth),
		supportedModelSet: supportedModelSetForAuth(auth.ID),
	}
}

// supportedModelSetForAuth snapshots the registry models currently registered for an auth.
func supportedModelSetForAuth(authID string) map[string]struct{} {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	modelInfos := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(modelInfos) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(modelInfos)*2)
	for _, modelInfo := range modelInfos {
		if modelInfo == nil {
			continue
		}
		for _, modelName := range []string{modelInfo.ID, modelInfo.Name} {
			modelKey := canonicalModelKey(modelName)
			if modelKey == "" {
				continue
			}
			set[modelKey] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// upsertAuthLocked updates every existing model shard that can reference the auth metadata.
func (p *providerScheduler) upsertAuthLocked(meta *scheduledAuthMeta, now time.Time) {
	if p == nil || meta == nil || meta.auth == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.auths == nil {
		p.auths = make(map[string]*scheduledAuthMeta)
	}
	if p.modelShards == nil {
		p.modelShards = make(map[string]*modelScheduler)
	}
	p.evictExpiredModelShardsLocked(now)
	p.auths[meta.auth.ID] = meta
	for modelKey, shard := range p.modelShards {
		if shard == nil {
			continue
		}
		if !meta.supportsModel(modelKey) {
			shard.removeEntry(meta.auth.ID)
			continue
		}
		shard.upsertEntry(meta, now)
	}
}

// removeAuthLocked removes an auth from all model shards owned by the provider scheduler.
func (p *providerScheduler) removeAuthLocked(authID string) {
	if p == nil || authID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.auths, authID)
	for _, shard := range p.modelShards {
		if shard != nil {
			shard.removeEntry(authID)
		}
	}
}

// ensureModel returns the shard for modelKey, building it lazily from provider auths.
func (p *providerScheduler) ensureModel(modelKey string, now time.Time) *modelScheduler {
	if p == nil {
		return nil
	}
	modelKey = canonicalModelKey(modelKey)
	p.mu.RLock()
	shard, ok := p.modelShards[modelKey]
	p.mu.RUnlock()
	if ok && shard != nil {
		shard.touch(now)
		return shard
	}

	p.mu.Lock()
	if p.modelShards == nil {
		p.modelShards = make(map[string]*modelScheduler)
	}
	if shard, ok = p.modelShards[modelKey]; ok && shard != nil {
		shard.touch(now)
		p.mu.Unlock()
		return shard
	}
	p.evictExpiredModelShardsLocked(now)
	p.evictModelShardsToCapacityLocked(schedulerMaxModelShardsPerProvider - 1)
	shard = &modelScheduler{
		modelKey:        modelKey,
		entries:         make(map[string]*scheduledAuth),
		readyByPriority: make(map[int]*readyBucket),
	}
	for _, meta := range p.auths {
		if meta == nil || !meta.supportsModel(modelKey) {
			continue
		}
		shard.upsertEntryLocked(meta, now)
	}
	shard.touch(now)
	p.modelShards[modelKey] = shard
	p.mu.Unlock()
	return shard
}

// touch records recent use without taking the provider-wide shard lock. This
// keeps the scheduler read path inexpensive while giving eviction an
// approximate LRU ordering.
func (s *modelScheduler) touch(now time.Time) {
	if s == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.lastUsedUnixNano.Store(now.UnixNano())
}

func (s *modelScheduler) lastUsedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	lastUsed := s.lastUsedUnixNano.Load()
	if lastUsed <= 0 {
		return time.Time{}
	}
	return time.Unix(0, lastUsed)
}

func (p *providerScheduler) evictExpiredModelShardsLocked(now time.Time) {
	if p == nil || len(p.modelShards) == 0 {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	deadline := now.Add(-schedulerModelShardTTL)
	for modelKey, shard := range p.modelShards {
		if shard == nil || shard.lastUsedAt().Before(deadline) {
			delete(p.modelShards, modelKey)
		}
	}
}

func (p *providerScheduler) evictModelShardsToCapacityLocked(capacity int) {
	if p == nil {
		return
	}
	if capacity < 0 {
		capacity = 0
	}
	for len(p.modelShards) > capacity {
		var (
			oldestKey  string
			oldestUsed time.Time
		)
		for modelKey, shard := range p.modelShards {
			lastUsed := shard.lastUsedAt()
			if oldestKey == "" || lastUsed.Before(oldestUsed) || (lastUsed.Equal(oldestUsed) && modelKey < oldestKey) {
				oldestKey = modelKey
				oldestUsed = lastUsed
			}
		}
		if oldestKey == "" {
			return
		}
		delete(p.modelShards, oldestKey)
	}
}

// supportsModel reports whether the auth metadata currently supports modelKey.
func (m *scheduledAuthMeta) supportsModel(modelKey string) bool {
	modelKey = canonicalModelKey(modelKey)
	if modelKey == "" {
		return true
	}
	if len(m.supportedModelSet) == 0 {
		return false
	}
	_, ok := m.supportedModelSet[modelKey]
	return ok
}

func (m *modelScheduler) upsertEntry(meta *scheduledAuthMeta, now time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.upsertEntryLocked(meta, now)
	m.mu.Unlock()
}

// upsertEntryLocked updates or inserts one auth entry and rebuilds indexes when ordering changes.
func (m *modelScheduler) upsertEntryLocked(meta *scheduledAuthMeta, now time.Time) {
	if m == nil || meta == nil || meta.auth == nil {
		return
	}
	entry, ok := m.entries[meta.auth.ID]
	if !ok || entry == nil {
		entry = &scheduledAuth{}
		m.entries[meta.auth.ID] = entry
	}
	previousState := entry.state
	previousNextRetryAt := entry.nextRetryAt
	previousPriority := 0
	previousWebsocketEnabled := false
	if entry.meta != nil {
		previousPriority = entry.meta.priority
		previousWebsocketEnabled = entry.meta.websocketEnabled
	}

	entry.meta = meta
	entry.auth = meta.auth
	entry.nextRetryAt = time.Time{}
	blocked, reason, next := isAuthBlockedForModel(meta.auth, m.modelKey, now)
	switch {
	case !blocked:
		entry.state = scheduledStateReady
	case reason == blockReasonCooldown:
		entry.state = scheduledStateCooldown
		entry.nextRetryAt = next
	case reason == blockReasonDisabled:
		entry.state = scheduledStateDisabled
	default:
		entry.state = scheduledStateBlocked
		entry.nextRetryAt = next
	}

	if ok && previousState == entry.state && previousNextRetryAt.Equal(entry.nextRetryAt) && previousPriority == meta.priority && previousWebsocketEnabled == meta.websocketEnabled {
		return
	}
	m.rebuildIndexesLocked()
}

func (m *modelScheduler) removeEntry(authID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.removeEntryLocked(authID)
	m.mu.Unlock()
}

// removeEntryLocked deletes one auth entry and rebuilds the shard indexes if needed.
func (m *modelScheduler) removeEntryLocked(authID string) {
	if m == nil || authID == "" {
		return
	}
	if _, ok := m.entries[authID]; !ok {
		return
	}
	delete(m.entries, authID)
	m.rebuildIndexesLocked()
}

func (m *modelScheduler) promoteExpired(now time.Time) {
	if m == nil {
		return
	}
	m.mu.RLock()
	nextBlockedRetry := m.nextBlockedRetry
	m.mu.RUnlock()
	if nextBlockedRetry.IsZero() || nextBlockedRetry.After(now) {
		return
	}
	m.mu.Lock()
	m.promoteExpiredLocked(now)
	m.mu.Unlock()
}

// promoteExpiredLocked reevaluates blocked auths whose retry time has elapsed.
func (m *modelScheduler) promoteExpiredLocked(now time.Time) {
	if m == nil || len(m.blocked) == 0 {
		m.nextBlockedRetry = time.Time{}
		return
	}
	if m.nextBlockedRetry.IsZero() || m.nextBlockedRetry.After(now) {
		return
	}
	changed := false
	for _, entry := range m.blocked {
		if entry == nil || entry.auth == nil {
			continue
		}
		if entry.nextRetryAt.IsZero() || entry.nextRetryAt.After(now) {
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(entry.auth, m.modelKey, now)
		switch {
		case !blocked:
			entry.state = scheduledStateReady
			entry.nextRetryAt = time.Time{}
		case reason == blockReasonCooldown:
			entry.state = scheduledStateCooldown
			entry.nextRetryAt = next
		case reason == blockReasonDisabled:
			entry.state = scheduledStateDisabled
			entry.nextRetryAt = time.Time{}
		default:
			entry.state = scheduledStateBlocked
			entry.nextRetryAt = next
		}
		changed = true
	}
	if changed {
		m.rebuildIndexesLocked()
	}
}

func (m *modelScheduler) pickReady(preferWebsocket bool, strategy schedulerStrategy, filter authFilter, load authLoadFunc) *Auth {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	picked := m.pickReadyLocked(preferWebsocket, strategy, filter, load)
	m.mu.Unlock()
	return picked
}

func (m *modelScheduler) pickReadyStable(preferWebsocket bool, filter authFilter, affinityKey string) *Auth {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.promoteExpiredLocked(time.Now())
	priorityReady, okPriority := m.highestReadyPriorityLocked(preferWebsocket, filter)
	if !okPriority {
		m.mu.Unlock()
		return nil
	}
	picked := m.pickReadyStableAtPriorityLocked(preferWebsocket, priorityReady, filter, affinityKey)
	m.mu.Unlock()
	return picked
}

// pickReadyLocked selects the next ready auth from the highest available priority bucket.
func (m *modelScheduler) pickReadyLocked(preferWebsocket bool, strategy schedulerStrategy, filter authFilter, load authLoadFunc) *Auth {
	if m == nil {
		return nil
	}
	m.promoteExpiredLocked(time.Now())
	priorityReady, okPriority := m.highestReadyPriorityLocked(preferWebsocket, filter)
	if !okPriority {
		return nil
	}
	return m.pickReadyAtPriorityLocked(preferWebsocket, priorityReady, strategy, filter, load)
}

func (m *modelScheduler) highestReadyPriority(preferWebsocket bool, filter authFilter) (int, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.RLock()
	priorityReady, okPriority := m.highestReadyPriorityLocked(preferWebsocket, filter)
	m.mu.RUnlock()
	return priorityReady, okPriority
}

func (m *modelScheduler) highestReadyPriorityAndCount(preferWebsocket bool, filter authFilter) (int, int, bool) {
	if m == nil {
		return 0, 0, false
	}
	m.mu.RLock()
	priorityReady, okPriority := m.highestReadyPriorityLocked(preferWebsocket, filter)
	if !okPriority {
		m.mu.RUnlock()
		return 0, 0, false
	}
	readyCount := m.matchingReadyCountAtPriorityLocked(preferWebsocket, priorityReady, filter)
	m.mu.RUnlock()
	return priorityReady, readyCount, true
}

func (m *modelScheduler) highestReadyPriorityAndLoadStats(preferWebsocket bool, filter authFilter, load authLoadFunc) (int, int, int64, int, bool) {
	if m == nil {
		return 0, 0, 0, 0, false
	}
	m.mu.RLock()
	priorityReady, okPriority := m.highestReadyPriorityLocked(preferWebsocket, filter)
	if !okPriority {
		m.mu.RUnlock()
		return 0, 0, 0, 0, false
	}
	readyCount, minLoad, minLoadCount := m.loadStatsAtPriorityLocked(preferWebsocket, priorityReady, filter, load)
	m.mu.RUnlock()
	return priorityReady, readyCount, minLoad, minLoadCount, true
}

// highestReadyPriorityLocked returns the highest priority bucket that still has a matching ready auth.
// The caller must ensure expired entries are already promoted when needed.
func (m *modelScheduler) highestReadyPriorityLocked(preferWebsocket bool, filter authFilter) (int, bool) {
	if m == nil {
		return 0, false
	}
	if filter.empty() {
		if preferWebsocket {
			for _, priority := range m.priorityOrder {
				bucket := m.readyByPriority[priority]
				if bucket != nil && len(bucket.ws.flat) > 0 {
					return priority, true
				}
			}
		}
		for _, priority := range m.priorityOrder {
			bucket := m.readyByPriority[priority]
			if bucket != nil && len(bucket.all.flat) > 0 {
				return priority, true
			}
		}
		return 0, false
	}
	if preferWebsocket {
		// When downstream is websocket and Codex supports websocket transport, prefer websocket-enabled
		// credentials even if they are in a lower priority tier than HTTP-only credentials.
		for _, priority := range m.priorityOrder {
			bucket := m.readyByPriority[priority]
			if bucket == nil {
				continue
			}
			if bucket.ws.pickFirst(filter) != nil {
				return priority, true
			}
		}
	}
	for _, priority := range m.priorityOrder {
		bucket := m.readyByPriority[priority]
		if bucket == nil {
			continue
		}
		if bucket.all.pickFirst(filter) != nil {
			return priority, true
		}
	}
	return 0, false
}

func (m *modelScheduler) pickReadyAtPriority(preferWebsocket bool, priority int, strategy schedulerStrategy, filter authFilter, load authLoadFunc) *Auth {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	picked := m.pickReadyAtPriorityLocked(preferWebsocket, priority, strategy, filter, load)
	m.mu.Unlock()
	return picked
}

func (m *modelScheduler) pickReadyStableAtPriority(preferWebsocket bool, priority int, filter authFilter, affinityKey string) *Auth {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	picked := m.pickReadyStableAtPriorityLocked(preferWebsocket, priority, filter, affinityKey)
	m.mu.RUnlock()
	return picked
}

func (m *modelScheduler) pickReadyStableAtPriorityLocked(preferWebsocket bool, priority int, filter authFilter, affinityKey string) *Auth {
	if m == nil {
		return nil
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return nil
	}
	view := &bucket.all
	if preferWebsocket {
		if filter.empty() {
			if len(bucket.ws.flat) > 0 {
				view = &bucket.ws
			}
		} else if bucket.ws.pickFirst(filter) != nil {
			view = &bucket.ws
		}
	}
	picked := view.pickStable(filter, affinityKey)
	if picked == nil || picked.auth == nil {
		return nil
	}
	return picked.auth
}

// pickReadyAtPriorityLocked selects the next ready auth from a specific priority bucket.
// The caller must ensure expired entries are already promoted when needed.
func (m *modelScheduler) pickReadyAtPriorityLocked(preferWebsocket bool, priority int, strategy schedulerStrategy, filter authFilter, load authLoadFunc) *Auth {
	if m == nil {
		return nil
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return nil
	}
	view := &bucket.all
	if preferWebsocket {
		if filter.empty() {
			if len(bucket.ws.flat) > 0 {
				view = &bucket.ws
			}
		} else if bucket.ws.pickFirst(filter) != nil {
			view = &bucket.ws
		}
	}
	var picked *scheduledAuth
	if strategy == schedulerStrategyWeightedRoundRobin {
		picked = view.pickWeighted(filter, load)
	} else if strategy == schedulerStrategyFillFirst || load == nil {
		if filter.empty() {
			if strategy == schedulerStrategyFillFirst {
				picked = view.pickFirstNoFilter()
			} else {
				picked = view.pickRoundRobinNoFilter()
			}
		} else if strategy == schedulerStrategyFillFirst {
			picked = view.pickFirst(filter)
		} else {
			picked = view.pickRoundRobin(filter)
		}
	} else {
		picked = view.pickRoundRobinWithLoad(filter, load)
	}
	if picked == nil || picked.auth == nil {
		return nil
	}
	return picked.auth
}

func (m *modelScheduler) readyCountAtPriority(preferWebsocket bool, priority int) int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	readyCount := m.readyCountAtPriorityLocked(preferWebsocket, priority)
	m.mu.RUnlock()
	return readyCount
}

func (m *modelScheduler) readyCountAtPriorityLocked(preferWebsocket bool, priority int) int {
	if m == nil {
		return 0
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return 0
	}
	if preferWebsocket && len(bucket.ws.flat) > 0 {
		return len(bucket.ws.flat)
	}
	return len(bucket.all.flat)
}

func (m *modelScheduler) matchingReadyCountAtPriorityLocked(preferWebsocket bool, priority int, filter authFilter) int {
	if m == nil {
		return 0
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return 0
	}
	view := &bucket.all
	if preferWebsocket && len(bucket.ws.flat) > 0 {
		view = &bucket.ws
	}
	if filter.empty() {
		return len(view.flat)
	}
	count := 0
	for _, entry := range view.flat {
		if filter.matches(entry) {
			count++
		}
	}
	return count
}

func (m *modelScheduler) weightedEntriesAtPriority(preferWebsocket bool, priority int, filter authFilter, load authLoadFunc, targetLoad int64) []*scheduledAuth {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		m.mu.RUnlock()
		return nil
	}
	view := &bucket.all
	if preferWebsocket {
		if filter.empty() {
			if len(bucket.ws.flat) > 0 {
				view = &bucket.ws
			}
		} else if bucket.ws.pickFirst(filter) != nil {
			view = &bucket.ws
		}
	}
	entries := append([]*scheduledAuth(nil), view.entriesAtLoad(filter, load, targetLoad)...)
	m.mu.RUnlock()
	return entries
}

func (m *modelScheduler) loadStatsAtPriorityLocked(preferWebsocket bool, priority int, filter authFilter, load authLoadFunc) (int, int64, int) {
	if m == nil {
		return 0, 0, 0
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return 0, 0, 0
	}
	view := &bucket.all
	if preferWebsocket && len(bucket.ws.flat) > 0 {
		view = &bucket.ws
	}
	if len(view.flat) == 0 {
		return 0, 0, 0
	}
	if load == nil {
		if filter.empty() {
			return len(view.flat), 0, len(view.flat)
		}
		count := 0
		for _, entry := range view.flat {
			if filter.matches(entry) {
				count++
			}
		}
		return count, 0, count
	}
	readyCount := 0
	var minLoad int64
	minLoadCount := 0
	found := false
	for _, entry := range view.flat {
		if !filter.matches(entry) || entry == nil || entry.auth == nil {
			continue
		}
		readyCount++
		entryLoad := loadAuthCount(load, entry.auth.ID)
		if !found || entryLoad < minLoad {
			minLoad = entryLoad
			minLoadCount = 1
			found = true
			continue
		}
		if entryLoad == minLoad {
			minLoadCount++
		}
	}
	if !found {
		return 0, 0, 0
	}
	return readyCount, minLoad, minLoadCount
}

func loadAuthCount(load authLoadFunc, authID string) int64 {
	if load == nil {
		return 0
	}
	if count := load(strings.TrimSpace(authID)); count > 0 {
		return count
	}
	return 0
}

func (m *modelScheduler) unavailableError(provider, model string, filter authFilter) error {
	if m == nil {
		return &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	m.mu.RLock()
	errUnavailable := m.unavailableErrorLocked(provider, model, filter)
	m.mu.RUnlock()
	return errUnavailable
}

func (m *modelScheduler) pickReadyAtPriorityOffset(preferWebsocket bool, priority, offset int, filter authFilter, load authLoadFunc) *Auth {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	picked := m.pickReadyAtPriorityOffsetLocked(preferWebsocket, priority, offset, filter, load)
	m.mu.RUnlock()
	return picked
}

func (m *modelScheduler) pickReadyAtPriorityOffsetLocked(preferWebsocket bool, priority, offset int, filter authFilter, load authLoadFunc) *Auth {
	if m == nil {
		return nil
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return nil
	}
	view := &bucket.all
	if preferWebsocket {
		if filter.empty() {
			if len(bucket.ws.flat) > 0 {
				view = &bucket.ws
			}
		} else if bucket.ws.pickFirst(filter) != nil {
			view = &bucket.ws
		}
	}
	var picked *scheduledAuth
	if load == nil {
		if filter.empty() {
			picked = view.pickRoundRobinAtNoFilter(offset)
		} else {
			picked = view.pickRoundRobinAt(offset, filter)
		}
	} else {
		picked = view.pickRoundRobinAtWithLoad(offset, filter, load)
	}
	if picked == nil || picked.auth == nil {
		return nil
	}
	return picked.auth
}

// unavailableErrorLocked returns the correct unavailable or cooldown error for the shard.
func (m *modelScheduler) unavailableErrorLocked(provider, model string, filter authFilter) error {
	now := time.Now()
	total, cooldownCount, earliest := m.availabilitySummaryLocked(filter)
	if total == 0 {
		return authNotFoundErrorForFilter(filter)
	}
	if cooldownCount == total && !earliest.IsZero() {
		providerForError := provider
		if providerForError == "mixed" {
			providerForError = ""
		}
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return newModelCooldownError(model, providerForError, resetIn)
	}
	return authUnavailableErrorForFilter(filter)
}

func (m *modelScheduler) availabilitySummary(filter authFilter) (int, int, time.Time) {
	if m == nil {
		return 0, 0, time.Time{}
	}
	m.mu.RLock()
	total, cooldownCount, earliest := m.availabilitySummaryLocked(filter)
	m.mu.RUnlock()
	return total, cooldownCount, earliest
}

// availabilitySummaryLocked summarizes total candidates, cooldown count, and earliest retry time.
func (m *modelScheduler) availabilitySummaryLocked(filter authFilter) (int, int, time.Time) {
	if m == nil {
		return 0, 0, time.Time{}
	}
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	for _, entry := range m.entries {
		if !filter.matches(entry) {
			continue
		}
		total++
		if entry == nil || entry.auth == nil {
			continue
		}
		if entry.state != scheduledStateCooldown {
			continue
		}
		cooldownCount++
		if !entry.nextRetryAt.IsZero() && (earliest.IsZero() || entry.nextRetryAt.Before(earliest)) {
			earliest = entry.nextRetryAt
		}
	}
	return total, cooldownCount, earliest
}

// rebuildIndexesLocked reconstructs ready and blocked views from the current entry map.
func (m *modelScheduler) rebuildIndexesLocked() {
	cursorStates := make(map[int]readyBucketCursorState, len(m.readyByPriority))
	for priority, bucket := range m.readyByPriority {
		if bucket == nil {
			continue
		}
		cursorStates[priority] = readyBucketCursorState{
			all: snapshotReadyViewCursors(bucket.all),
			ws:  snapshotReadyViewCursors(bucket.ws),
		}
	}

	m.readyByPriority = make(map[int]*readyBucket)
	m.priorityOrder = m.priorityOrder[:0]
	m.blocked = m.blocked[:0]
	m.nextBlockedRetry = time.Time{}
	priorityBuckets := make(map[int][]*scheduledAuth)
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil {
			continue
		}
		switch entry.state {
		case scheduledStateReady:
			priority := entry.meta.priority
			priorityBuckets[priority] = append(priorityBuckets[priority], entry)
		case scheduledStateCooldown, scheduledStateBlocked:
			m.blocked = append(m.blocked, entry)
		}
	}
	for priority, entries := range priorityBuckets {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].auth.ID < entries[j].auth.ID
		})
		bucket := buildReadyBucket(entries)
		if cursorState, ok := cursorStates[priority]; ok && bucket != nil {
			restoreReadyViewCursors(&bucket.all, cursorState.all)
			restoreReadyViewCursors(&bucket.ws, cursorState.ws)
		}
		m.readyByPriority[priority] = bucket
		m.priorityOrder = append(m.priorityOrder, priority)
	}
	sort.Slice(m.priorityOrder, func(i, j int) bool {
		return m.priorityOrder[i] > m.priorityOrder[j]
	})
	sort.Slice(m.blocked, func(i, j int) bool {
		left := m.blocked[i]
		right := m.blocked[j]
		if left == nil || right == nil {
			return left != nil
		}
		if left.nextRetryAt.Equal(right.nextRetryAt) {
			return left.auth.ID < right.auth.ID
		}
		if left.nextRetryAt.IsZero() {
			return false
		}
		if right.nextRetryAt.IsZero() {
			return true
		}
		return left.nextRetryAt.Before(right.nextRetryAt)
	})
	for _, entry := range m.blocked {
		if entry == nil || entry.nextRetryAt.IsZero() {
			continue
		}
		m.nextBlockedRetry = entry.nextRetryAt
		break
	}
}

// buildReadyBucket prepares the general and websocket-only ready views for one priority bucket.
func buildReadyBucket(entries []*scheduledAuth) *readyBucket {
	bucket := &readyBucket{}
	bucket.all = buildReadyView(entries)
	wsEntries := make([]*scheduledAuth, 0, len(entries))
	for _, entry := range entries {
		if entry != nil && entry.meta != nil && entry.meta.websocketEnabled {
			wsEntries = append(wsEntries, entry)
		}
	}
	bucket.ws = buildReadyView(wsEntries)
	return bucket
}

func buildReadyView(entries []*scheduledAuth) readyView {
	return readyView{flat: append([]*scheduledAuth(nil), entries...)}
}

// pickFirst returns the first ready entry that satisfies predicate without advancing cursors.
func (v *readyView) pickFirst(filter authFilter) *scheduledAuth {
	for _, entry := range v.flat {
		if filter.matches(entry) {
			return entry
		}
	}
	return nil
}

func (v *readyView) pickFirstNoFilter() *scheduledAuth {
	if len(v.flat) == 0 {
		return nil
	}
	return v.flat[0]
}

func (v *readyView) pickStable(filter authFilter, affinityKey string) *scheduledAuth {
	if len(v.flat) == 0 {
		return nil
	}
	var picked *scheduledAuth
	bestScore := uint64(0)
	for _, entry := range v.flat {
		if !filter.matches(entry) || entry.auth == nil {
			continue
		}
		score := stableAffinityScore(affinityKey, entry.auth.ID)
		if picked == nil || score > bestScore || (score == bestScore && entry.auth.ID < picked.auth.ID) {
			picked = entry
			bestScore = score
		}
	}
	return picked
}

func stableAffinityScore(affinityKey, authID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(affinityKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(authID))
	return h.Sum64()
}

func preferWebsocketForProvider(preferUpstreamWebsocket bool, provider string) bool {
	if !preferUpstreamWebsocket {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(provider), "codex")
}

func (v *readyView) pickRoundRobin(filter authFilter) *scheduledAuth {
	return v.pickRoundRobinWithLoad(filter, nil)
}

// pickWeighted selects proportionally among the least-loaded candidates that
// are not disfavored by the Codex quota scheduler. Weight never bypasses those
// safety preferences; it only decides the distribution within the eligible set.
func (v *readyView) pickWeighted(filter authFilter, load authLoadFunc) *scheduledAuth {
	if v == nil || len(v.flat) == 0 {
		return nil
	}
	entries := v.weightedEligibleEntries(filter, load)
	v.weightedState.prepare(scheduledWeightVector(entries))
	return pickSmoothWeightedScheduled(entries, v.weightedState.current)
}

func (v *readyView) weightedEligibleEntries(filter authFilter, load authLoadFunc) []*scheduledAuth {
	if v == nil {
		return nil
	}
	var minLoad int64
	hasCandidate := false
	for _, entry := range v.flat {
		if !filter.matches(entry) {
			continue
		}
		entryLoad := loadAuthCount(load, entry.auth.ID)
		if !hasCandidate || entryLoad < minLoad {
			minLoad = entryLoad
			hasCandidate = true
		}
	}
	if !hasCandidate {
		return nil
	}
	return filterWeightedScheduledByQuota(v.entriesAtLoad(filter, load, minLoad))
}

func (v *readyView) entriesAtLoad(filter authFilter, load authLoadFunc, targetLoad int64) []*scheduledAuth {
	if v == nil {
		return nil
	}
	entries := make([]*scheduledAuth, 0, len(v.flat))
	for _, entry := range v.flat {
		if !filter.matches(entry) || loadAuthCount(load, entry.auth.ID) != targetLoad {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

func filterWeightedScheduledByQuota(entries []*scheduledAuth) []*scheduledAuth {
	if len(entries) < 2 {
		return entries
	}
	now := time.Now()
	best := quotaSchedulingScore(entries[0].auth, now)
	for _, entry := range entries[1:] {
		candidate := quotaSchedulingScore(entry.auth, now)
		if quotaSchedulingScoreBetter(candidate, best) {
			best = candidate
		}
	}
	eligible := make([]*scheduledAuth, 0, len(entries))
	for _, entry := range entries {
		if !quotaSchedulingScoreBetter(best, quotaSchedulingScore(entry.auth, now)) {
			eligible = append(eligible, entry)
		}
	}
	return eligible
}

func scheduledWeightVector(entries []*scheduledAuth) map[string]int64 {
	weights := make(map[string]int64, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || entry.meta == nil || entry.meta.weight <= 0 {
			continue
		}
		weights[entry.auth.ID] = entry.meta.weight
	}
	return weights
}

func pickSmoothWeightedScheduled(entries []*scheduledAuth, current map[string]int64) *scheduledAuth {
	active := make(map[string]struct{}, len(entries))
	var picked *scheduledAuth
	var pickedCurrent int64
	var totalWeight int64
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || entry.meta == nil || entry.meta.weight <= 0 {
			continue
		}
		active[entry.auth.ID] = struct{}{}
		current[entry.auth.ID] = saturatingAddInt64(current[entry.auth.ID], entry.meta.weight)
		totalWeight = saturatingAddInt64(totalWeight, entry.meta.weight)
		if picked == nil || current[entry.auth.ID] > pickedCurrent {
			picked = entry
			pickedCurrent = current[entry.auth.ID]
		}
	}
	for authID := range current {
		if _, ok := active[authID]; !ok {
			delete(current, authID)
		}
	}
	if picked == nil {
		return nil
	}
	current[picked.auth.ID] = saturatingAddInt64(current[picked.auth.ID], -totalWeight)
	return picked
}

func (v *readyView) pickRoundRobinWithLoad(filter authFilter, load authLoadFunc) *scheduledAuth {
	if len(v.flat) == 0 {
		return nil
	}
	start := 0
	if len(v.flat) > 0 {
		start = v.cursor % len(v.flat)
	}
	var (
		picked      *scheduledAuth
		pickedIndex = -1
		bestLoad    int64
		bestQuota   codexQuotaSchedulingScore
		found       bool
	)
	now := time.Now()
	for offset := 0; offset < len(v.flat); offset++ {
		index := (start + offset) % len(v.flat)
		entry := v.flat[index]
		if !filter.matches(entry) || entry == nil || entry.auth == nil {
			continue
		}
		entryLoad := loadAuthCount(load, entry.auth.ID)
		entryQuota := quotaSchedulingScore(entry.auth, now)
		if !found || entryLoad < bestLoad || (entryLoad == bestLoad && quotaSchedulingScoreBetter(entryQuota, bestQuota)) {
			picked = entry
			pickedIndex = index
			bestLoad = entryLoad
			bestQuota = entryQuota
			found = true
		}
	}
	if picked != nil {
		v.cursor = pickedIndex + 1
	}
	return picked
}

func (v *readyView) pickRoundRobinNoFilter() *scheduledAuth {
	if len(v.flat) == 0 {
		return nil
	}
	return v.pickRoundRobinWithLoad(authFilter{}, nil)
}

func (v *readyView) pickRoundRobinAt(offset int, filter authFilter) *scheduledAuth {
	return v.pickRoundRobinAtWithLoad(offset, filter, nil)
}

func (v *readyView) pickRoundRobinAtWithLoad(offset int, filter authFilter, load authLoadFunc) *scheduledAuth {
	if len(v.flat) == 0 {
		return nil
	}
	start := 0
	if len(v.flat) > 0 {
		start = offset % len(v.flat)
		if start < 0 {
			start += len(v.flat)
		}
	}
	var (
		picked    *scheduledAuth
		bestLoad  int64
		bestQuota codexQuotaSchedulingScore
		found     bool
	)
	now := time.Now()
	for step := 0; step < len(v.flat); step++ {
		index := (start + step) % len(v.flat)
		entry := v.flat[index]
		if !filter.matches(entry) || entry == nil || entry.auth == nil {
			continue
		}
		entryLoad := loadAuthCount(load, entry.auth.ID)
		entryQuota := quotaSchedulingScore(entry.auth, now)
		if !found || entryLoad < bestLoad || (entryLoad == bestLoad && quotaSchedulingScoreBetter(entryQuota, bestQuota)) {
			picked = entry
			bestLoad = entryLoad
			bestQuota = entryQuota
			found = true
		}
	}
	return picked
}

// quotaSchedulingScore ranks Codex credentials by how urgently their remaining
// quota should be consumed. The minimum remaining-percent-per-hour across all
// active windows is used as the bottleneck, so a nearly exhausted weekly window
// cannot be hidden by a healthy five-hour window. Unknown or stale snapshots
// deliberately fall back to the existing round-robin order.
func quotaSchedulingScore(auth *Auth, now time.Time) codexQuotaSchedulingScore {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || len(auth.RateLimits) == 0 {
		return codexQuotaSchedulingScore{rank: codexQuotaSchedulingUnknown}
	}

	result := codexQuotaSchedulingScore{rank: codexQuotaSchedulingUnknown}
	foundWindow := false
	_, hasAccountSnapshot := auth.RateLimits["codex"]
	for limitID, snapshot := range auth.RateLimits {
		// The background usage poll publishes the account-wide limit as "codex".
		// When it is available, model-specific meters must not penalize unrelated
		// requests because the scheduler does not know whether they apply here.
		if hasAccountSnapshot && limitID != "codex" {
			continue
		}
		if snapshot.UpdatedAt.IsZero() || snapshot.UpdatedAt.After(now.Add(codexQuotaSchedulingClockSkew)) || now.Sub(snapshot.UpdatedAt) > codexQuotaSchedulingSnapshotTTL {
			continue
		}
		for _, window := range []*RateLimitWindow{snapshot.Primary, snapshot.Secondary} {
			if window == nil || window.ResetsAt == nil || window.UsedPercent < 0 || window.UsedPercent > 100 {
				continue
			}
			resetAt := time.Unix(*window.ResetsAt, 0)
			remainingDuration := resetAt.Sub(now)
			if remainingDuration <= 0 {
				continue
			}
			remainingPercent := 100 - window.UsedPercent
			if remainingPercent <= 0.01 {
				return codexQuotaSchedulingScore{rank: codexQuotaSchedulingDepleted}
			}
			urgency := remainingPercent / remainingDuration.Hours()
			if !foundWindow || urgency < result.urgency {
				result.urgency = urgency
			}
			foundWindow = true
		}
	}
	if foundWindow {
		result.rank = codexQuotaSchedulingUsable
	}
	return result
}

func quotaSchedulingScoreBetter(candidate, current codexQuotaSchedulingScore) bool {
	if candidate.rank == codexQuotaSchedulingDepleted {
		return false
	}
	if current.rank == codexQuotaSchedulingDepleted {
		return true
	}
	// If either snapshot is unknown, retain the cursor order. This prevents a
	// newly added credential or a temporarily failed poll from being starved.
	if candidate.rank == codexQuotaSchedulingUnknown || current.rank == codexQuotaSchedulingUnknown {
		return false
	}
	return candidate.urgency > current.urgency
}

func (v *readyView) pickRoundRobinAtNoFilter(offset int) *scheduledAuth {
	return v.pickRoundRobinAtWithLoad(offset, authFilter{}, nil)
}
