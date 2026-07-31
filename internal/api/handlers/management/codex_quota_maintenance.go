package management

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	codexQuotaInitialRefreshMinDelay = 2 * time.Second
	codexQuotaInitialRefreshMaxDelay = 10 * time.Second
	codexQuotaRefreshMinInterval     = 10 * time.Minute
	codexQuotaRefreshMaxInterval     = 20 * time.Minute
	codexQuotaResetFollowupMinDelay  = 5 * time.Second
	codexQuotaResetFollowupMaxDelay  = 30 * time.Second
	codexFiveHourWindow              = 5 * time.Hour
	codexFiveHourWindowTolerance     = 5 * time.Minute
	codexPersistedResetTolerance     = time.Second
	codexQuotaPrimeMaxUsedPercent    = 1.0
	codexQuotaWorkerLimit            = 4
	codexQuotaRequestTimeout         = 45 * time.Second
	codexQuotaPrimeModelID           = "gpt-5.5"
)

var errCodexQuotaPrimeModelUnavailable = errors.New("no Codex model available for quota priming")

type codexQuotaPrimeState struct {
	resetAt  time.Time
	inFlight bool
}

type codexQuotaPrimeRequest struct {
	Model           string                        `json:"model"`
	Instructions    string                        `json:"instructions"`
	Input           []codexQuotaPrimeInputMessage `json:"input"`
	MaxOutputTokens int                           `json:"max_output_tokens"`
	Store           bool                          `json:"store"`
	Stream          bool                          `json:"stream"`
}

type codexQuotaPrimeInputMessage struct {
	Type    string                        `json:"type"`
	Role    string                        `json:"role"`
	Content []codexQuotaPrimeInputContent `json:"content"`
}

type codexQuotaPrimeInputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// StartCodexQuotaMaintenance starts the background Codex quota poller. The
// first refresh uses a short jitter so quota data becomes available soon after
// startup without synchronizing requests across multiple proxy instances.
func (h *Handler) StartCodexQuotaMaintenance() {
	if h == nil {
		return
	}
	select {
	case <-h.cleanupStop:
		return
	default:
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return
	}
	h.codexQuotaOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		h.codexQuotaMu.Lock()
		if h.codexQuotaPrimed == nil {
			h.codexQuotaPrimed = make(map[string]codexQuotaPrimeState)
		}
		h.codexQuotaCancel = cancel
		h.codexQuotaMu.Unlock()
		h.codexQuotaWG.Add(1)
		go func() {
			defer h.codexQuotaWG.Done()
			h.runCodexQuotaMaintenanceLoop(ctx)
		}()
	})
}

func (h *Handler) runCodexQuotaMaintenanceLoop(ctx context.Context) {
	timer := time.NewTimer(randomCodexQuotaInitialRefreshDelay())
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			h.runCodexQuotaMaintenanceCycle(ctx)
			timer.Reset(h.nextCodexQuotaRefreshInterval(time.Now()))
		case <-ctx.Done():
			return
		}
	}
}

func randomCodexQuotaInitialRefreshDelay() time.Duration {
	return randomCodexQuotaDuration(codexQuotaInitialRefreshMinDelay, codexQuotaInitialRefreshMaxDelay)
}

func randomCodexQuotaRefreshInterval() time.Duration {
	return randomCodexQuotaDuration(codexQuotaRefreshMinInterval, codexQuotaRefreshMaxInterval)
}

func randomCodexQuotaDuration(minimum, maximum time.Duration) time.Duration {
	span := maximum - minimum
	if span <= 0 {
		return minimum
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(span)+1))
	if err != nil {
		return minimum + span/2
	}
	return minimum + time.Duration(value.Int64())
}

