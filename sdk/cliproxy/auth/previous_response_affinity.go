package auth

import (
	"bytes"
	"container/list"
	"context"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	previousResponseAuthTTL        = 2 * time.Hour
	previousResponseAuthMaxEntries = 65536
	previousResponseStoreTimeout   = 500 * time.Millisecond
)

type previousResponseAuthEntry struct {
	authID    string
	expiresAt time.Time
	element   *list.Element
}

type previousResponseAuthCache struct {
	mu          sync.RWMutex
	entries     map[string]previousResponseAuthEntry
	invalidated map[string]time.Time
	order       *list.List
	ttl         time.Duration
	maxEntries  int
	lastCleanup time.Time
}

func newPreviousResponseAuthCache(ttl time.Duration, maxEntries int) *previousResponseAuthCache {
	if ttl <= 0 {
		ttl = previousResponseAuthTTL
	}
	if maxEntries <= 0 {
		maxEntries = previousResponseAuthMaxEntries
	}
	return &previousResponseAuthCache{
		entries:     make(map[string]previousResponseAuthEntry),
		invalidated: make(map[string]time.Time),
		order:       list.New(),
		ttl:         ttl,
		maxEntries:  maxEntries,
	}
}

func (c *previousResponseAuthCache) Configure(ttl time.Duration, maxEntries int) {
	if c == nil {
		return
	}
	if ttl <= 0 {
		ttl = previousResponseAuthTTL
	}
	if maxEntries <= 0 {
		maxEntries = previousResponseAuthMaxEntries
	}
	c.mu.Lock()
	c.ttl = ttl
	c.maxEntries = maxEntries
	c.cleanupExpiredLocked(time.Now())
	c.evictOverLimitLocked()
	c.mu.Unlock()
}

func (c *previousResponseAuthCache) TTL() time.Duration {
	if c == nil {
		return previousResponseAuthTTL
	}
	c.mu.RLock()
	ttl := c.ttl
	c.mu.RUnlock()
	if ttl <= 0 {
		return previousResponseAuthTTL
	}
	return ttl
}

func (c *previousResponseAuthCache) GetAndRefresh(responseID string) (string, bool) {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	entry, ok := c.entries[responseID]
	if !ok {
		c.mu.Unlock()
		return "", false
	}
	if now.After(entry.expiresAt) {
		c.deleteLocked(responseID)
		c.mu.Unlock()
		return "", false
	}
	entry.expiresAt = now.Add(c.ttl)
	if entry.element != nil {
		c.order.MoveToBack(entry.element)
	}
	c.entries[responseID] = entry
	c.mu.Unlock()
	return entry.authID, true
}

func (c *previousResponseAuthCache) Set(responseID, authID string) {
	responseID = strings.TrimSpace(responseID)
	authID = strings.TrimSpace(authID)
	if c == nil || responseID == "" || authID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	delete(c.invalidated, responseID)
	c.cleanupExpiredLocked(now)
	if entry, exists := c.entries[responseID]; exists {
		entry.authID = authID
		entry.expiresAt = now.Add(c.ttl)
		if entry.element != nil {
			c.order.MoveToBack(entry.element)
		}
		c.entries[responseID] = entry
		c.mu.Unlock()
		return
	}
	if len(c.entries) >= c.maxEntries {
		c.evictOldestLocked()
	}
	element := c.order.PushBack(responseID)
	c.entries[responseID] = previousResponseAuthEntry{
		authID:    authID,
		expiresAt: now.Add(c.ttl),
		element:   element,
	}
	c.mu.Unlock()
}

func (c *previousResponseAuthCache) Invalidate(responseID string) {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" {
		return
	}
	c.mu.Lock()
	c.deleteLocked(responseID)
	c.mu.Unlock()
}

func (c *previousResponseAuthCache) InvalidateAuth(authID string) {
	authID = strings.TrimSpace(authID)
	if c == nil || authID == "" {
		return
	}
	c.mu.Lock()
	if c.invalidated == nil {
		c.invalidated = make(map[string]time.Time)
	}
	for responseID, entry := range c.entries {
		if entry.authID == authID {
			c.invalidated[responseID] = entry.expiresAt
			c.deleteLocked(responseID)
		}
	}
	c.mu.Unlock()
}

