package auth

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type fakeProxyLeaseStore struct {
	leases        map[string]string
	released      []string
	failures      map[string]int
	cooldown      map[string]time.Time
	reconciles    int
	acquires      int
	batchAcquires int
}

type failingReconcileProxyLeaseStore struct {
	fakeProxyLeaseStore
}

func (s *failingReconcileProxyLeaseStore) ReconcileProxyLeases(_ context.Context, _ []string, _ []string) error {
	return errors.New("reconcile failed")
}

type benchmarkProxyLeaseStore struct{}

func (s benchmarkProxyLeaseStore) AcquireProxyLease(_ context.Context, authID string, proxyURLs []string) (ProxyLease, bool, error) {
	if len(proxyURLs) == 0 {
		return ProxyLease{}, false, nil
	}
	return ProxyLease{AuthID: authID, ProxyURL: proxyURLs[0]}, true, nil
}

func (s benchmarkProxyLeaseStore) AcquireProxyLeases(_ context.Context, authIDs []string, proxyURLs []string) ([]ProxyLease, error) {
	if len(authIDs) == 0 || len(proxyURLs) == 0 {
		return nil, nil
	}
	leases := make([]ProxyLease, len(authIDs))
	for i, authID := range authIDs {
		if i >= len(proxyURLs) {
			break
		}
		leases[i] = ProxyLease{AuthID: authID, ProxyURL: proxyURLs[i]}
	}
	return leases, nil
}

func (s benchmarkProxyLeaseStore) ReleaseProxyLease(_ context.Context, _ string) error {
	return nil
}

func (s benchmarkProxyLeaseStore) ReconcileProxyLeases(_ context.Context, _ []string, _ []string) error {
	return nil
}

func (s benchmarkProxyLeaseStore) RecordProxyLeaseFailure(_ context.Context, _, _ string, _ int, _ time.Duration) (ProxyLeaseFailure, error) {
	return ProxyLeaseFailure{}, nil
}

func (s benchmarkProxyLeaseStore) ClearProxyLeaseFailure(_ context.Context, _ string) error {
	return nil
}

func (s *fakeProxyLeaseStore) AcquireProxyLease(_ context.Context, authID string, proxyURLs []string) (ProxyLease, bool, error) {
	s.acquires++
	return s.acquireProxyLease(authID, proxyURLs)
}

func (s *fakeProxyLeaseStore) AcquireProxyLeases(_ context.Context, authIDs []string, proxyURLs []string) ([]ProxyLease, error) {
	s.batchAcquires++
	leases := make([]ProxyLease, len(authIDs))
	for i, authID := range authIDs {
		lease, ok, err := s.acquireProxyLease(authID, proxyURLs)
		if err != nil {
			return nil, err
		}
		if ok {
			leases[i] = lease
		}
	}
	return leases, nil
}

func (s *fakeProxyLeaseStore) acquireProxyLease(authID string, proxyURLs []string) (ProxyLease, bool, error) {
	if s.leases == nil {
		s.leases = make(map[string]string)
	}
	if proxyURL := strings.TrimSpace(s.leases[authID]); proxyURL != "" {
		return ProxyLease{AuthID: authID, ProxyURL: proxyURL}, true, nil
	}
	used := make(map[string]struct{}, len(s.leases))
	for _, proxyURL := range s.leases {
		used[proxyURL] = struct{}{}
	}
	for _, proxyURL := range proxyURLs {
		proxyURL = strings.TrimSpace(proxyURL)
		if proxyURL == "" {
			continue
		}
		if recoverAt, ok := s.cooldown[proxyURL]; ok {
			if recoverAt.After(time.Now()) {
				continue
			}
			delete(s.cooldown, proxyURL)
			delete(s.failures, proxyURL)
		}
		if _, ok := used[proxyURL]; ok {
			continue
		}
		s.leases[authID] = proxyURL
		return ProxyLease{AuthID: authID, ProxyURL: proxyURL}, true, nil
	}
	return ProxyLease{}, false, nil
}

func (s *fakeProxyLeaseStore) ReleaseProxyLease(_ context.Context, authID string) error {
	delete(s.leases, authID)
	s.released = append(s.released, authID)
	return nil
}

