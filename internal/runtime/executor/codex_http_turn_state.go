package executor

import (
	"container/list"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	codexHTTPTurnStateTTL        = 2 * time.Hour
	codexHTTPTurnStateMaxEntries = 4096
)

type codexHTTPTurnStateEntry struct {
	state    string
	scope    string
	lastSeen time.Time
	element  *list.Element
}

type codexHTTPTurnStateStore struct {
	mu         sync.Mutex
	entries    map[string]*codexHTTPTurnStateEntry
	recency    *list.List // newest key at the front; protected by mu
	maxEntries int
}

func newCodexHTTPTurnStateStore() *codexHTTPTurnStateStore {
	return newCodexHTTPTurnStateStoreWithLimit(codexHTTPTurnStateMaxEntries)
}

func newCodexHTTPTurnStateStoreWithLimit(maxEntries int) *codexHTTPTurnStateStore {
	if maxEntries <= 0 {
		maxEntries = codexHTTPTurnStateMaxEntries
	}
	return &codexHTTPTurnStateStore{
		entries:    make(map[string]*codexHTTPTurnStateEntry),
		recency:    list.New(),
		maxEntries: maxEntries,
	}
}

func (e *CodexExecutor) applyCodexHTTPTurnState(auth *cliproxyauth.Auth, executionSessionID string, headers http.Header) {
	if e == nil || e.httpTurnState == nil || headers == nil {
		return
	}
	if strings.TrimSpace(headers.Get(codexHeaderTurnState)) != "" {
		return
	}
	key := e.codexHTTPTurnStateKey(auth, executionSessionID)
	if key == "" {
		return
	}
	scope := codexHTTPTurnStateScope(headers.Get(codexHeaderTurnMetadata))
	if scope == "" {
		return
	}
	if state := e.httpTurnState.get(key, scope, time.Now()); state != "" {
		headers.Set(codexHeaderTurnState, state)
	}
}

func (e *CodexExecutor) rememberCodexHTTPTurnState(auth *cliproxyauth.Auth, prepared codexPreparedRequest, responseHeaders http.Header) {
	if e == nil || e.httpTurnState == nil || prepared.httpReq == nil || responseHeaders == nil {
		return
	}
	if prepared.httpReq.URL == nil || codexFinalUpstreamRequestKindForURL(prepared.httpReq.URL.String()) != codexFinalUpstreamResponses {
		return
	}
	state := strings.TrimSpace(responseHeaders.Get(codexHeaderTurnState))
	if state == "" {
		return
	}
	key := e.codexHTTPTurnStateKey(auth, prepared.executionSessionID)
	if key == "" {
		return
	}
	scope := codexHTTPTurnStateScope(prepared.httpReq.Header.Get(codexHeaderTurnMetadata))
	if scope == "" {
		return
	}
	e.httpTurnState.put(key, scope, state, time.Now())
	if strings.TrimSpace(prepared.httpReq.Header.Get(codexHeaderTurnState)) == "" {
		prepared.httpReq.Header.Set(codexHeaderTurnState, state)
	}
}

func (e *CodexExecutor) forgetCodexHTTPTurnState(auth *cliproxyauth.Auth, prepared codexPreparedRequest) {
	if e == nil || e.httpTurnState == nil || prepared.httpReq == nil {
		return
	}
	if prepared.httpReq.URL == nil || codexFinalUpstreamRequestKindForURL(prepared.httpReq.URL.String()) != codexFinalUpstreamResponses {
		return
	}
	state := strings.TrimSpace(prepared.httpReq.Header.Get(codexHeaderTurnState))
	if state == "" {
		return
	}
	key := e.codexHTTPTurnStateKey(auth, prepared.executionSessionID)
	if key == "" {
		return
	}
	scope := codexHTTPTurnStateScope(prepared.httpReq.Header.Get(codexHeaderTurnMetadata))
	if scope == "" {
		return
	}
	e.httpTurnState.delete(key, scope, state)
}

func (e *CodexExecutor) CloseExecutionSession(sessionID string) {
	e.clearCodexHTTPTurnStateSession(sessionID)
}

func (e *CodexExecutor) ResetExecutionSession(sessionID string) {
	e.clearCodexHTTPTurnStateSession(sessionID)
}

func (e *CodexExecutor) ResetAuthContinuity(authID string) {
	if e == nil {
		return
	}
	codexResetClientProfileForAuthID(authID)
	if e.httpTurnState != nil {
		e.httpTurnState.deleteAuth(authID)
	}
}

func (e *CodexExecutor) clearCodexHTTPTurnStateSession(sessionID string) {
	if e == nil || e.httpTurnState == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		e.httpTurnState.clear()
		return
	}
	e.httpTurnState.deleteExecutionSession(sessionID)
}

func (e *CodexExecutor) codexHTTPTurnStateKey(auth *cliproxyauth.Auth, executionSessionID string) string {
	executionSessionID = strings.TrimSpace(executionSessionID)
	if executionSessionID == "" {
		return ""
	}
	return e.codexResponseDedupeScope(auth) + "|session:" + executionSessionID
}

