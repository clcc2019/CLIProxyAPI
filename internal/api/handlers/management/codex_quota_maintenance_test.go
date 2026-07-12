package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRandomCodexQuotaRefreshIntervalStaysWithinTenToTwentyMinutes(t *testing.T) {
	for index := 0; index < 200; index++ {
		interval := randomCodexQuotaRefreshInterval()
		if interval < codexQuotaRefreshMinInterval || interval > codexQuotaRefreshMaxInterval {
			t.Fatalf("interval = %s, want [%s, %s]", interval, codexQuotaRefreshMinInterval, codexQuotaRefreshMaxInterval)
		}
	}
}

func TestRandomCodexQuotaInitialRefreshDelayStaysWithinTwoToTenSeconds(t *testing.T) {
	for index := 0; index < 200; index++ {
		delay := randomCodexQuotaInitialRefreshDelay()
		if delay < codexQuotaInitialRefreshMinDelay || delay > codexQuotaInitialRefreshMaxDelay {
			t.Fatalf("initial delay = %s, want [%s, %s]", delay, codexQuotaInitialRefreshMinDelay, codexQuotaInitialRefreshMaxDelay)
		}
	}
}

func TestNextCodexQuotaRefreshIntervalFollowsUpcomingReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	handler := &Handler{codexQuotaPrimed: map[string]codexQuotaPrimeState{
		"account:acct-1": {resetAt: now.Add(5 * time.Minute)},
	}}

	got := handler.nextCodexQuotaRefreshInterval(now)
	minimum := 5*time.Minute + codexQuotaResetFollowupMinDelay
	maximum := 5*time.Minute + codexQuotaResetFollowupMaxDelay
	if got < minimum || got > maximum {
		t.Fatalf("next refresh = %s, want reset-aligned interval [%s, %s]", got, minimum, maximum)
	}
}

func TestRunCodexQuotaAuthWorkersBoundsConcurrencyAndProcessesAll(t *testing.T) {
	const (
		authCount   = 40
		workerLimit = 4
	)
	auths := make([]*coreauth.Auth, authCount)
	for index := range auths {
		auths[index] = &coreauth.Auth{ID: strconv.Itoa(index), Provider: "codex"}
	}

	entered := make(chan struct{}, workerLimit)
	release := make(chan struct{})
	done := make(chan struct{})
	var started atomic.Int32
	var processed atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	go func() {
		runCodexQuotaAuthWorkers(context.Background(), auths, workerLimit, func(_ context.Context, _ *coreauth.Auth) {
			current := active.Add(1)
			for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
			}
			if started.Add(1) <= workerLimit {
				entered <- struct{}{}
			}
			<-release
			active.Add(-1)
			processed.Add(1)
		})
		close(done)
	}()

	for range workerLimit {
		<-entered
	}
	if got := maximum.Load(); got != workerLimit {
		t.Fatalf("maximum concurrency = %d, want %d", got, workerLimit)
	}
	close(release)
	<-done
	if got := processed.Load(); got != authCount {
		t.Fatalf("processed auths = %d, want %d", got, authCount)
	}
}