func (h *Handler) nextCodexQuotaRefreshInterval(now time.Time) time.Duration {
	if now.IsZero() {
		now = time.Now()
	}
	next := randomCodexQuotaRefreshInterval()
	resetJitter := randomCodexQuotaDuration(codexQuotaResetFollowupMinDelay, codexQuotaResetFollowupMaxDelay)
	if h == nil {
		return next
	}
	h.codexQuotaMu.Lock()
	defer h.codexQuotaMu.Unlock()
	for _, state := range h.codexQuotaPrimed {
		if state.inFlight || !state.resetAt.After(now) {
			continue
		}
		untilReset := state.resetAt.Sub(now) + resetJitter
		if untilReset > 0 && untilReset < next {
			next = untilReset
		}
	}
	return next
}

func (h *Handler) runCodexQuotaMaintenanceCycle(ctx context.Context) {
	if h == nil {
		return
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return
	}
	auths := manager.ListByProvider("codex")
	activeQuotaKeys := make(map[string]struct{})
	eligibleAuths := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if codexQuotaMaintenanceAuthEligible(auth) {
			activeQuotaKeys[codexQuotaPrimeKey(auth)] = struct{}{}
			eligibleAuths = append(eligibleAuths, auth)
		}
	}
	h.pruneCodexQuotaPrimeStates(activeQuotaKeys, time.Now())
	runCodexQuotaAuthWorkers(ctx, eligibleAuths, codexQuotaWorkerLimit, func(workerCtx context.Context, auth *coreauth.Auth) {
		requestCtx, cancel := context.WithTimeout(workerCtx, codexQuotaRequestTimeout)
		defer cancel()
		h.maintainCodexQuotaForAuth(requestCtx, auth, time.Now())
	})
}

func runCodexQuotaAuthWorkers(ctx context.Context, auths []*coreauth.Auth, workerLimit int, process func(context.Context, *coreauth.Auth)) {
	if len(auths) == 0 || process == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	workerCount := workerLimit
	if workerCount <= 0 {
		workerCount = codexQuotaWorkerLimit
	}
	if workerCount > len(auths) {
		workerCount = len(auths)
	}

	jobs := make(chan *coreauth.Auth)
	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case auth, ok := <-jobs:
					if !ok {
						return
					}
					if auth != nil && ctx.Err() == nil {
						process(ctx, auth)
					}
				}
			}
		}()
	}

dispatch:
	for _, auth := range auths {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- auth:
		}
	}
	close(jobs)
	wg.Wait()
}

func codexQuotaMaintenanceAuthEligible(auth *coreauth.Auth) bool {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return !auth.IsDisabled()
}

