package management

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"math/big"
	"sort"
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
	codexQuotaRefreshMinInterval  = 10 * time.Minute
	codexQuotaRefreshMaxInterval  = 20 * time.Minute
	codexFiveHourWindow           = 5 * time.Hour
	codexFiveHourWindowTolerance  = 5 * time.Minute
	codexQuotaPrimeMaxUsedPercent = 1.0
	codexQuotaWorkerLimit         = 4
	codexQuotaRequestTimeout      = 45 * time.Second
)

type codexQuotaPrimeState struct {
	resetAt  time.Time
	inFlight bool
}

// StartCodexQuotaMaintenance starts the background Codex quota poller. The
// first poll is deliberately jittered by 10-20 minutes so startup and multiple
// proxy instances do not create a synchronized usage-request burst.
func (h *Handler) StartCodexQuotaMaintenance() {
	if h == nil {
		return
	}
	select {
	case <-h.cleanupStop:
		return
	default:
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
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
	timer := time.NewTimer(randomCodexQuotaRefreshInterval())
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			h.runCodexQuotaMaintenanceCycle(ctx)
			timer.Reset(randomCodexQuotaRefreshInterval())
		case <-ctx.Done():
			return
		}
	}
}

func randomCodexQuotaRefreshInterval() time.Duration {
	span := codexQuotaRefreshMaxInterval - codexQuotaRefreshMinInterval
	if span <= 0 {
		return codexQuotaRefreshMinInterval
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(span)+1))
	if err != nil {
		return codexQuotaRefreshMinInterval + span/2
	}
	return codexQuotaRefreshMinInterval + time.Duration(value.Int64())
}

func (h *Handler) runCodexQuotaMaintenanceCycle(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return
	}
	auths := manager.List()
	sem := make(chan struct{}, codexQuotaWorkerLimit)
	var wg sync.WaitGroup
	for _, auth := range auths {
		if !codexQuotaMaintenanceAuthEligible(auth) {
			continue
		}
		wg.Add(1)
		go func(auth *coreauth.Auth) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			ctx, cancel := context.WithTimeout(ctx, codexQuotaRequestTimeout)
			defer cancel()
			h.maintainCodexQuotaForAuth(ctx, auth, time.Now())
		}(auth)
	}
	wg.Wait()
}

func codexQuotaMaintenanceAuthEligible(auth *coreauth.Auth) bool {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return !auth.Disabled && auth.Status != coreauth.StatusDisabled
}

func (h *Handler) maintainCodexQuotaForAuth(ctx context.Context, auth *coreauth.Auth, now time.Time) {
	if h == nil || !codexQuotaMaintenanceAuthEligible(auth) {
		return
	}
	auth = h.refreshCodexUsageAuthIfNeeded(ctx, auth)
	payload, _, err := h.fetchCodexUsageWithCache(ctx, auth, codexUsageRequestOptions{
		force: true,
		ttl:   codexUsageCacheDefaultTTL,
	})
	if err != nil {
		log.WithError(err).WithField("auth_id", auth.ID).Debug("codex quota maintenance usage refresh failed")
		return
	}
	h.updateCodexRateLimitsFromUsage(ctx, auth, payload, now)
	resetAt, shouldPrime := codexUsageNeedsFiveHourPrime(payload, now)
	if !shouldPrime || !h.reserveCodexQuotaPrime(auth.ID, resetAt, now) {
		return
	}

	if err = h.sendCodexQuotaPrimeRequest(ctx, auth); err != nil {
		h.finishCodexQuotaPrime(auth.ID, resetAt, false)
		log.WithError(err).WithField("auth_id", auth.ID).Warn("codex quota maintenance failed to prime 5h window")
		return
	}
	h.finishCodexQuotaPrime(auth.ID, resetAt, true)
	log.WithFields(log.Fields{"auth_id": auth.ID, "reset_at": resetAt.UTC()}).Info("codex quota maintenance primed 5h window")

	if refreshed, _, refreshErr := h.fetchCodexUsageWithCache(ctx, auth, codexUsageRequestOptions{
		force: true,
		ttl:   codexUsageCacheDefaultTTL,
	}); refreshErr == nil {
		h.updateCodexRateLimitsFromUsage(ctx, auth, refreshed, time.Now())
	}
}