func (s *fakeProxyLeaseStore) RecordProxyLeaseFailure(_ context.Context, authID, proxyURL string, threshold int, cooldown time.Duration) (ProxyLeaseFailure, error) {
	if s.failures == nil {
		s.failures = make(map[string]int)
	}
	if s.cooldown == nil {
		s.cooldown = make(map[string]time.Time)
	}
	if strings.TrimSpace(s.leases[authID]) != strings.TrimSpace(proxyURL) || threshold <= 0 {
		return ProxyLeaseFailure{}, nil
	}
	s.failures[proxyURL]++
	result := ProxyLeaseFailure{ProxyURL: proxyURL, Failures: s.failures[proxyURL]}
	if s.failures[proxyURL] >= threshold {
		delete(s.leases, authID)
		delete(s.failures, proxyURL)
		result.CooledDown = true
		result.RecoverAt = time.Now().Add(cooldown)
		s.cooldown[proxyURL] = result.RecoverAt
	}
	return result, nil
}

func (s *fakeProxyLeaseStore) ClearProxyLeaseFailure(_ context.Context, proxyURL string) error {
	delete(s.failures, proxyURL)
	delete(s.cooldown, proxyURL)
	return nil
}

func (s *fakeProxyLeaseStore) ReconcileProxyLeases(_ context.Context, activeAuthIDs []string, proxyURLs []string) error {
	s.reconciles++
	active := make(map[string]struct{}, len(activeAuthIDs))
	for _, authID := range activeAuthIDs {
		active[authID] = struct{}{}
	}
	allowed := make(map[string]struct{}, len(proxyURLs))
	for _, proxyURL := range proxyURLs {
		allowed[proxyURL] = struct{}{}
	}
	for authID, proxyURL := range s.leases {
		if _, ok := active[authID]; !ok {
			delete(s.leases, authID)
			continue
		}
		if _, ok := allowed[proxyURL]; !ok {
			delete(s.leases, authID)
		}
	}
	return nil
}

func proxyPoolTestConfig() *internalconfig.Config {
	return &internalconfig.Config{
		ProxyPool: internalconfig.ProxyPoolConfig{
			Enabled:               true,
			StateStore:            "redis",
			ReleaseOnAuthDisabled: true,
			ProxyFailureThreshold: 3,
			ProxyFailureCooldown:  "10m",
			Proxies: []string{
				"http://proxy-a.example.com:8080",
				"http://proxy-b.example.com:8080",
			},
		},
	}
}

func TestProxyPoolAssignsLeaseAndDoesNotPersistProxyURL(t *testing.T) {
	store := &captureAuthStore{}
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(store, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	auth := &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}
	registered, err := mgr.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if registered.ProxyURL != "http://proxy-a.example.com:8080" {
		t.Fatalf("registered ProxyURL = %q", registered.ProxyURL)
	}
	if got := leaseStore.leases[auth.ID]; got != "http://proxy-a.example.com:8080" {
		t.Fatalf("lease = %q", got)
	}
	saved := store.saved[auth.ID]
	if saved == nil {
		t.Fatal("auth was not persisted")
	}
	if saved.ProxyURL != "" {
		t.Fatalf("persisted ProxyURL = %q, want empty", saved.ProxyURL)
	}
	if saved.Attributes != nil && saved.Attributes[proxyPoolAssignedAttribute] != "" {
		t.Fatalf("persisted proxy-pool marker = %q", saved.Attributes[proxyPoolAssignedAttribute])
	}
}

func TestProxyPoolReusesExistingLease(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{leases: map[string]string{
		"oauth-1": "http://proxy-b.example.com:8080",
	}}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	registered, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-1",
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "claude", "access_token": "token"},
	})
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if registered.ProxyURL != "http://proxy-b.example.com:8080" {
		t.Fatalf("ProxyURL = %q, want existing lease", registered.ProxyURL)
	}
}

func TestProxyPoolSkipsExplicitProxyAndAPIKeyAuth(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	explicit, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "explicit",
		Provider: "codex",
		Status:   StatusActive,
		ProxyURL: "http://manual.example.com:8080",
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	})
	if err != nil {
		t.Fatalf("Register explicit error: %v", err)
	}
	if explicit.ProxyURL != "http://manual.example.com:8080" {
		t.Fatalf("explicit ProxyURL = %q", explicit.ProxyURL)
	}

	apiKey, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "api-key",
		Provider: "codex",
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key": "sk-test",
		},
		Metadata: map[string]any{"type": "api_key"},
	})
	if err != nil {
		t.Fatalf("Register api key error: %v", err)
	}
	if apiKey.ProxyURL != "" {
		t.Fatalf("api-key ProxyURL = %q, want empty", apiKey.ProxyURL)
	}
	if len(leaseStore.leases) != 0 {
		t.Fatalf("leases = %#v, want none", leaseStore.leases)
	}
}