func codexHTTPTurnStateScope(rawMetadata string) string {
	rawMetadata = strings.TrimSpace(rawMetadata)
	if rawMetadata == "" {
		return ""
	}
	if !gjson.Valid(rawMetadata) {
		return rawMetadata
	}
	metadata := gjson.Parse(rawMetadata)
	if !metadata.IsObject() {
		return rawMetadata
	}
	fields := []string{
		codexRequestKindMetadataPath,
		"session_id",
		"thread_id",
		"forked_from_thread_id",
		"parent_thread_id",
		"subagent_kind",
		"thread_source",
		"turn_id",
		"sandbox",
		codexWindowIDMetadataPath,
	}
	var builder strings.Builder
	for _, field := range fields {
		value := strings.TrimSpace(metadata.Get(field).String())
		if value == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('|')
		}
		builder.WriteString(field)
		builder.WriteByte('=')
		builder.WriteString(value)
	}
	if builder.Len() == 0 {
		return rawMetadata
	}
	return builder.String()
}

func (s *codexHTTPTurnStateStore) get(key string, scope string, now time.Time) string {
	key = strings.TrimSpace(key)
	scope = strings.TrimSpace(scope)
	if s == nil || key == "" || scope == "" {
		return ""
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	entry, ok := s.entries[key]
	if !ok || entry == nil || entry.scope != scope {
		return ""
	}
	if now.Sub(entry.lastSeen) > codexHTTPTurnStateTTL {
		s.removeEntryLocked(key, entry)
		return ""
	}
	entry.lastSeen = now
	if entry.element != nil {
		s.recency.MoveToFront(entry.element)
	}
	return entry.state
}

func (s *codexHTTPTurnStateStore) put(key string, scope string, state string, now time.Time) {
	key = strings.TrimSpace(key)
	scope = strings.TrimSpace(scope)
	state = strings.TrimSpace(state)
	if s == nil || key == "" || scope == "" || state == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	if existing := s.entries[key]; existing != nil {
		existing.state = state
		existing.scope = scope
		existing.lastSeen = now
		if existing.element != nil {
			s.recency.MoveToFront(existing.element)
		}
		return
	}
	for len(s.entries) >= s.maxEntries {
		s.removeOldestLocked()
	}
	entry := &codexHTTPTurnStateEntry{
		state:    state,
		scope:    scope,
		lastSeen: now,
	}
	entry.element = s.recency.PushFront(key)
	s.entries[key] = entry
}

func (s *codexHTTPTurnStateStore) delete(key string, scope string, state string) {
	key = strings.TrimSpace(key)
	scope = strings.TrimSpace(scope)
	state = strings.TrimSpace(state)
	if s == nil || key == "" || scope == "" || state == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || entry == nil || entry.scope != scope || entry.state != state {
		return
	}
	s.removeEntryLocked(key, entry)
}

func (s *codexHTTPTurnStateStore) deleteExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if s == nil || sessionID == "" {
		return
	}
	suffix := "|session:" + sessionID
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if strings.HasSuffix(key, suffix) {
			s.removeEntryLocked(key, entry)
		}
	}
}

func (s *codexHTTPTurnStateStore) deleteAuth(authID string) {
	authID = strings.TrimSpace(authID)
	if s == nil || authID == "" {
		return
	}
	prefix := "id:" + authID + "|"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if strings.HasPrefix(key, prefix) {
			s.removeEntryLocked(key, entry)
		}
	}
}

func (s *codexHTTPTurnStateStore) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.entries = make(map[string]*codexHTTPTurnStateEntry)
	s.recency.Init()
	s.mu.Unlock()
}

func (s *codexHTTPTurnStateStore) cleanupLocked(now time.Time) {
	if s == nil || s.recency == nil {
		return
	}
	for {
		oldest := s.recency.Back()
		if oldest == nil {
			return
		}
		key, _ := oldest.Value.(string)
		entry := s.entries[key]
		if entry == nil {
			s.recency.Remove(oldest)
			continue
		}
		if now.Sub(entry.lastSeen) <= codexHTTPTurnStateTTL {
			return
		}
		s.removeEntryLocked(key, entry)
	}
}

func (s *codexHTTPTurnStateStore) removeOldestLocked() {
	if s == nil || s.recency == nil {
		return
	}
	oldest := s.recency.Back()
	if oldest == nil {
		return
	}
	key, _ := oldest.Value.(string)
	entry := s.entries[key]
	if entry == nil {
		s.recency.Remove(oldest)
		return
	}
	s.removeEntryLocked(key, entry)
}

func (s *codexHTTPTurnStateStore) removeEntryLocked(key string, entry *codexHTTPTurnStateEntry) {
	if s == nil || entry == nil {
		return
	}
	delete(s.entries, key)
	if s.recency != nil && entry.element != nil {
		s.recency.Remove(entry.element)
		entry.element = nil
	}
}