func (h *Handler) maintainCodexQuotaForAuth(ctx context.Context, auth *coreauth.Auth, now time.Time) {
	if h == nil || !codexQuotaMaintenanceAuthEligible(auth) {
		return
	}
	auth = h.refreshCodexUsageAuthIfNeeded(ctx, auth)
	quotaKey := codexQuotaPrimeKey(auth)
	payload, _, err := h.fetchCodexUsageWithCache(ctx, auth, codexUsageRequestOptions{
		force:        true,
		requireFresh: true,
		ttl:          codexUsageCacheDefaultTTL,
	})
	if err != nil {
		log.WithError(err).WithField("auth_id", auth.ID).Debug("codex quota maintenance usage refresh failed")
		return
	}
	if codexUsagePayloadIsStale(payload) {
		log.WithField("auth_id", auth.ID).Debug("codex quota maintenance skipped stale usage payload")
		return
	}
	h.updateCodexRateLimitsFromUsage(ctx, auth, payload, now)
	resetAt, shouldPrime := codexUsageNeedsFiveHourPrime(payload, now)
	if !shouldPrime {
		if fixedResetAt, ok := codexUsageFiveHourResetAt(payload, now); ok && fixedResetAt.After(now) {
			h.finishCodexQuotaPrime(quotaKey, fixedResetAt, true)
		}
		return
	}
	if codexQuotaResetAlreadyFixed(auth, resetAt) {
		h.finishCodexQuotaPrime(quotaKey, resetAt, true)
		return
	}
	if !h.reserveCodexQuotaPrime(quotaKey, resetAt, now) {
		return
	}

	if err = h.sendCodexQuotaPrimeRequest(ctx, auth); err != nil {
		h.finishCodexQuotaPrime(quotaKey, resetAt, false)
		entry := log.WithError(err).WithField("auth_id", auth.ID)
		if errors.Is(err, errCodexQuotaPrimeModelUnavailable) {
			entry.Debug("codex quota maintenance has no model available to prime 5h window")
		} else {
			entry.Warn("codex quota maintenance failed to prime 5h window")
		}
		return
	}

	confirmedResetAt := resetAt
	if refreshed, _, refreshErr := h.fetchCodexUsageWithCache(ctx, auth, codexUsageRequestOptions{
		force:        true,
		requireFresh: true,
		ttl:          codexUsageCacheDefaultTTL,
	}); refreshErr == nil && !codexUsagePayloadIsStale(refreshed) {
		refreshedAt := time.Now()
		h.updateCodexRateLimitsFromUsage(ctx, auth, refreshed, refreshedAt)
		if refreshedResetAt, ok := codexUsagePrimaryResetAt(refreshed, refreshedAt); ok {
			confirmedResetAt = refreshedResetAt
		}
	} else if refreshErr != nil {
		log.WithError(refreshErr).WithField("auth_id", auth.ID).Debug("failed to refresh codex usage after priming 5h window")
	}
	h.finishCodexQuotaPrime(quotaKey, confirmedResetAt, true)
	log.WithFields(log.Fields{"auth_id": auth.ID, "reset_at": confirmedResetAt.UTC()}).Info("codex quota maintenance primed 5h window")
}

func codexUsagePayloadIsStale(payload gin.H) bool {
	if payload == nil {
		return false
	}
	switch value := payload["codex_usage_stale"].(type) {
	case bool:
		return value
	case string:
		return isTruthyQueryValue(value)
	default:
		return false
	}
}

func codexUsagePrimaryResetAt(payload gin.H, now time.Time) (time.Time, bool) {
	primary, ok := codexUsagePrimaryWindow(payload)
	if !ok {
		return time.Time{}, false
	}
	return codexUsageWindowResetAtOrAfter(primary, now)
}

func codexUsagePrimaryWindow(payload gin.H) (map[string]any, bool) {
	rateLimit, ok := codexUsageWindowMap(payload["rate_limit"])
	if !ok {
		return nil, false
	}
	return codexUsageWindowMap(rateLimit["primary_window"])
}

func codexQuotaPrimeKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	accessToken := codexUsageAccessToken(auth)
	if accountID := strings.TrimSpace(resolveCodexUsageAccountID(auth, accessToken)); accountID != "" {
		return "account:" + accountID
	}
	return "auth:" + strings.TrimSpace(auth.ID)
}

func codexQuotaResetAlreadyFixed(auth *coreauth.Auth, resetAt time.Time) bool {
	if auth == nil || resetAt.IsZero() {
		return false
	}
	snapshot, ok := auth.RateLimits["codex"]
	if !ok || snapshot.Primary == nil || snapshot.Primary.ResetsAt == nil || snapshot.Primary.UsedPercent <= 0 {
		return false
	}
	previousResetAt := time.Unix(*snapshot.Primary.ResetsAt, 0)
	return absoluteDurationDifference(previousResetAt.Sub(resetAt), 0) <= codexPersistedResetTolerance
}