func TestProxyPoolSkipsAuthFileProxyNoneAndKeepsWebsocketFlag(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	auth := NewAuthFromAuthFileMetadata(map[string]any{
		"type":         "codex",
		"access_token": "token",
		"proxy-url":    "none",
		"websocket":    true,
	}, AuthFileProjectionOptions{ID: "oauth-none"})

	registered, err := mgr.Register(WithSkipPersist(context.Background()), auth)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if registered.ProxyURL != "none" {
		t.Fatalf("ProxyURL = %q, want none", registered.ProxyURL)
	}
	if got := registered.Attributes["websockets"]; got != "true" {
		t.Fatalf("websockets attr = %q, want true", got)
	}
	if len(leaseStore.leases) != 0 {
		t.Fatalf("leases = %#v, want none for explicit direct proxy", leaseStore.leases)
	}
}

func TestProxyPoolKeepsLeaseWhenAuthFileTogglesWebsocketMode(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	auth := NewAuthFromAuthFileMetadata(map[string]any{
		"type":         "codex",
		"access_token": "token",
		"websockets":   true,
	}, AuthFileProjectionOptions{ID: "oauth-ws"})
	registered, err := mgr.Register(WithSkipPersist(context.Background()), auth)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if registered.ProxyURL != "http://proxy-a.example.com:8080" {
		t.Fatalf("ProxyURL = %q, want pool lease", registered.ProxyURL)
	}

	updated := NewAuthFromAuthFileMetadata(map[string]any{
		"type":         "codex",
		"access_token": "token",
		"websockets":   false,
	}, AuthFileProjectionOptions{ID: "oauth-ws"})
	registered, err = mgr.Update(WithSkipPersist(context.Background()), updated)
	if err != nil {
		t.Fatalf("Update error: %v", err)
	}
	if registered.ProxyURL != "http://proxy-a.example.com:8080" {
		t.Fatalf("ProxyURL after websocket toggle = %q, want same pool lease", registered.ProxyURL)
	}
	if got := registered.Attributes["websockets"]; got != "false" {
		t.Fatalf("websockets attr = %q, want false", got)
	}
	if got := leaseStore.leases["oauth-ws"]; got != "http://proxy-a.example.com:8080" {
		t.Fatalf("lease = %q, want unchanged", got)
	}
}

func TestProxyPoolReassignsLimitedProxyByAuthPriority(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	cfg := proxyPoolTestConfig()
	cfg.ProxyPool.Proxies = []string{"http://proxy-a.example.com:8080"}
	mgr.SetConfig(cfg)

	low := NewAuthFromAuthFileMetadata(map[string]any{
		"type":         "codex",
		"access_token": "low-token",
		"priority":     0,
	}, AuthFileProjectionOptions{ID: "oauth-low"})
	registeredLow, err := mgr.Register(WithSkipPersist(context.Background()), low)
	if err != nil {
		t.Fatalf("Register low error: %v", err)
	}
	if registeredLow.ProxyURL != "http://proxy-a.example.com:8080" {
		t.Fatalf("low ProxyURL = %q, want initial lease", registeredLow.ProxyURL)
	}

	high := NewAuthFromAuthFileMetadata(map[string]any{
		"type":         "codex",
		"access_token": "high-token",
		"priority":     10,
	}, AuthFileProjectionOptions{ID: "oauth-high"})
	if _, err := mgr.Register(WithSkipPersist(context.Background()), high); err != nil {
		t.Fatalf("Register high error: %v", err)
	}
	mgr.flushProxyPoolReconcileQueue(context.Background())
	registeredHigh, ok := mgr.GetByID("oauth-high")
	if !ok {
		t.Fatal("oauth-high missing")
	}
	if registeredHigh.ProxyURL != "http://proxy-a.example.com:8080" {
		t.Fatalf("high ProxyURL = %q, want priority lease", registeredHigh.ProxyURL)
	}
	currentLow, ok := mgr.GetByID("oauth-low")
	if !ok {
		t.Fatal("oauth-low missing")
	}
	if currentLow.ProxyURL != "" {
		t.Fatalf("low ProxyURL after priority reconcile = %q, want cleared", currentLow.ProxyURL)
	}
	if got := leaseStore.leases["oauth-high"]; got != "http://proxy-a.example.com:8080" {
		t.Fatalf("high lease = %q, want proxy-a", got)
	}
	if _, ok := leaseStore.leases["oauth-low"]; ok {
		t.Fatalf("low lease still present: %#v", leaseStore.leases)
	}
}