func (c *previousResponseAuthCache) IsInvalidated(responseID string) bool {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" {
		return false
	}
	now := time.Now()
	c.mu.Lock()
	expiresAt, ok := c.invalidated[responseID]
	if ok && !now.Before(expiresAt) {
		delete(c.invalidated, responseID)
		ok = false
	}
	c.mu.Unlock()
	return ok
}

func (c *previousResponseAuthCache) cleanupExpiredLocked(now time.Time) {
	if c == nil {
		return
	}
	interval := c.ttl / 2
	if interval <= 0 {
		interval = time.Hour
	}
	if !c.lastCleanup.IsZero() && now.Sub(c.lastCleanup) < interval {
		return
	}
	for {
		oldest := c.order.Front()
		if oldest == nil {
			break
		}
		responseID, _ := oldest.Value.(string)
		entry, ok := c.entries[responseID]
		if !ok {
			c.order.Remove(oldest)
			continue
		}
		if !now.After(entry.expiresAt) {
			break
		}
		c.deleteLocked(responseID)
	}
	for responseID, expiresAt := range c.invalidated {
		if !now.Before(expiresAt) {
			delete(c.invalidated, responseID)
		}
	}
	c.lastCleanup = now
}

func (c *previousResponseAuthCache) evictOverLimitLocked() {
	if c == nil || c.maxEntries <= 0 {
		return
	}
	for len(c.entries) > c.maxEntries {
		if !c.evictOldestLocked() {
			return
		}
	}
}

func (c *previousResponseAuthCache) evictOldestLocked() bool {
	oldest := c.order.Front()
	if oldest == nil {
		return false
	}
	responseID, _ := oldest.Value.(string)
	if responseID == "" {
		c.order.Remove(oldest)
		return true
	}
	c.deleteLocked(responseID)
	return true
}

func (c *previousResponseAuthCache) deleteLocked(responseID string) {
	entry, ok := c.entries[responseID]
	if !ok {
		return
	}
	if entry.element != nil {
		c.order.Remove(entry.element)
	}
	delete(c.entries, responseID)
}

func previousResponseIDFromExecution(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	for _, body := range [][]byte{req.Payload, opts.OriginalRequest} {
		if id := previousResponseIDFromJSON(body); id != "" {
			return id
		}
	}
	return ""
}

func previousResponseIDFromJSON(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
}

func responseIDFromProviderPayload(payload []byte) string {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return ""
	}
	if bytes.HasPrefix(payload, []byte("data:")) || bytes.Contains(payload, []byte("\ndata:")) {
		return responseIDFromSSEPayload(payload)
	}
	return responseIDFromJSONPayload(payload)
}

func responseIDFromSSEPayload(payload []byte) string {
	lines := bytes.Split(payload, []byte{'\n'})
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if id := responseIDFromJSONPayload(data); id != "" {
			return id
		}
	}
	return ""
}

func responseIDFromJSONPayload(payload []byte) string {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType != "" && eventType != "response.completed" && eventType != "response.done" {
		return ""
	}
	for _, path := range []string{"response.id", "id"} {
		if id := strings.TrimSpace(gjson.GetBytes(payload, path).String()); id != "" {
			return id
		}
	}
	return ""
}

func (m *Manager) bindPreviousResponseFromPayload(ctx context.Context, authID string, payload []byte) {
	if m == nil {
		return
	}
	if !m.executionAuthPrincipalMatchesID(ctx, authID) {
		return
	}
	m.bindPreviousResponseID(ctx, responseIDFromProviderPayload(payload), authID)
}