func TestRunCodexQuotaAuthWorkersStopsDispatchOnCancellation(t *testing.T) {
	const (
		authCount   = 100
		workerLimit = 4
	)
	auths := make([]*coreauth.Auth, authCount)
	for index := range auths {
		auths[index] = &coreauth.Auth{ID: strconv.Itoa(index), Provider: "codex"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	startedCh := make(chan struct{}, workerLimit)
	done := make(chan struct{})
	var started atomic.Int32
	go func() {
		runCodexQuotaAuthWorkers(ctx, auths, workerLimit, func(workerCtx context.Context, _ *coreauth.Auth) {
			started.Add(1)
			startedCh <- struct{}{}
			<-workerCtx.Done()
		})
		close(done)
	}()

	for range workerLimit {
		<-startedCh
	}
	cancel()
	<-done
	if got := started.Load(); got > workerLimit {
		t.Fatalf("started auths after cancellation = %d, want at most %d", got, workerLimit)
	}
}

func TestCodexUsageNeedsFiveHourPrime(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	payload := func(used float64, windowSeconds int64, resetAfter time.Duration) gin.H {
		return gin.H{"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent":         used,
				"limit_window_seconds": windowSeconds,
				"reset_at":             now.Add(resetAfter).Unix(),
			},
		}}
	}

	tests := []struct {
		name    string
		payload gin.H
		want    bool
	}{
		{name: "pristine five hour window", payload: payload(0, 18_000, 5*time.Hour), want: true},
		{name: "ninety nine percent remaining", payload: payload(1, 18_000, 5*time.Hour), want: true},
		{name: "below ninety nine percent remaining", payload: payload(1.01, 18_000, 5*time.Hour)},
		{name: "fixed window already aging", payload: payload(0, 18_000, 4*time.Hour)},
		{name: "weekly window", payload: payload(0, 7*24*60*60, 5*time.Hour)},
		{name: "missing rate limit", payload: gin.H{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := codexUsageNeedsFiveHourPrime(test.payload, now)
			if got != test.want {
				t.Fatalf("codexUsageNeedsFiveHourPrime() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCodexQuotaMaintenanceRejectsStaleUsagePayload(t *testing.T) {
	if !codexUsagePayloadIsStale(gin.H{"codex_usage_stale": true}) {
		t.Fatal("boolean stale marker should be recognized")
	}
	if !codexUsagePayloadIsStale(gin.H{"codex_usage_stale": "true"}) {
		t.Fatal("string stale marker should be recognized")
	}
	if codexUsagePayloadIsStale(gin.H{"codex_usage_stale": false}) {
		t.Fatal("fresh payload should not be marked stale")
	}
}

func TestCodexQuotaMaintenanceRequiresFreshUsageWhileUIFallbackRemainsAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withFastCodexUsageRetry(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary gateway failure", http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	originalUsageURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalUsageURL })

	auth := &coreauth.Auth{
		ID:       "fresh-required.json",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "account-1",
		},
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	t.Cleanup(handler.Close)
	cache := handler.codexUsageHandlerCache()
	cache.store(handler.codexUsageCacheKey(auth), &codexUsageCacheEntry{
		Payload: gin.H{"rate_limit": gin.H{
			"primary_window": gin.H{"used_percent": 0, "limit_window_seconds": 18_000},
		}},
		ExpiresAt:  time.Now().Add(-time.Second),
		StaleUntil: time.Now().Add(time.Minute),
	})

	payload, status, err := handler.fetchCodexUsageWithCache(context.Background(), auth, codexUsageRequestOptions{
		force:        true,
		requireFresh: true,
		ttl:          codexUsageCacheDefaultTTL,
	})
	if err == nil || payload != nil || status != http.StatusBadGateway {
		t.Fatalf("fresh fetch = payload %#v, status %d, err %v; want upstream failure", payload, status, err)
	}

	payload, status, err = handler.fetchCodexUsageWithCache(context.Background(), auth, codexUsageRequestOptions{
		force: true,
		ttl:   codexUsageCacheDefaultTTL,
	})
	if err != nil || status != http.StatusOK || !codexUsagePayloadIsStale(payload) {
		t.Fatalf("UI fallback = payload %#v, status %d, err %v; want stale cached success", payload, status, err)
	}
}

func TestCodexQuotaPrimeKeyDeduplicatesAuthFilesForSameAccount(t *testing.T) {
	first := &coreauth.Auth{ID: "first.json", Metadata: map[string]any{"account_id": "acct-1"}}
	second := &coreauth.Auth{ID: "second.json", Metadata: map[string]any{"account_id": "acct-1"}}
	third := &coreauth.Auth{ID: "third.json", Metadata: map[string]any{"account_id": "acct-2"}}

	if got, want := codexQuotaPrimeKey(first), codexQuotaPrimeKey(second); got != want {
		t.Fatalf("same account quota keys differ: %q != %q", got, want)
	}
	if codexQuotaPrimeKey(first) == codexQuotaPrimeKey(third) {
		t.Fatal("different accounts should not share a quota key")
	}
}

func TestCodexQuotaResetAlreadyFixedUsesPersistedRateLimit(t *testing.T) {
	resetAt := time.Unix(1_800_018_000, 0)
	resetUnix := resetAt.Unix()
	auth := &coreauth.Auth{RateLimits: map[string]coreauth.RateLimitSnapshot{
		"codex": {
			Primary: &coreauth.RateLimitWindow{UsedPercent: 0.5, ResetsAt: &resetUnix},
		},
	}}
	if !codexQuotaResetAlreadyFixed(auth, resetAt) {
		t.Fatal("matching used persisted window should be treated as fixed")
	}
	if codexQuotaResetAlreadyFixed(auth, resetAt.Add(2*time.Second)) {
		t.Fatal("a moved reset boundary should still be eligible for priming")
	}
	auth.RateLimits["codex"].Primary.UsedPercent = 0
	if codexQuotaResetAlreadyFixed(auth, resetAt) {
		t.Fatal("pristine persisted observation is insufficient proof of a fixed window")
	}
}

func TestReserveCodexQuotaPrimeDeduplicatesConcurrentCyclesAndRetriesFailure(t *testing.T) {
	handler := &Handler{}
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(5 * time.Hour)

	const workers = 32
	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if handler.reserveCodexQuotaPrime("codex-auth", resetAt, now) {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful concurrent reservations = %d, want 1", got)
	}

	handler.finishCodexQuotaPrime("codex-auth", resetAt, false)
	if !handler.reserveCodexQuotaPrime("codex-auth", resetAt, now) {
		t.Fatal("failed prime should be eligible for retry")
	}
	handler.finishCodexQuotaPrime("codex-auth", resetAt, true)
	if handler.reserveCodexQuotaPrime("codex-auth", resetAt, now.Add(time.Minute)) {
		t.Fatal("successful prime should remain deduplicated until the reset boundary")
	}
}

func TestPruneCodexQuotaPrimeStatesRemovesExpiredAndMissingAccounts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	handler := &Handler{codexQuotaPrimed: map[string]codexQuotaPrimeState{
		"account:active":  {resetAt: now.Add(time.Hour)},
		"account:expired": {resetAt: now.Add(-time.Second)},
		"account:removed": {resetAt: now.Add(time.Hour)},
	}}
	handler.pruneCodexQuotaPrimeStates(map[string]struct{}{
		"account:active":  {},
		"account:expired": {},
	}, now)

	if _, ok := handler.codexQuotaPrimed["account:active"]; !ok {
		t.Fatal("active future state was pruned")
	}
	if _, ok := handler.codexQuotaPrimed["account:expired"]; ok {
		t.Fatal("expired state was not pruned")
	}
	if _, ok := handler.codexQuotaPrimed["account:removed"]; ok {
		t.Fatal("state for removed auth was not pruned")
	}
}

func TestCodexQuotaPrimeModelRequiresGPT55(t *testing.T) {
	const unavailableAuthID = "quota-prime-without-gpt55"
	registry.GetGlobalRegistry().RegisterClient(unavailableAuthID, "codex", []*registry.ModelInfo{{ID: "gpt-5.6-sol"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(unavailableAuthID) })
	if got := codexQuotaPrimeModel(unavailableAuthID); got != "" {
		t.Fatalf("codexQuotaPrimeModel() = %q without gpt-5.5, want empty", got)
	}

	const availableAuthID = "quota-prime-with-gpt55"
	registry.GetGlobalRegistry().RegisterClient(availableAuthID, "codex", []*registry.ModelInfo{
		{ID: "gpt-5.6-sol"},
		{ID: "GPT-5.5"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(availableAuthID) })
	if got := codexQuotaPrimeModel(availableAuthID); got != codexQuotaPrimeModelID {
		t.Fatalf("codexQuotaPrimeModel() = %q, want %q", got, codexQuotaPrimeModelID)
	}
}

type quotaPrimeExecutor struct {
	calls    atomic.Int32
	authID   atomic.Value
	payload  atomic.Value
	pinnedID atomic.Value
}

func (e *quotaPrimeExecutor) Identifier() string { return "codex" }

func (e *quotaPrimeExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls.Add(1)
	e.authID.Store(auth.ID)
	e.payload.Store(string(req.Payload))
	e.pinnedID.Store(opts.Metadata[coreexecutor.PinnedAuthMetadataKey].(string))
	return coreexecutor.Response{Payload: []byte(`{"id":"response-prime","status":"completed"}`)}, nil
}

func (e *quotaPrimeExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *quotaPrimeExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *quotaPrimeExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *quotaPrimeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestMaintainCodexQuotaPrimesOncePerFiveHourWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().Truncate(time.Second)
	var usageCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		usageCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":` +
			strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10) + `},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":` +
			strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10) + `}}}`))
	}))
	t.Cleanup(server.Close)
	originalUsageURL := codexUsageURL
	codexUsageURL = server.URL
	t.Cleanup(func() { codexUsageURL = originalUsageURL })

	const authID = "quota-prime-auth"
	const model = "gpt-5.5"
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	executor := &quotaPrimeExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "account-1",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	t.Cleanup(handler.Close)
	handler.maintainCodexQuotaForAuth(context.Background(), mustQuotaMaintenanceAuthByID(t, manager, authID), now)
	handler.maintainCodexQuotaForAuth(context.Background(), mustQuotaMaintenanceAuthByID(t, manager, authID), now.Add(time.Minute))

	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("quota prime Execute calls = %d, want 1", got)
	}
	if got := usageCalls.Load(); got != 3 {
		t.Fatalf("usage calls = %d, want 3 (poll, post-prime refresh, next poll)", got)
	}
	if got := executor.authID.Load(); got != authID {
		t.Fatalf("executor auth ID = %#v, want %q", got, authID)
	}
	if got := executor.pinnedID.Load(); got != authID {
		t.Fatalf("pinned auth ID = %#v, want %q", got, authID)
	}
	primePayload := executor.payload.Load().(string)
	if !strings.Contains(primePayload, `"max_output_tokens":16`) || !strings.Contains(primePayload, `"store":false`) {
		t.Fatalf("quota prime payload is not minimal/client-shaped: %s", primePayload)
	}
	var decodedPrime struct {
		Model string `json:"model"`
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(primePayload), &decodedPrime); err != nil {
		t.Fatalf("decode quota prime payload: %v", err)
	}
	if decodedPrime.Model != model {
		t.Fatalf("quota prime model = %q, want %q", decodedPrime.Model, model)
	}
	if len(decodedPrime.Input) != 1 || decodedPrime.Input[0].Type != "message" || decodedPrime.Input[0].Role != "user" {
		t.Fatalf("quota prime input is not a user message list: %#v", decodedPrime.Input)
	}
	if len(decodedPrime.Input[0].Content) != 1 || decodedPrime.Input[0].Content[0].Type != "input_text" || decodedPrime.Input[0].Content[0].Text != "OK" {
		t.Fatalf("quota prime message content is invalid: %#v", decodedPrime.Input[0].Content)
	}
	updated := mustQuotaMaintenanceAuthByID(t, manager, authID)
	snapshot, ok := updated.RateLimits["codex"]
	if !ok || snapshot.Primary == nil {
		t.Fatalf("rate limit snapshot missing: %#v", updated.RateLimits)
	}
	if snapshot.Primary.UsedPercent != 1 || snapshot.Primary.WindowMinutes == nil || *snapshot.Primary.WindowMinutes != 300 {
		t.Fatalf("primary window = %#v, want 99%% remaining 300-minute window", snapshot.Primary)
	}
}

func mustQuotaMaintenanceAuthByID(t *testing.T, manager *coreauth.Manager, authID string) *coreauth.Auth {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %q not found", authID)
	}
	return auth
}