func TestProxyPoolAuthChangeReconcileIsDebounced(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	cfg := proxyPoolTestConfig()
	cfg.ProxyPool.Proxies = []string{"http://proxy-a.example.com:8080"}
	mgr.SetConfig(cfg)

	for i := 0; i < 3; i++ {
		auth := NewAuthFromAuthFileMetadata(map[string]any{
			"type":         "codex",
			"access_token": "token-" + strconv.Itoa(i),
			"priority":     i,
		}, AuthFileProjectionOptions{ID: "oauth-" + strconv.Itoa(i)})
		if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatalf("Register #%d error: %v", i, err)
		}
	}

	mgr.flushProxyPoolReconcileQueue(context.Background())
	if leaseStore.reconciles != 1 {
		t.Fatalf("reconciles = %d, want 1", leaseStore.reconciles)
	}
}

func TestProxyPoolReconcileUsesBatchAcquireWhenAvailable(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	cfg := proxyPoolTestConfig()
	mgr.runtimeConfig.Store(cfg)

	mgr.mu.Lock()
	mgr.auths["oauth-1"] = &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token-1"},
	}
	mgr.auths["oauth-2"] = &Auth{
		ID:       "oauth-2",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token-2"},
	}
	mgr.mu.Unlock()

	mgr.ReconcileProxyPoolLeases(context.Background())
	if leaseStore.batchAcquires != 1 {
		t.Fatalf("batch acquires = %d, want 1", leaseStore.batchAcquires)
	}
	if leaseStore.acquires != 0 {
		t.Fatalf("single acquires = %d, want 0", leaseStore.acquires)
	}
	if got := leaseStore.leases["oauth-1"]; got != "http://proxy-a.example.com:8080" {
		t.Fatalf("oauth-1 lease = %q, want proxy-a", got)
	}
	if got := leaseStore.leases["oauth-2"]; got != "http://proxy-b.example.com:8080" {
		t.Fatalf("oauth-2 lease = %q, want proxy-b", got)
	}
}

func TestProxyPoolReleasesOnRemove(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	auth := &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if _, err := mgr.Remove(context.Background(), auth.ID); err != nil {
		t.Fatalf("Remove error: %v", err)
	}
	if len(leaseStore.released) != 1 || leaseStore.released[0] != auth.ID {
		t.Fatalf("released = %#v, want %q", leaseStore.released, auth.ID)
	}
}

func TestProxyPoolReleasesWhenConfigDisabled(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	auth := &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	mgr.SetConfig(&internalconfig.Config{})

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing after config update")
	}
	if current.ProxyURL != "" {
		t.Fatalf("ProxyURL = %q, want cleared", current.ProxyURL)
	}
	if _, ok := leaseStore.leases[auth.ID]; ok {
		t.Fatalf("lease still present after config disabled: %#v", leaseStore.leases)
	}
}

func TestProxyPoolReconcileFailureFallsBackToRelease(t *testing.T) {
	leaseStore := &failingReconcileProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	auth := &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	mgr.SetConfig(&internalconfig.Config{})

	if len(leaseStore.released) != 1 || leaseStore.released[0] != auth.ID {
		t.Fatalf("released = %#v, want %q", leaseStore.released, auth.ID)
	}
}

func TestProxyPoolReconcilePrunesMissingAuthLease(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{leases: map[string]string{
		"missing-auth": "http://proxy-a.example.com:8080",
	}}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())
	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	mgr.ReconcileProxyPoolLeases(context.Background())

	if _, ok := leaseStore.leases["missing-auth"]; ok {
		t.Fatalf("stale lease was not pruned: %#v", leaseStore.leases)
	}
}

