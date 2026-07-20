package executor

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexWebsocketReconnectsWhenSessionTargetChanges(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(&config.Config{})
	executor.store = &codexWebsocketSessionStore{
		sessions: make(map[string]*codexWebsocketSession),
		parked:   make(map[string]*codexWebsocketSession),
	}

	serverA, closedA := newCodexWebsocketTargetServer(t)
	defer serverA.Close()
	serverB, closedB := newCodexWebsocketTargetServer(t)
	defer serverB.Close()

	const sessionID = "target-switch-session"
	disconnectCh := executor.UpstreamDisconnectChan(sessionID)
	sess := executor.getOrCreateSession(sessionID, "")
	if sess == nil {
		t.Fatal("expected websocket session")
	}
	defer executor.CloseExecutionSession(sessionID)

	authA := &cliproxyauth.Auth{ID: "auth-a", ProxyURL: "direct"}
	authB := &cliproxyauth.Auth{ID: "auth-b", ProxyURL: "direct"}
	wsURLA := "ws" + strings.TrimPrefix(serverA.URL, "http")
	wsURLB := "ws" + strings.TrimPrefix(serverB.URL, "http")

	connA := ensureCodexWebsocketTargetConn(t, executor, authA, sess, wsURLA)
	connAReused := ensureCodexWebsocketTargetConn(t, executor, authA, sess, wsURLA)
	if connAReused != connA {
		t.Fatal("matching websocket target did not reuse the existing connection")
	}

	connURLB := ensureCodexWebsocketTargetConn(t, executor, authA, sess, wsURLB)
	if connURLB == connA {
		t.Fatal("websocket URL change reused the existing connection")
	}
	waitCodexWebsocketTargetClosed(t, closedA, authA.ID)

	connAuthB := ensureCodexWebsocketTargetConn(t, executor, authB, sess, wsURLB)
	if connAuthB == connURLB {
		t.Fatal("websocket auth change reused the existing connection")
	}
	waitCodexWebsocketTargetClosed(t, closedB, authA.ID)

	sess.connMu.Lock()
	gotAuthID := sess.authID
	gotURL := sess.wsURL
	gotProxyPolicy := sess.proxyPolicy
	sess.connMu.Unlock()
	if gotAuthID != authB.ID || gotURL != wsURLB {
		t.Fatalf("session target = {%q %q}, want {%q %q}", gotAuthID, gotURL, authB.ID, wsURLB)
	}
	if wantPolicy := codexWebsocketProxyPolicyFingerprint(executor.cfg, authB); gotProxyPolicy != wantPolicy {
		t.Fatalf("session proxy policy = %q, want %q", gotProxyPolicy, wantPolicy)
	}
	assertNoControlledCodexWebsocketDisconnect(t, disconnectCh)
}

func TestCodexWebsocketReconnectsWhenProxyPolicyChanges(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*config.Config, *cliproxyauth.Auth)
		update func(*config.Config, *cliproxyauth.Auth, string)
	}{
		{
			name: "auth proxy",
			setup: func(_ *config.Config, auth *cliproxyauth.Auth) {
				auth.ProxyURL = "direct"
			},
			update: func(_ *config.Config, auth *cliproxyauth.Auth, proxyURL string) {
				auth.ProxyURL = proxyURL
			},
		},
		{
			name: "global proxy",
			setup: func(cfg *config.Config, _ *cliproxyauth.Auth) {
				cfg.ProxyURL = "direct"
			},
			update: func(cfg *config.Config, _ *cliproxyauth.Auth, proxyURL string) {
				cfg.ProxyURL = proxyURL
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			auth := &cliproxyauth.Auth{ID: "auth-a"}
			tt.setup(cfg, auth)
			executor := NewCodexWebsocketsExecutor(cfg)
			executor.store = &codexWebsocketSessionStore{
				sessions: make(map[string]*codexWebsocketSession),
				parked:   make(map[string]*codexWebsocketSession),
			}

			target, closed := newCodexWebsocketTargetServer(t)
			defer target.Close()
			connectProxy, proxyConnectHost := newCodexWebsocketConnectProxy(t)
			defer connectProxy.Close()

			sessionID := "proxy-switch-" + strings.ReplaceAll(tt.name, " ", "-")
			disconnectCh := executor.UpstreamDisconnectChan(sessionID)
			sess := executor.getOrCreateSession(sessionID, "")
			if sess == nil {
				t.Fatal("expected websocket session")
			}
			defer executor.CloseExecutionSession(sessionID)

			wsURL := "ws" + strings.TrimPrefix(target.URL, "http")
			firstPolicy := codexWebsocketProxyPolicyFingerprint(cfg, auth)
			firstConn := ensureCodexWebsocketTargetConn(t, executor, auth, sess, wsURL)

			tt.update(cfg, auth, connectProxy.URL)
			secondPolicy := codexWebsocketProxyPolicyFingerprint(cfg, auth)
			if secondPolicy == firstPolicy {
				t.Fatal("proxy policy fingerprint did not change")
			}
			secondConn := ensureCodexWebsocketTargetConn(t, executor, auth, sess, wsURL)
			if secondConn == firstConn {
				t.Fatal("proxy policy change reused the existing connection")
			}

			waitCodexWebsocketTargetClosed(t, closed, auth.ID)
			select {
			case gotHost := <-proxyConnectHost:
				wantHost := strings.TrimPrefix(target.URL, "http://")
				if gotHost != wantHost {
					t.Fatalf("proxy CONNECT host = %q, want %q", gotHost, wantHost)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("updated proxy policy was not used for websocket reconnect")
			}

			sess.connMu.Lock()
			gotPolicy := sess.proxyPolicy
			sess.connMu.Unlock()
			if gotPolicy != secondPolicy {
				t.Fatalf("session proxy policy = %q, want %q", gotPolicy, secondPolicy)
			}
			assertNoControlledCodexWebsocketDisconnect(t, disconnectCh)
		})
	}
}

