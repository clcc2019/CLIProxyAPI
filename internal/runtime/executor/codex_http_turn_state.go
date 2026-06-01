package executor

import (
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	codexHTTPTurnStateTTL             = 2 * time.Hour
	codexHTTPTurnStateCleanupInterval = 64
)

type codexHTTPTurnStateEntry struct {
	state    string
	scope    string
	lastSeen time.Time
}

type codexHTTPTurnStateStore struct {
	mu         sync.Mutex
	entries    map[string]codexHTTPTurnStateEntry
	cleanupOps uint64
}

func newCodexHTTPTurnStateStore() *codexHTTPTurnStateStore {
	return &codexHTTPTurnStateStore{entries: make(map[string]codexHTTPTurnStateEntry)}
}

func (e *CodexExecutor) applyCodexHTTPTurnState(auth *cliproxyauth.Auth, executionSessionID string, headers http.Header) {
	if e == nil || e.httpTurnState == nil || headers == nil {
		return
	}
	if strings.TrimSpace(headers.Get(codexHeaderTurnState)) != "" {
		return
	}
	key := e.codexHTTPTurnStateKey(auth, executionSessionID)
	scope := codexHTTPTurnStateScope(headers.Get(codexHeaderTurnMetadata))
	if key == "" || scope == "" {
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
	scope := codexHTTPTurnStateScope(prepared.httpReq.Header.Get(codexHeaderTurnMetadata))
	if key == "" || scope == "" {
		return
	}
	e.httpTurnState.put(key, scope, state, time.Now())
	if strings.TrimSpace(prepared.httpReq.Header.Get(codexHeaderTurnState)) == "" {
		prepared.httpReq.Header.Set(codexHeaderTurnState, state)
	}
}

func (e *CodexExecutor) CloseExecutionSession(sessionID string) {
	e.clearCodexHTTPTurnStateSession(sessionID)
}

func (e *CodexExecutor) ResetExecutionSession(sessionID string) {
	e.clearCodexHTTPTurnStateSession(sessionID)
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
	return e.codexResponseDedupeScope(auth) + "|" + executionSessionID
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
	if !ok || entry.scope != scope {
		return ""
	}
	if now.Sub(entry.lastSeen) > codexHTTPTurnStateTTL {
		delete(s.entries, key)
		return ""
	}
	entry.lastSeen = now
	s.entries[key] = entry
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
	s.entries[key] = codexHTTPTurnStateEntry{
		state:    state,
		scope:    scope,
		lastSeen: now,
	}
}

func (s *codexHTTPTurnStateStore) deleteExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if s == nil || sessionID == "" {
		return
	}
	suffix := "|" + sessionID
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.entries {
		if strings.HasSuffix(key, suffix) {
			delete(s.entries, key)
		}
	}
}

func (s *codexHTTPTurnStateStore) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.entries = make(map[string]codexHTTPTurnStateEntry)
	s.cleanupOps = 0
	s.mu.Unlock()
}

func (s *codexHTTPTurnStateStore) cleanupLocked(now time.Time) {
	if s == nil {
		return
	}
	s.cleanupOps++
	if s.cleanupOps%codexHTTPTurnStateCleanupInterval != 0 {
		return
	}
	for key, entry := range s.entries {
		if now.Sub(entry.lastSeen) > codexHTTPTurnStateTTL {
			delete(s.entries, key)
		}
	}
}