func TestProxyPoolClearsAssignedProxyWhenNoLeaseAvailable(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	mgr.SetConfig(proxyPoolTestConfig())

	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register oauth-1 error: %v", err)
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-2",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register oauth-2 error: %v", err)
	}

	mgr.SetConfig(&internalconfig.Config{
		ProxyPool: internalconfig.ProxyPoolConfig{
			Enabled:               true,
			StateStore:            "redis",
			ReleaseOnAuthDisabled: true,
			Proxies:               []string{"http://proxy-a.example.com:8080"},
		},
	})

	current, ok := mgr.GetByID("oauth-2")
	if !ok {
		t.Fatal("oauth-2 missing")
	}
	if current.ProxyURL != "" {
		t.Fatalf("oauth-2 ProxyURL = %q, want cleared", current.ProxyURL)
	}
}

func TestProxyPoolTransportFailureBelowThresholdKeepsLease(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	cfg := proxyPoolTestConfig()
	cfg.ProxyPool.ProxyFailureThreshold = 2
	mgr.SetConfig(cfg)

	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	mgr.MarkResult(context.Background(), Result{
		AuthID:  "oauth-1",
		Success: false,
		Error:   &Error{Message: "proxyconnect tcp: dial tcp 127.0.0.1:1080: i/o timeout"},
	})

	current, ok := mgr.GetByID("oauth-1")
	if !ok {
		t.Fatal("auth missing")
	}
	if current.ProxyURL != "http://proxy-a.example.com:8080" {
		t.Fatalf("ProxyURL = %q, want lease kept", current.ProxyURL)
	}
	if got := leaseStore.failures["http://proxy-a.example.com:8080"]; got != 1 {
		t.Fatalf("failures = %d, want 1", got)
	}
}

func TestProxyPoolTransportFailureAtThresholdCoolsAndReleasesLease(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	cfg := proxyPoolTestConfig()
	cfg.ProxyPool.ProxyFailureThreshold = 2
	cfg.ProxyPool.ProxyFailureCooldown = "30m"
	mgr.SetConfig(cfg)

	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register oauth-1 error: %v", err)
	}
	for i := 0; i < 2; i++ {
		mgr.MarkResult(context.Background(), Result{
			AuthID:  "oauth-1",
			Success: false,
			Error:   &Error{Message: "proxyconnect tcp: dial tcp 127.0.0.1:1080: connection refused"},
		})
	}

	current, ok := mgr.GetByID("oauth-1")
	if !ok {
		t.Fatal("oauth-1 missing")
	}
	if current.ProxyURL != "" {
		t.Fatalf("ProxyURL = %q, want cleared after proxy cooldown", current.ProxyURL)
	}
	if _, ok := leaseStore.leases["oauth-1"]; ok {
		t.Fatalf("lease was not released: %#v", leaseStore.leases)
	}
	if recoverAt, ok := leaseStore.cooldown["http://proxy-a.example.com:8080"]; !ok || !recoverAt.After(time.Now()) {
		t.Fatalf("cooldown = %v/%v, want active", recoverAt, ok)
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-2",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register oauth-2 error: %v", err)
	}
	next, ok := mgr.GetByID("oauth-2")
	if !ok {
		t.Fatal("oauth-2 missing")
	}
	if next.ProxyURL != "http://proxy-b.example.com:8080" {
		t.Fatalf("oauth-2 ProxyURL = %q, want cooled proxy skipped", next.ProxyURL)
	}
}

func TestProxyPoolHTTPStatusDoesNotCountAsProxyFailure(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	cfg := proxyPoolTestConfig()
	cfg.ProxyPool.ProxyFailureThreshold = 1
	mgr.SetConfig(cfg)

	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	mgr.MarkResult(context.Background(), Result{
		AuthID:  "oauth-1",
		Success: false,
		Error:   &Error{Message: "rate limited", HTTPStatus: 429},
	})

	current, ok := mgr.GetByID("oauth-1")
	if !ok {
		t.Fatal("auth missing")
	}
	if current.ProxyURL != "http://proxy-a.example.com:8080" {
		t.Fatalf("ProxyURL = %q, want lease kept", current.ProxyURL)
	}
	if got := leaseStore.failures["http://proxy-a.example.com:8080"]; got != 0 {
		t.Fatalf("failures = %d, want 0", got)
	}
}