func (m *Manager) bindPreviousResponseID(ctx context.Context, responseID, authID string) {
	if m == nil || m.previousResponseAuths == nil {
		return
	}
	responseID = strings.TrimSpace(responseID)
	authID = strings.TrimSpace(authID)
	if responseID == "" || authID == "" {
		return
	}
	m.previousResponseAuths.Set(responseID, authID)
	store := m.previousResponseStoreSnapshot()
	if store == nil {
		return
	}
	storeCtx, cancel := previousResponseStoreContext(ctx)
	defer cancel()
	if err := store.SetPreviousResponseAuth(storeCtx, responseID, authID, m.previousResponseAuths.TTL()); err != nil {
		log.WithError(err).Debug("previous-response affinity: failed to persist binding")
	}
}

func (m *Manager) previousResponsePinnedAuthID(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (string, string, bool) {
	if m == nil || m.previousResponseAuths == nil {
		return "", "", false
	}
	responseID := previousResponseIDFromExecution(req, opts)
	if responseID == "" {
		return "", "", false
	}
	if m.previousResponseAuths.IsInvalidated(responseID) {
		return responseID, "", true
	}
	authID, ok := m.previousResponseAuths.GetAndRefresh(responseID)
	if ok {
		return responseID, authID, false
	}
	store := m.previousResponseStoreSnapshot()
	if store == nil {
		return responseID, "", false
	}
	storeCtx, cancel := previousResponseStoreContext(ctx)
	defer cancel()
	authID, ok, err := store.GetPreviousResponseAuth(storeCtx, responseID, m.previousResponseAuths.TTL())
	if err != nil {
		log.WithError(err).Debug("previous-response affinity: failed to load binding")
		return responseID, "", false
	}
	authID = strings.TrimSpace(authID)
	if !ok || authID == "" {
		return responseID, "", false
	}
	m.previousResponseAuths.Set(responseID, authID)
	return responseID, authID, false
}

func (m *Manager) invalidatePreviousResponseID(ctx context.Context, responseID string) {
	if m == nil {
		return
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return
	}
	if m.previousResponseAuths != nil {
		m.previousResponseAuths.Invalidate(responseID)
	}
	store := m.previousResponseStoreSnapshot()
	if store == nil {
		return
	}
	storeCtx, cancel := previousResponseStoreContext(ctx)
	defer cancel()
	if err := store.DeletePreviousResponseAuth(storeCtx, responseID); err != nil {
		log.WithError(err).Debug("previous-response affinity: failed to delete binding")
	}
}

func (m *Manager) invalidatePreviousResponsesForAuth(ctx context.Context, authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if m.previousResponseAuths != nil {
		m.previousResponseAuths.InvalidateAuth(authID)
	}
	m.invalidatePreviousResponseStoreForAuth(ctx, authID)
}

func (m *Manager) invalidatePreviousResponseStoreForAuth(ctx context.Context, authID string) {
	store, ok := m.previousResponseStoreSnapshot().(PreviousResponseAuthInvalidator)
	if !ok || store == nil {
		return
	}
	storeCtx, cancel := previousResponseStoreContext(ctx)
	defer cancel()
	if err := store.DeletePreviousResponseAuthByAuthID(storeCtx, authID); err != nil {
		log.WithError(err).Debug("previous-response affinity: failed to delete auth bindings")
	}
}

func (m *Manager) previousResponseStoreSnapshot() PreviousResponseStore {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	store := m.previousResponseStore
	m.mu.RUnlock()
	return store
}

func previousResponseStoreContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), previousResponseStoreTimeout)
}

func (m *Manager) configurePreviousResponseAffinity(cfg *internalconfig.Config) {
	if m == nil || m.previousResponseAuths == nil {
		return
	}
	ttl := previousResponseAuthTTL
	maxEntries := previousResponseAuthMaxEntries
	if cfg != nil {
		if raw := strings.TrimSpace(cfg.Routing.PreviousResponseAffinityTTL); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil || parsed <= 0 {
				log.Warnf("invalid routing.previous-response-affinity-ttl %q; using %s", raw, previousResponseAuthTTL)
			} else {
				ttl = parsed
			}
		}
		if cfg.Routing.PreviousResponseAffinityMaxEntries > 0 {
			maxEntries = cfg.Routing.PreviousResponseAffinityMaxEntries
		}
	}
	m.previousResponseAuths.Configure(ttl, maxEntries)
}