func codexUsageNeedsFiveHourPrime(payload gin.H, now time.Time) (time.Time, bool) {
	primary, ok := codexUsagePrimaryWindow(payload)
	if !ok {
		return time.Time{}, false
	}
	usedPercent, ok := numberFromAny(primary["used_percent"])
	// The management API displays remaining quota, while ChatGPT reports the
	// consumed percentage. Treat pristine and 99%-remaining windows as safe to
	// pin; anything more heavily used was already started by real traffic.
	if !ok || usedPercent > codexQuotaPrimeMaxUsedPercent {
		return time.Time{}, false
	}
	window, ok := codexUsageWindowDuration(primary)
	if !ok || absoluteDurationDifference(window, codexFiveHourWindow) > codexFiveHourWindowTolerance {
		return time.Time{}, false
	}
	resetAt, ok := codexUsageWindowResetAtOrAfter(primary, now)
	if !ok {
		return time.Time{}, false
	}
	// An untouched rolling window keeps reporting a reset horizon close to five
	// hours from every observation. Once traffic fixes the boundary, this
	// distance grows and the maintenance request is no longer eligible.
	if absoluteDurationDifference(resetAt.Sub(now), codexFiveHourWindow) > codexFiveHourWindowTolerance {
		return time.Time{}, false
	}
	return resetAt, true
}

func codexUsageFiveHourResetAt(payload gin.H, now time.Time) (time.Time, bool) {
	primary, ok := codexUsagePrimaryWindow(payload)
	if !ok {
		return time.Time{}, false
	}
	window, ok := codexUsageWindowDuration(primary)
	if !ok || absoluteDurationDifference(window, codexFiveHourWindow) > codexFiveHourWindowTolerance {
		return time.Time{}, false
	}
	return codexUsageWindowResetAtOrAfter(primary, now)
}

func codexUsageWindowDuration(window map[string]any) (time.Duration, bool) {
	if seconds, ok := numberFromAny(window["limit_window_seconds"]); ok && seconds > 0 {
		return time.Duration(seconds * float64(time.Second)), true
	}
	for _, key := range []string{"window_minutes", "window_duration_mins"} {
		if minutes, ok := numberFromAny(window[key]); ok && minutes > 0 {
			return time.Duration(minutes * float64(time.Minute)), true
		}
	}
	return 0, false
}

func codexUsageWindowResetAtOrAfter(window map[string]any, now time.Time) (time.Time, bool) {
	if resetAt, ok := codexUsageWindowResetAt(window); ok {
		return resetAt, true
	}
	for _, key := range []string{"reset_after_seconds", "resets_in_seconds"} {
		if seconds, ok := numberFromAny(window[key]); ok && seconds > 0 {
			return now.Add(time.Duration(seconds * float64(time.Second))), true
		}
	}
	return time.Time{}, false
}

func absoluteDurationDifference(left, right time.Duration) time.Duration {
	if left < right {
		return right - left
	}
	return left - right
}

func (h *Handler) pruneCodexQuotaPrimeStates(activeQuotaKeys map[string]struct{}, now time.Time) {
	if h == nil {
		return
	}
	h.codexQuotaMu.Lock()
	defer h.codexQuotaMu.Unlock()
	for quotaKey, state := range h.codexQuotaPrimed {
		_, active := activeQuotaKeys[quotaKey]
		if !active || (!state.inFlight && !state.resetAt.After(now)) {
			delete(h.codexQuotaPrimed, quotaKey)
		}
	}
}

func (h *Handler) reserveCodexQuotaPrime(quotaKey string, resetAt time.Time, now time.Time) bool {
	quotaKey = strings.TrimSpace(quotaKey)
	if h == nil || quotaKey == "" {
		return false
	}
	h.codexQuotaMu.Lock()
	defer h.codexQuotaMu.Unlock()
	if h.codexQuotaPrimed == nil {
		h.codexQuotaPrimed = make(map[string]codexQuotaPrimeState)
	}
	state := h.codexQuotaPrimed[quotaKey]
	if state.inFlight || state.resetAt.After(now.Add(time.Minute)) {
		return false
	}
	h.codexQuotaPrimed[quotaKey] = codexQuotaPrimeState{resetAt: resetAt, inFlight: true}
	return true
}

