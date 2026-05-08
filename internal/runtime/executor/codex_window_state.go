package executor

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codexWindowStateTTL             = 12 * time.Hour
	codexWindowStateCleanupInterval = 64
	// codexWindowStateShards controls fan-out of the session map. Using a
	// power of two keeps the shard index computation to a single mask op.
	codexWindowStateShards     = 16
	codexWindowStateShardsMask = codexWindowStateShards - 1
)

type codexWindowStateEntry struct {
	generation uint64
	lastSeen   time.Time
}

type codexWindowStateShard struct {
	mu       sync.Mutex
	sessions map[string]codexWindowStateEntry
	ops      uint64
}

// codexWindowStateStore tracks the per-session generation counter used to mint
// X-Codex-Window-Id values. It is sharded so that concurrent requests on
// distinct sessions do not all serialize on the same mutex.
type codexWindowStateStore struct {
	shards [codexWindowStateShards]codexWindowStateShard
}

var globalCodexWindowStateStore = newCodexWindowStateStore()

func newCodexWindowStateStore() *codexWindowStateStore {
	s := &codexWindowStateStore{}
	for i := range s.shards {
		s.shards[i].sessions = make(map[string]codexWindowStateEntry)
	}
	return s
}

func codexCurrentWindowID(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	generation := globalCodexWindowStateStore.currentGeneration(sessionID)
	return sessionID + ":" + strconv.FormatUint(generation, 10)
}

func codexAdvanceWindowGeneration(sessionID string) {
	globalCodexWindowStateStore.advance(sessionID)
}

func codexWindowStateKey(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if threadID := strings.TrimSpace(headers.Get(codexHeaderThreadID)); threadID != "" {
		return threadID
	}
	return strings.TrimSpace(headers.Get(codexHeaderSessionID))
}

// shardFor returns the shard that owns sessionID. The FNV-1a hash is cheap and
// gives a uniform distribution across shards for typical UUID-shaped keys.
func (s *codexWindowStateStore) shardFor(sessionID string) *codexWindowStateShard {
	const (
		fnvOffset = 14695981039346656037
		fnvPrime  = 1099511628211
	)
	hash := uint64(fnvOffset)
	for i := 0; i < len(sessionID); i++ {
		hash ^= uint64(sessionID[i])
		hash *= fnvPrime
	}
	return &s.shards[hash&codexWindowStateShardsMask]
}

func (s *codexWindowStateStore) currentGeneration(sessionID string) uint64 {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || s == nil {
		return 0
	}

	now := time.Now()
	shard := s.shardFor(sessionID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	shard.cleanupLocked(now)
	entry := shard.sessions[sessionID]
	entry.lastSeen = now
	shard.sessions[sessionID] = entry
	return entry.generation
}

func (s *codexWindowStateStore) advance(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || s == nil {
		return
	}

	now := time.Now()
	shard := s.shardFor(sessionID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	shard.cleanupLocked(now)
	entry := shard.sessions[sessionID]
	entry.generation++
	entry.lastSeen = now
	shard.sessions[sessionID] = entry
}

// reset clears every shard. Intended for tests that need a clean slate.
func (s *codexWindowStateStore) reset() {
	if s == nil {
		return
	}
	for i := range s.shards {
		shard := &s.shards[i]
		shard.mu.Lock()
		shard.sessions = make(map[string]codexWindowStateEntry)
		shard.ops = 0
		shard.mu.Unlock()
	}
}

func (s *codexWindowStateShard) cleanupLocked(now time.Time) {
	if s == nil {
		return
	}
	s.ops++
	if s.ops%codexWindowStateCleanupInterval != 0 {
		return
	}
	for sessionID, entry := range s.sessions {
		if now.Sub(entry.lastSeen) > codexWindowStateTTL {
			delete(s.sessions, sessionID)
		}
	}
}
