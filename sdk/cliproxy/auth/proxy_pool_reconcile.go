package auth

import (
	"context"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

type proxyPoolRecoveryTimer struct {
	timer     *time.Timer
	recoverAt time.Time
	sequence  uint64
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
	m.clearPendingProxyPoolReconcile()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	m.mu.RLock()
	store := m.proxyLeaseStore
	activeAuthIDs := make([]string, 0, len(m.auths))
	var assignedAuthIDs []string
	var assignableAuths []proxyPoolAuthSnapshot
	usableProxyCount := proxyPoolUsableProxyCount(cfg)
	collectAssignable := cfg != nil && cfg.ProxyPool.Enabled && usableProxyCount > 0
	if collectAssignable {
		assignedAuthIDs = make([]string, 0, min(len(m.auths), usableProxyCount))
		assignableAuths = make([]proxyPoolAuthSnapshot, 0, len(m.auths))
	}
	for _, auth := range m.auths {
		if auth != nil {
			if !collectAssignable {
				authID := strings.TrimSpace(auth.ID)
				if authID != "" {
					activeAuthIDs = append(activeAuthIDs, authID)
					if authProxyPoolAssigned(auth) {
						assignedAuthIDs = append(assignedAuthIDs, authID)
					}
				}
				continue
			}
			snapshot := proxyPoolAuthSnapshotFromAuth(auth)
			if snapshot.id != "" {
				activeAuthIDs = append(activeAuthIDs, snapshot.id)
			}
			if snapshot.assigned && snapshot.id != "" {
				assignedAuthIDs = append(assignedAuthIDs, snapshot.id)
			}
			if collectAssignable && proxyPoolCanAssignSnapshot(cfg, snapshot) {
				assignableAuths = append(assignableAuths, snapshot)
			}
		}
	}
	m.mu.RUnlock()
	proxyPoolSortAuthSnapshots(assignableAuths)
	leaseAuthIDs := activeAuthIDs
	var selectedLeaseAuthIDs map[string]struct{}
	if cfg != nil && cfg.ProxyPool.Enabled {
		leaseAuthIDs = proxyPoolSelectedLeaseSnapshotAuthIDs(assignableAuths, usableProxyCount)
		selectedLeaseAuthIDs = make(map[string]struct{}, len(leaseAuthIDs))
		for _, authID := range leaseAuthIDs {
			selectedLeaseAuthIDs[authID] = struct{}{}
		}
	}
	reconciledProxyLeases := false
	if store != nil {
		proxyURLs := []string(nil)
		if cfg != nil && cfg.ProxyPool.Enabled {
			proxyURLs = cfg.ProxyPool.Proxies
		}
		shouldReconcile := len(leaseAuthIDs) > 0
		if cfg != nil {
			shouldReconcile = shouldReconcile ||
				(cfg.ProxyPool.Enabled && len(activeAuthIDs) > 0) ||
				!cfg.ProxyPool.Enabled
		}
		if shouldReconcile {
			if err := store.ReconcileProxyLeases(ctx, leaseAuthIDs, proxyURLs); err != nil {
				log.Warnf("proxy-pool: failed to reconcile proxy leases: %v", err)
			} else {
				reconciledProxyLeases = true
			}
		}
	}
	for _, authID := range assignedAuthIDs {
		if _, selected := selectedLeaseAuthIDs[authID]; !selected {
			if reconciledProxyLeases {
				m.clearRuntimeProxyPoolLease(authID)
				continue
			}
			m.releaseProxyLease(ctx, authID)
		}
	}
	m.applyProxyPoolLeaseSnapshots(ctx, cfg, store, assignableAuths, selectedLeaseAuthIDs, leaseAuthIDs)
}

func (m *Manager) applyProxyPoolLeaseSnapshots(ctx context.Context, cfg *internalconfig.Config, store ProxyLeaseStore, auths []proxyPoolAuthSnapshot, selected map[string]struct{}, selectedAuthIDs []string) {
	if m == nil || store == nil || cfg == nil || len(auths) == 0 {
		return
	}
	if len(selectedAuthIDs) == 0 {
		return
	}
	if batchStore, ok := store.(ProxyLeaseBatchStore); ok {
		leases, err := batchStore.AcquireProxyLeases(ctx, selectedAuthIDs, cfg.ProxyPool.Proxies)
		if err != nil {
			log.Warnf("proxy-pool: failed to acquire proxy leases in batch: %v", err)
			return
		}
		leaseIndex := 0
		for _, auth := range auths {
			if _, ok := selected[auth.id]; !ok {
				continue
			}
			var lease ProxyLease
			if leaseIndex < len(leases) {
				lease = leases[leaseIndex]
			}
			leaseIndex++
			leaseAuthID := strings.TrimSpace(lease.AuthID)
			ok := strings.TrimSpace(lease.ProxyURL) != "" && (leaseAuthID == "" || leaseAuthID == auth.id)
			m.applyProxyPoolLeaseSnapshotResult(cfg, auth, lease, ok)
		}
		return
	}
	for _, auth := range auths {
		if _, ok := selected[auth.id]; !ok {
			continue
		}
		m.applyProxyPoolLeaseSnapshot(ctx, cfg, store, auth)
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
	m.enqueueProxyPoolReconcile(ctx)
}

func (m *Manager) enqueueProxyPoolReconcile(ctx context.Context) {
	if m == nil {
		return
	}
	m.proxyReconcileMu.Lock()
	if m.proxyReconcileStopping {
		m.proxyReconcileMu.Unlock()
		return
	}
	m.proxyReconcilePending = true
	if m.proxyReconcileWake == nil || m.proxyReconcileCancel == nil {
		m.startProxyPoolReconcileLoopLocked()
	}
	wake := m.proxyReconcileWake
	m.proxyReconcileMu.Unlock()

	select {
	case wake <- struct{}{}:
	default:
	}
}

func (m *Manager) clearPendingProxyPoolReconcile() {
	if m == nil {
		return
	}
	m.proxyReconcileMu.Lock()
	m.proxyReconcilePending = false
	m.proxyReconcileMu.Unlock()
}

func (m *Manager) startProxyPoolReconcileLoopLocked() {
	if m == nil {
		return
	}
	if m.proxyReconcileStopping {
		return
	}
	if m.proxyReconcileWake != nil && m.proxyReconcileCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	m.proxyReconcileWake = wake
	m.proxyReconcileCancel = cancel
	m.proxyReconcileWG.Add(1)
	go m.runProxyPoolReconcileLoop(ctx, wake)
}

func (m *Manager) stopProxyPoolReconcileLoop() {
	if m == nil {
		return
	}
	m.proxyReconcileMu.Lock()
	if m.proxyReconcileStopping {
		done := m.proxyReconcileStopDone
		m.proxyReconcileMu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	m.proxyReconcileStopping = true
	done := make(chan struct{})
	m.proxyReconcileStopDone = done
	for proxyURL, recovery := range m.proxyRecoveryTimers {
		if recovery != nil && recovery.timer != nil && recovery.timer.Stop() {
			m.proxyRecoveryWG.Done()
		}
		delete(m.proxyRecoveryTimers, proxyURL)
	}
	cancel := m.proxyReconcileCancel
	m.proxyReconcileCancel = nil
	m.proxyReconcileWake = nil
	m.proxyReconcileMu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.proxyRecoveryWG.Wait()
	m.proxyReconcileWG.Wait()

	m.proxyReconcileMu.Lock()
	m.proxyReconcileStopping = false
	if m.proxyReconcileStopDone == done {
		m.proxyReconcileStopDone = nil
		close(done)
	}
	m.proxyReconcileMu.Unlock()
}

func (m *Manager) scheduleProxyPoolRecoveryReconcile(proxyURL string, recoverAt time.Time) {
	if m == nil {
		return
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return
	}
	delay := time.Until(recoverAt)
	if recoverAt.IsZero() || delay <= 0 {
		m.enqueueProxyPoolReconcile(context.Background())
		return
	}

	m.proxyReconcileMu.Lock()
	if m.proxyReconcileStopping {
		m.proxyReconcileMu.Unlock()
		return
	}
	if m.proxyRecoveryTimers == nil {
		m.proxyRecoveryTimers = make(map[string]*proxyPoolRecoveryTimer)
	}
	if current := m.proxyRecoveryTimers[proxyURL]; current != nil {
		if current.recoverAt.Equal(recoverAt) {
			m.proxyReconcileMu.Unlock()
			return
		}
		if current.timer != nil && current.timer.Stop() {
			m.proxyRecoveryWG.Done()
		}
	}
	m.proxyRecoverySequence++
	sequence := m.proxyRecoverySequence
	recovery := &proxyPoolRecoveryTimer{recoverAt: recoverAt, sequence: sequence}
	m.proxyRecoveryWG.Add(1)
	recovery.timer = time.AfterFunc(delay, func() {
		defer m.proxyRecoveryWG.Done()
		m.proxyReconcileMu.Lock()
		current := m.proxyRecoveryTimers[proxyURL]
		if m.proxyReconcileStopping || current == nil || current.sequence != sequence {
			m.proxyReconcileMu.Unlock()
			return
		}
		delete(m.proxyRecoveryTimers, proxyURL)
		m.proxyReconcileMu.Unlock()
		m.enqueueProxyPoolReconcile(context.Background())
	})
	m.proxyRecoveryTimers[proxyURL] = recovery
	m.proxyReconcileMu.Unlock()
}

func (m *Manager) runProxyPoolReconcileLoop(ctx context.Context, wake chan struct{}) {
	defer m.proxyReconcileWG.Done()

	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)

	resetTimer := func() {
		if timer == nil {
			timer = time.NewTimer(proxyPoolReconcileDebounceWindow)
			timerC = timer.C
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(proxyPoolReconcileDebounceWindow)
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
			m.flushProxyPoolReconcileQueue(context.Background())
			return
		case <-wake:
			resetTimer()
		case <-timerC:
			stopTimer()
			m.flushProxyPoolReconcileQueue(context.Background())
			if m.stopProxyPoolReconcileLoopIfIdle(wake) {
				return
			}
		}
	}
}

func (m *Manager) stopProxyPoolReconcileLoopIfIdle(wake chan struct{}) bool {
	if m == nil {
		return true
	}
	m.proxyReconcileMu.Lock()
	if m.proxyReconcileWake != wake || m.proxyReconcilePending {
		m.proxyReconcileMu.Unlock()
		return false
	}
	cancel := m.proxyReconcileCancel
	m.proxyReconcileWake = nil
	m.proxyReconcileCancel = nil
	m.proxyReconcileMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (m *Manager) flushProxyPoolReconcileQueue(ctx context.Context) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.proxyReconcileMu.Lock()
	if !m.proxyReconcilePending {
		m.proxyReconcileMu.Unlock()
		return
	}
	m.proxyReconcilePending = false
	m.proxyReconcileMu.Unlock()
	m.ReconcileProxyPoolLeases(ctx)
}