func codexUsageNeedsFiveHourPrime(payload gin.H, now time.Time) (time.Time, bool) {
	rateLimit, ok := codexUsageWindowMap(payload["rate_limit"])
	if !ok {
		return time.Time{}, false
	}
	primary, ok := codexUsageWindowMap(rateLimit["primary_window"])
	if !ok {
		return time.Time{}, false
	}
	usedPercent, ok := numberFromAny(primary["used_percent"])
	// The UI expresses this as remaining quota, so 99% remaining corresponds
	// to an upstream used_percent value of 1. Prime both pristine and nearly
	// pristine windows to stabilize their five-hour reset boundary.
	if !ok || usedPercent > codexQuotaPrimeMaxUsedPercent {
		return time.Time{}, false
	}
	window, ok := codexUsageWindowDuration(primary)
	if !ok || durationDistance(window, codexFiveHourWindow) > codexFiveHourWindowTolerance {
		return time.Time{}, false
	}
	resetAt, ok := codexUsageWindowResetAtOrAfter(primary, now)
	if !ok {
		return time.Time{}, false
	}
	remaining := resetAt.Sub(now)
	if durationDistance(remaining, codexFiveHourWindow) > codexFiveHourWindowTolerance {
		return time.Time{}, false
	}
	return resetAt, true
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

func durationDistance(left, right time.Duration) time.Duration {
	if left < right {
		return right - left
	}
	return left - right
}

func (h *Handler) reserveCodexQuotaPrime(authID string, resetAt time.Time, now time.Time) bool {
	authID = strings.TrimSpace(authID)
	if h == nil || authID == "" {
		return false
	}
	h.codexQuotaMu.Lock()
	defer h.codexQuotaMu.Unlock()
	if h.codexQuotaPrimed == nil {
		h.codexQuotaPrimed = make(map[string]codexQuotaPrimeState)
	}
	state := h.codexQuotaPrimed[authID]
	if state.inFlight || state.resetAt.After(now.Add(time.Minute)) {
		return false
	}
	h.codexQuotaPrimed[authID] = codexQuotaPrimeState{resetAt: resetAt, inFlight: true}
	return true
}

func (h *Handler) finishCodexQuotaPrime(authID string, resetAt time.Time, success bool) {
	h.codexQuotaMu.Lock()
	defer h.codexQuotaMu.Unlock()
	if !success {
		delete(h.codexQuotaPrimed, authID)
		return
	}
	h.codexQuotaPrimed[authID] = codexQuotaPrimeState{resetAt: resetAt}
}

func (h *Handler) sendCodexQuotaPrimeRequest(ctx context.Context, auth *coreauth.Auth) error {
	if h == nil || h.authManager == nil || auth == nil {
		return context.Canceled
	}
	model := codexQuotaPrimeModel(auth.ID)
	if model == "" {
		return &coreauth.Error{Code: "model_not_found", Message: "no Codex model available for quota priming"}
	}
	payload, err := json.Marshal(map[string]any{
		"model":             model,
		"instructions":      "Reply with OK.",
		"input":             "OK",
		"max_output_tokens": 16,
		"store":             false,
		"stream":            false,
	})
	if err != nil {
		return err
	}
	_, err = h.authManager.Execute(ctx, []string{"codex"}, coreexecutor.Request{
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
	if len(models) == 0 {
		return ""
	}
	preferences := []string{"gpt-5.4-mini", "gpt-5.6-luna", "gpt-5.6-terra"}
	for _, preferred := range preferences {
		for _, model := range models {
			if strings.EqualFold(strings.TrimSpace(model), preferred) {
				return strings.TrimSpace(model)
			}
		}
	}
	sort.Strings(models)
	for _, model := range models {
		lower := strings.ToLower(strings.TrimSpace(model))
		if lower == "" || strings.Contains(lower, "image") || strings.Contains(lower, "auto-review") {
			continue
		}
		return strings.TrimSpace(model)
	}
	return ""
}

func (h *Handler) updateCodexRateLimitsFromUsage(ctx context.Context, auth *coreauth.Auth, payload gin.H, now time.Time) {
	if h == nil || h.authManager == nil || auth == nil {
		return
	}
	rateLimit, ok := codexUsageWindowMap(payload["rate_limit"])
	if !ok {
		return
	}
	snapshot := coreauth.RateLimitSnapshot{LimitID: "codex", UpdatedAt: now}
	if primary, ok := codexUsageWindowMap(rateLimit["primary_window"]); ok {
		snapshot.Primary = codexUsageRateLimitWindow(primary, now)
	}
	if secondary, ok := codexUsageWindowMap(rateLimit["secondary_window"]); ok {
		snapshot.Secondary = codexUsageRateLimitWindow(secondary, now)
	}
	if snapshot.Primary != nil || snapshot.Secondary != nil {
		h.authManager.UpdateRateLimits(ctx, auth.ID, []coreauth.RateLimitSnapshot{snapshot})
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
