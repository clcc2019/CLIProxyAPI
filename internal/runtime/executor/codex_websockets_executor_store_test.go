package executor

import (
	"strconv"
	"sync"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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
	sess := newCodexWebsocketSession("exec-parked", reuseKey)
	sess.authID = "auth-parked"
	sess.wsURL = "wss://example.test/responses"
	store.parked[reuseKey] = sess

	CloseCodexWebsocketSessionsForAuthID("auth-parked", "test_cleanup")

	store.parkedMu.Lock()
	_, stillParked := store.parked[reuseKey]
	store.parkedMu.Unlock()
	if stillParked {
		t.Fatalf("expected parked session to be removed for auth")
	}
}

// A parked session keeps its connection — and the readUpstreamLoop goroutine
// reading from it — alive. Unparking rebinds sessionID/reuseKey under
// sessionsMu while that goroutine reads them under connMu and, for logging,
// under no lock at all. Locking on both sides is not enough when the locks
// differ, so these fields must be safe for concurrent access on their own.
// Run with -race; this fails on plain string fields.
func TestCodexWebsocketSessionIdentityConcurrentRebind(t *testing.T) {
	store := &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}
	sess := newCodexWebsocketSession("exec-initial", "reuse-initial")
	store.sessions["exec-initial"] = sess

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: the unpark rehoming path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			store.sessionsMu.Lock()
			sess.setSessionID("exec-rebound-" + strconv.Itoa(i))
			sess.setReuseKey("reuse-rebound-" + strconv.Itoa(i))
			store.sessionsMu.Unlock()
		}
	}()

	// Reader: the surviving readUpstreamLoop goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			sess.connMu.Lock()
			_ = sess.sessionID()
			sess.connMu.Unlock()
			// Unsynchronized reads, as the connect/disconnect logging does.
			_ = sess.sessionID()
			_ = sess.reuseKey()
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if sess.sessionID() == "" {
		t.Error("sessionID was lost during concurrent rebinding")
	}
	if sess.reuseKey() == "" {
		t.Error("reuseKey was lost during concurrent rebinding")
	}
}