func TestProxyPoolTransportFailureMatchesMixedCaseAndJoinedFields(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
	}{
		{
			name: "mixed case message",
			err:  &Error{Message: "ProxyConnect TCP: Dial TCP 127.0.0.1:1080: I/O Timeout"},
		},
		{
			name: "code message boundary",
			err:  &Error{Code: "proxy", Message: "connect tcp: connection refused"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !isProxyPoolTransportFailure(tt.err) {
				t.Fatal("expected proxy transport failure")
			}
		})
	}
}

func TestProxyPoolSuccessClearsProxyFailureCount(t *testing.T) {
	leaseStore := &fakeProxyLeaseStore{}
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(leaseStore)
	cfg := proxyPoolTestConfig()
	cfg.ProxyPool.ProxyFailureThreshold = 2
	mgr.SetConfig(cfg)

	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "oauth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	mgr.MarkResult(context.Background(), Result{
		AuthID:  "oauth-1",
		Success: false,
		Error:   &Error{Message: "dial tcp: i/o timeout"},
	})
	if got := leaseStore.failures["http://proxy-a.example.com:8080"]; got != 1 {
		t.Fatalf("failures after error = %d, want 1", got)
	}
	mgr.MarkResult(context.Background(), Result{AuthID: "oauth-1", Success: true})
	if got := leaseStore.failures["http://proxy-a.example.com:8080"]; got != 0 {
		t.Fatalf("failures after success = %d, want 0", got)
	}
}

func BenchmarkProxyFailureCooldown(b *testing.B) {
	cfg := proxyPoolTestConfig()
	cfg.ProxyPool.ProxyFailureCooldown = "30m"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := proxyFailureCooldown(cfg); got != 30*time.Minute {
			b.Fatalf("cooldown = %v", got)
		}
	}
}

func BenchmarkProxyPoolTransportFailureMatch(b *testing.B) {
	err := &Error{Message: "proxyconnect tcp: dial tcp 127.0.0.1:1080: i/o timeout"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !isProxyPoolTransportFailure(err) {
			b.Fatal("expected proxy transport failure")
		}
	}
}

func BenchmarkProxyPoolTransportFailureNoMatch(b *testing.B) {
	err := &Error{
		Code:    "upstream_error",
		Message: "the upstream response ended before a valid completion was produced",
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if isProxyPoolTransportFailure(err) {
			b.Fatal("unexpected proxy transport failure")
		}
	}
}

func BenchmarkProxyPoolReconcileLargePool(b *testing.B) {
	mgr := NewManager(nil, nil, nil)
	mgr.SetProxyLeaseStore(benchmarkProxyLeaseStore{})
	cfg := proxyPoolTestConfig()
	cfg.ProxyPool.Proxies = make([]string, 128)
	for i := range cfg.ProxyPool.Proxies {
		cfg.ProxyPool.Proxies[i] = "http://proxy-" + strconv.Itoa(i) + ".example.com:8080"
	}
	mgr.runtimeConfig.Store(cfg)

	mgr.mu.Lock()
	for i := 0; i < 1000; i++ {
		authID := "oauth-" + strconv.Itoa(i)
		attrs := map[string]string{"priority": strconv.Itoa(i % 16)}
		metadata := map[string]any{
			"type":          "codex",
			"email":         "user-" + strconv.Itoa(i) + "@example.com",
			"access_token":  strings.Repeat("token", 8),
			"refresh_token": strings.Repeat("refresh", 8),
		}
		modelStates := make(map[string]*ModelState, 4)
		for modelIndex := 0; modelIndex < 4; modelIndex++ {
			modelStates["model-"+strconv.Itoa(modelIndex)] = &ModelState{
				Status:        StatusActive,
				StatusMessage: "ready",
				UpdatedAt:     time.Unix(int64(i+modelIndex), 0),
			}
		}
		if i%20 == 0 {
			attrs["api_key"] = "sk-" + strconv.Itoa(i)
			delete(metadata, "email")
			metadata["type"] = "api_key"
		}
		mgr.auths[authID] = &Auth{
			ID:          authID,
			Provider:    "codex",
			Status:      StatusActive,
			Attributes:  attrs,
			Metadata:    metadata,
			ModelStates: modelStates,
		}
	}
	mgr.mu.Unlock()

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mgr.ReconcileProxyPoolLeases(ctx)
	}
}
