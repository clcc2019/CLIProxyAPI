package auth

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

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
	proxyURL := strings.TrimSpace(lease.ProxyURL)
	markProxyPoolLease(auth, proxyURL)
	var schedulerSnapshot *Auth
	scheduler := m.scheduler
	m.mu.Lock()
	if current := m.auths[auth.ID]; current != nil {
		changed := !authProxyPoolAssigned(current) || strings.TrimSpace(current.ProxyURL) != proxyURL
		markProxyPoolLease(current, proxyURL)
		if changed && scheduler != nil {
			schedulerSnapshot = current.CloneForScheduler()
		}
	}
	m.mu.Unlock()
	if scheduler != nil && schedulerSnapshot != nil {
		scheduler.upsertAuth(schedulerSnapshot)
	}
}

func (m *Manager) applyProxyPoolLeaseSnapshot(ctx context.Context, cfg *internalconfig.Config, store ProxyLeaseStore, auth proxyPoolAuthSnapshot) {
	if m == nil || store == nil || !proxyPoolCanAssignSnapshot(cfg, auth) {
		return
	}
	lease, ok, err := store.AcquireProxyLease(ctx, auth.id, cfg.ProxyPool.Proxies)
	if err != nil {
		log.WithField("auth_id", auth.id).Warnf("proxy-pool: failed to acquire proxy lease: %v", err)
		return
	}
	proxyURL := strings.TrimSpace(lease.ProxyURL)
	if !ok || proxyURL == "" {
		if auth.assigned {
			m.clearRuntimeProxyPoolLease(auth.id)
		}
		log.WithField("auth_id", auth.id).Warn("proxy-pool: no available proxy lease")
		return
	}
	var schedulerSnapshot *Auth
	scheduler := m.scheduler
	m.mu.Lock()
	if current := m.auths[auth.id]; current != nil {
		changed := !authProxyPoolAssigned(current) || strings.TrimSpace(current.ProxyURL) != proxyURL
		markProxyPoolLease(current, proxyURL)
		if changed && scheduler != nil {
			schedulerSnapshot = current.CloneForScheduler()
		}
	}
	m.mu.Unlock()
	if scheduler != nil && schedulerSnapshot != nil {
		scheduler.upsertAuth(schedulerSnapshot)
	}
}

