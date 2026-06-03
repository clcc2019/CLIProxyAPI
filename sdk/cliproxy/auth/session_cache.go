package auth

import (
	"sync"
	"time"
)

// sessionEntry stores auth binding with expiration.
type sessionEntry struct {
	authID    string
	expiresAt time.Time
}

// SessionCache provides TTL-based session to auth mapping with automatic cleanup.
type SessionCache struct {
	mu       sync.RWMutex
	entries  map[string]sessionEntry
	forceNew map[string]time.Time
	ttl      time.Duration
	stopCh   chan struct{}
}

// NewSessionCache creates a cache with the specified TTL.
// A background goroutine periodically cleans expired entries.
func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	c := &SessionCache{
		entries:  make(map[string]sessionEntry),
		forceNew: make(map[string]time.Time),
		ttl:      ttl,
		stopCh:   make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

// Get retrieves the auth ID bound to a session, if still valid.
// Does NOT refresh the TTL on access. Expired entries are not eagerly deleted
// on read — cleanupLoop handles removal to keep the hot path on the read lock.
func (c *SessionCache) Get(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.authID, true
}

// GetAndRefresh retrieves the auth ID bound to a session and refreshes TTL on hit.
// This extends the binding lifetime for active sessions.
func (c *SessionCache) GetAndRefresh(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	entry, ok := c.entries[sessionID]
	if !ok {
		c.mu.Unlock()
		return "", false
	}
	if now.After(entry.expiresAt) {
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", false
	}
	// Refresh TTL on successful access
	entry.expiresAt = now.Add(c.ttl)
	c.entries[sessionID] = entry
	c.mu.Unlock()
	return entry.authID, true
}

// Set binds a session to an auth ID with TTL refresh.
func (c *SessionCache) Set(sessionID, authID string) {
	if sessionID == "" || authID == "" {
		return
	}
	c.mu.Lock()
	c.entries[sessionID] = sessionEntry{
		authID:    authID,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// Invalidate removes a specific session binding.
func (c *SessionCache) Invalidate(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	delete(c.entries, sessionID)
	c.mu.Unlock()
}

// InvalidateAuth removes all sessions bound to a specific auth ID.
// Used when an auth becomes unavailable.
func (c *SessionCache) InvalidateAuth(authID string) {
	if authID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	if c.forceNew == nil {
		c.forceNew = make(map[string]time.Time)
	}
	for sid, entry := range c.entries {
		if entry.authID == authID {
			delete(c.entries, sid)
			if entry.expiresAt.After(now) {
				c.forceNew[sid] = entry.expiresAt
			}
		}
	}
	c.mu.Unlock()
}

// ConsumeForceNew reports whether the session was recently unbound from an
// unavailable auth and clears the marker. Callers use this to start a fresh
// upstream provider session when the same downstream session is rebound to a
// different credential.
func (c *SessionCache) ConsumeForceNew(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	now := time.Now()
	c.mu.Lock()
	expiresAt, ok := c.forceNew[sessionID]
	if ok {
		delete(c.forceNew, sessionID)
	}
	c.mu.Unlock()
	return ok && expiresAt.After(now)
}

// ForceNewPending reports whether a still-valid fresh-upstream marker exists
// without clearing it. Callers should clear the marker only after successfully
// rebinding the downstream session to a credential.
func (c *SessionCache) ForceNewPending(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	now := time.Now()
	c.mu.RLock()
	expiresAt, ok := c.forceNew[sessionID]
	c.mu.RUnlock()
	return ok && expiresAt.After(now)
}

func consumeForceNewMarkers(cache *SessionCache, sessionIDs ...string) {
	if cache == nil {
		return
	}
	for _, sessionID := range sessionIDs {
		cache.ConsumeForceNew(sessionID)
	}
}

// Stop terminates the background cleanup goroutine.
func (c *SessionCache) Stop() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
}

func (c *SessionCache) cleanupLoop() {
	ticker := time.NewTicker(c.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

func (c *SessionCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	for sid, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, sid)
		}
	}
	for sid, expiresAt := range c.forceNew {
		if now.After(expiresAt) {
			delete(c.forceNew, sid)
		}
	}
	c.mu.Unlock()
}