func (h *Handler) finishCodexQuotaPrime(quotaKey string, resetAt time.Time, success bool) {
	h.codexQuotaMu.Lock()
	defer h.codexQuotaMu.Unlock()
	if !success {
		delete(h.codexQuotaPrimed, quotaKey)
		return
	}
	if h.codexQuotaPrimed == nil {
		h.codexQuotaPrimed = make(map[string]codexQuotaPrimeState)
	}
	h.codexQuotaPrimed[quotaKey] = codexQuotaPrimeState{resetAt: resetAt}
}

func (h *Handler) sendCodexQuotaPrimeRequest(ctx context.Context, auth *coreauth.Auth) error {
	if h == nil || auth == nil {
		return context.Canceled
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return context.Canceled
	}
	model := codexQuotaPrimeModel(auth.ID)
	if model == "" {
		return errCodexQuotaPrimeModelUnavailable
	}
	payload, err := json.Marshal(codexQuotaPrimeRequest{
		Model:           model,
		Instructions:    "Reply with OK.",
		MaxOutputTokens: 16,
		Store:           false,
		Stream:          false,
		Input: []codexQuotaPrimeInputMessage{{
			Type: "message",
			Role: "user",
			Content: []codexQuotaPrimeInputContent{{
				Type: "input_text",
				Text: "OK",
			}},
		}},
	})
	if err != nil {
		return err
	}
	_, err = manager.Execute(ctx, []string{"codex"}, coreexecutor.Request{
		Model:   model,
		Payload: payload,
		Format:  sdktranslator.FormatOpenAIResponse,
	}, coreexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		OriginalRequest: payload,
		Metadata: map[string]any{
			coreexecutor.PinnedAuthMetadataKey:          auth.ID,
			coreexecutor.RequestedModelMetadataKey:      model,
			coreexecutor.MaxRetryCredentialsMetadataKey: 1,
		},
	})
	return err
}

func codexQuotaPrimeModel(authID string) string {
	models := registry.GetGlobalRegistry().GetModelIDsForClient(strings.TrimSpace(authID))
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model), codexQuotaPrimeModelID) {
			return codexQuotaPrimeModelID
		}
	}
	return ""
}

func (h *Handler) updateCodexRateLimitsFromUsage(ctx context.Context, auth *coreauth.Auth, payload gin.H, now time.Time) {
	if h == nil || auth == nil {
		return
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return
	}
	rateLimit, ok := codexUsageWindowMap(payload["rate_limit"])
	if !ok {
		return
	}
	snapshot := coreauth.RateLimitSnapshot{
		LimitID:   "codex",
		PlanType:  strings.TrimSpace(valueAsString(payload["plan_type"])),
		UpdatedAt: now,
	}
	if snapshot.PlanType == "" {
		snapshot.PlanType = codexUsagePlanType(auth)
	}
	if primary, ok := codexUsageWindowMap(rateLimit["primary_window"]); ok {
		snapshot.Primary = codexUsageRateLimitWindow(primary, now)
	}
	if secondary, ok := codexUsageWindowMap(rateLimit["secondary_window"]); ok {
		snapshot.Secondary = codexUsageRateLimitWindow(secondary, now)
	}
	if snapshot.Primary != nil || snapshot.Secondary != nil {
		manager.UpdateRateLimits(ctx, auth.ID, []coreauth.RateLimitSnapshot{snapshot})
	}
}

func codexUsageRateLimitWindow(window map[string]any, now time.Time) *coreauth.RateLimitWindow {
	used, ok := numberFromAny(window["used_percent"])
	if !ok {
		return nil
	}
	result := &coreauth.RateLimitWindow{UsedPercent: used}
	if duration, ok := codexUsageWindowDuration(window); ok {
		minutes := int64(duration / time.Minute)
		result.WindowMinutes = &minutes
	}
	if resetAt, ok := codexUsageWindowResetAtOrAfter(window, now); ok {
		unix := resetAt.Unix()
		result.ResetsAt = &unix
	}
	return result
}