func (m *Manager) applyProxyPoolLeaseSnapshotResult(auth proxyPoolAuthSnapshot, lease ProxyLease, ok bool) {
	if m == nil {
		return
	}
	proxyURL := strings.TrimSpace(lease.ProxyURL)
	if !ok || proxyURL == "" {
		if auth.assigned {
			m.clearRuntimeProxyPoolLease(auth.id)
		}
		log.WithField("auth_id", auth.id).Warn("proxy-pool: no available proxy lease")
		return
	}
	var schedulerSnapshot *Auth
	scheduler := m.scheduler
	m.mu.Lock()
	if current := m.auths[auth.id]; current != nil {
		changed := !authProxyPoolAssigned(current) || strings.TrimSpace(current.ProxyURL) != proxyURL
		markProxyPoolLease(current, proxyURL)
		if changed && scheduler != nil {
			schedulerSnapshot = current.CloneForScheduler()
		}
	}
	m.mu.Unlock()
	if scheduler != nil && schedulerSnapshot != nil {
		scheduler.upsertAuth(schedulerSnapshot)
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
	scheduler := m.scheduler
	m.mu.Lock()
	if current := m.auths[authID]; current != nil && authProxyPoolAssigned(current) {
		clearProxyPoolLease(current)
		if scheduler != nil {
			schedulerSnapshot = current.CloneForScheduler()
		}
	}
	m.mu.Unlock()
	if scheduler != nil && schedulerSnapshot != nil {
		scheduler.upsertAuth(schedulerSnapshot)
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

type proxyPoolAuthSnapshot struct {
	id       string
	proxyURL string
	priority int
	status   Status
	assigned bool
	disabled bool
	apiKey   bool
}

func proxyPoolAuthSnapshotFromAuth(auth *Auth) proxyPoolAuthSnapshot {
	if auth == nil {
		return proxyPoolAuthSnapshot{}
	}
	return proxyPoolAuthSnapshot{
		id:       strings.TrimSpace(auth.ID),
		proxyURL: strings.TrimSpace(auth.ProxyURL),
		priority: authPriority(auth),
		status:   auth.Status,
		assigned: authProxyPoolAssigned(auth),
		disabled: auth.Disabled,
		apiKey:   proxyPoolAuthIsAPIKey(auth),
	}
}

func proxyPoolAuthIsAPIKey(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
			return false
		}
	}
	return auth.Attributes != nil && auth.Attributes["api_key"] != ""
}

func proxyPoolCanAssignSnapshot(cfg *internalconfig.Config, auth proxyPoolAuthSnapshot) bool {
	if cfg == nil || !cfg.ProxyPool.Enabled || len(cfg.ProxyPool.Proxies) == 0 || auth.id == "" {
		return false
	}
	if auth.disabled || auth.status == StatusDisabled {
		return false
	}
	if auth.proxyURL != "" && !auth.assigned {
		return false
	}
	return !auth.apiKey
}

func proxyPoolSortAuthSnapshots(auths []proxyPoolAuthSnapshot) {
	if len(auths) < 2 {
		return
	}
	sort.Slice(auths, func(i, j int) bool {
		left := auths[i]
		right := auths[j]
		if left.priority != right.priority {
			return left.priority > right.priority
		}
		return left.id < right.id
	})
}

func proxyPoolSelectedLeaseSnapshotAuthIDs(auths []proxyPoolAuthSnapshot, limit int) []string {
	if limit <= 0 || len(auths) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(auths), limit))
	for _, auth := range auths {
		if auth.id == "" {
			continue
		}
		out = append(out, auth.id)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func proxyPoolUsableProxyCount(cfg *internalconfig.Config) int {
	if cfg == nil || len(cfg.ProxyPool.Proxies) == 0 {
		return 0
	}
	count := 0
	for _, proxyURL := range cfg.ProxyPool.Proxies {
		if strings.TrimSpace(proxyURL) != "" {
			count++
		}
	}
	return count
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

var proxyPoolTransportFailurePatterns = [...]string{
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

func isProxyPoolTransportFailure(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != 0 {
		return false
	}
	code := err.Code
	message := err.Message
	if strings.TrimSpace(code) == "" && strings.TrimSpace(message) == "" {
		return false
	}
	fold := hasUpperASCII(code) || hasUpperASCII(message)
	for _, pattern := range proxyPoolTransportFailurePatterns {
		if containsProxyPoolFailurePattern(code, message, pattern, fold) {
			return true
		}
	}
	return false
}

func containsProxyPoolFailurePattern(code, message, pattern string, fold bool) bool {
	if !fold {
		if strings.Contains(code, pattern) || strings.Contains(message, pattern) {
			return true
		}
	} else if asciifold.Contains(code, pattern) || asciifold.Contains(message, pattern) {
		return true
	}
	if code == "" || message == "" || !strings.Contains(pattern, " ") {
		return false
	}
	return containsJoinedWithSpace(code, message, pattern, fold)
}

func hasUpperASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 'A' && value[i] <= 'Z' {
			return true
		}
	}
	return false
}

func containsJoinedWithSpace(left, right, needle string, fold bool) bool {
	for split := 0; split < len(needle); split++ {
		if needle[split] != ' ' {
			continue
		}
		prefix := needle[:split]
		suffix := needle[split+1:]
		if len(prefix) > len(left) || len(suffix) > len(right) {
			continue
		}
		if hasSuffixASCIIFold(left, prefix, fold) && hasPrefixASCIIFold(right, suffix, fold) {
			return true
		}
	}
	return false
}

func hasSuffixASCIIFold(value, suffix string, fold bool) bool {
	if !fold {
		return strings.HasSuffix(value, suffix)
	}
	if len(suffix) > len(value) {
		return false
	}
	return asciifold.HasSuffix(value, suffix)
}

func hasPrefixASCIIFold(value, prefix string, fold bool) bool {
	if !fold {
		return strings.HasPrefix(value, prefix)
	}
	if len(prefix) > len(value) {
		return false
	}
	return asciifold.HasPrefix(value, prefix)
}

func proxyPoolCanAssign(cfg *internalconfig.Config, auth *Auth) bool {
	if auth == nil {
		return false
	}
	return proxyPoolCanAssignSnapshot(cfg, proxyPoolAuthSnapshot{
		id:       strings.TrimSpace(auth.ID),
		proxyURL: strings.TrimSpace(auth.ProxyURL),
		status:   auth.Status,
		assigned: authProxyPoolAssigned(auth),
		disabled: auth.Disabled,
		apiKey:   proxyPoolAuthIsAPIKey(auth),
	})
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
