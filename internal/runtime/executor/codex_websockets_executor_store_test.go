package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCodexWebsocketsExecutor_SessionStoreSurvivesExecutorReplacement(t *testing.T) {
	sessionID := "test-session-store-survives-replace"

	globalCodexWebsocketSessionStore.sessionsMu.Lock()
	delete(globalCodexWebsocketSessionStore.sessions, sessionID)
	globalCodexWebsocketSessionStore.sessionsMu.Unlock()

	globalCodexWebsocketSessionStore.parkedMu.Lock()
	delete(globalCodexWebsocketSessionStore.parked, sessionID)
	globalCodexWebsocketSessionStore.parkedMu.Unlock()

	exec1 := NewCodexWebsocketsExecutor(nil)
	sess1 := exec1.getOrCreateSession(sessionID, "")
	if sess1 == nil {
		t.Fatalf("expected session to be created")
	}

	exec2 := NewCodexWebsocketsExecutor(nil)
	sess2 := exec2.getOrCreateSession(sessionID, "")
	if sess2 == nil {
		t.Fatalf("expected session to be available across executors")
	}
	if sess1 != sess2 {
		t.Fatalf("expected the same session instance across executors")
	}

	exec1.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)

	globalCodexWebsocketSessionStore.sessionsMu.Lock()
	_, stillPresent := globalCodexWebsocketSessionStore.sessions[sessionID]
	globalCodexWebsocketSessionStore.sessionsMu.Unlock()
	if !stillPresent {
		t.Fatalf("expected session to remain after executor replacement close marker")
	}

	exec2.CloseExecutionSession(sessionID)

	globalCodexWebsocketSessionStore.sessionsMu.Lock()
	_, presentAfterClose := globalCodexWebsocketSessionStore.sessions[sessionID]
	globalCodexWebsocketSessionStore.sessionsMu.Unlock()
	if presentAfterClose {
		t.Fatalf("expected session to be removed after explicit close")
	}
}

func TestCloseCodexWebsocketSessionsForAuthIDClosesParkedSession(t *testing.T) {
	previousStore := globalCodexWebsocketSessionStore
	t.Cleanup(func() { globalCodexWebsocketSessionStore = previousStore })

	store := &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	globalCodexWebsocketSessionStore = store

	const reuseKey = "auth-parked|wss://example.test/responses|cache-1"
	sess := &codexWebsocketSession{
		sessionID: "exec-parked",
		reuseKey:  reuseKey,
		authID:    "auth-parked",
		wsURL:     "wss://example.test/responses",
	}
	store.parked[reuseKey] = sess

	CloseCodexWebsocketSessionsForAuthID("auth-parked", "test_cleanup")

	store.parkedMu.Lock()
	_, stillParked := store.parked[reuseKey]
	store.parkedMu.Unlock()
	if stillParked {
		t.Fatalf("expected parked session to be removed for auth")
	}
}