func TestCodexWebsocketProxyPolicyKeysDoNotExposeCredentials(t *testing.T) {
	const (
		username = "proxy-user-secret"
		password = "proxy-password-secret"
	)
	auth := &cliproxyauth.Auth{
		ID:       "auth-a",
		ProxyURL: "http://" + username + ":" + password + "@proxy.example.test:8080",
	}
	fingerprint := codexWebsocketProxyPolicyFingerprint(nil, auth)
	reuseKey := codexWebsocketReusableKeyFromParts("auth-a", "wss://example.test/responses", "cache-a", "window-a", fingerprint)
	cacheKey := codexWebsocketDialerCacheKey(nil, auth)

	for name, value := range map[string]string{
		"fingerprint": fingerprint,
		"reuse key":   reuseKey,
		"dialer key":  cacheKey,
	} {
		if strings.Contains(value, username) || strings.Contains(value, password) {
			t.Fatalf("%s exposed proxy credentials: %q", name, value)
		}
	}

	rotated := auth.Clone()
	rotated.ProxyURL = "http://" + username + ":rotated-password@proxy.example.test:8080"
	if rotatedFingerprint := codexWebsocketProxyPolicyFingerprint(nil, rotated); rotatedFingerprint == fingerprint {
		t.Fatal("proxy credential rotation did not change policy fingerprint")
	}
}

func TestBuildCodexWebsocketDialerRejectsInvalidExplicitProxy(t *testing.T) {
	dialer := buildCodexWebsocketDialer("ftp://proxy-user-secret:proxy-password-secret@proxy.example.test:21")
	if dialer.Proxy == nil {
		t.Fatal("invalid explicit proxy unexpectedly enabled direct dialing")
	}
	_, errProxy := dialer.Proxy(httptest.NewRequest(http.MethodGet, "http://upstream.example.test", nil))
	if errProxy == nil {
		t.Fatal("invalid explicit proxy did not fail closed")
	}
	if strings.Contains(errProxy.Error(), "proxy-user-secret") || strings.Contains(errProxy.Error(), "proxy-password-secret") {
		t.Fatalf("invalid proxy error exposed credentials: %v", errProxy)
	}
}

func ensureCodexWebsocketTargetConn(t *testing.T, executor *CodexWebsocketsExecutor, auth *cliproxyauth.Auth, sess *codexWebsocketSession, wsURL string) *websocket.Conn {
	t.Helper()
	headers := http.Header{"X-Test-Auth": []string{auth.ID}}
	conn, resp, errEnsure := executor.ensureUpstreamConn(context.Background(), auth, sess, auth.ID, wsURL, headers)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if errEnsure != nil {
		t.Fatalf("ensure websocket connection: %v", errEnsure)
	}
	if conn == nil {
		t.Fatal("ensure websocket connection returned nil")
	}
	return conn
}

func newCodexWebsocketTargetServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	closed := make(chan string, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authID := r.Header.Get("X-Test-Auth")
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() {
			_ = conn.Close()
			closed <- authID
		}()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	return server, closed
}

func newCodexWebsocketConnectProxy(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	connectHost := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		upstream, errDial := net.DialTimeout("tcp", r.Host, 2*time.Second)
		if errDial != nil {
			http.Error(w, errDial.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		downstream, buffered, errHijack := hijacker.Hijack()
		if errHijack != nil {
			_ = upstream.Close()
			return
		}
		defer func() { _ = downstream.Close() }()
		defer func() { _ = upstream.Close() }()
		connectHost <- r.Host
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if errFlush := buffered.Flush(); errFlush != nil {
			return
		}
		copyDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, downstream)
			close(copyDone)
		}()
		_, _ = io.Copy(downstream, upstream)
		<-copyDone
	}))
	return server, connectHost
}

func waitCodexWebsocketTargetClosed(t *testing.T, closed <-chan string, wantAuthID string) {
	t.Helper()
	select {
	case gotAuthID := <-closed:
		if gotAuthID != wantAuthID {
			t.Fatalf("closed websocket auth = %q, want %q", gotAuthID, wantAuthID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stale websocket connection to close")
	}
}

func assertNoControlledCodexWebsocketDisconnect(t *testing.T, disconnectCh <-chan error) {
	t.Helper()
	select {
	case errDisconnect := <-disconnectCh:
		t.Fatalf("controlled websocket switch notified downstream: %v", errDisconnect)
	case <-time.After(100 * time.Millisecond):
	}
}
