package executor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

func newCodexAgentIdentityTestAuth(t *testing.T, taskID string) (*cliproxyauth.Auth, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	metadata := map[string]any{
		"type":              "codex",
		"auth_kind":         "agent_identity",
		"agent_runtime_id":  "runtime-test",
		"agent_private_key": base64.StdEncoding.EncodeToString(der),
		"account_id":        "account-test",
	}
	if taskID != "" {
		metadata["task_id"] = taskID
	}
	return &cliproxyauth.Auth{ID: "agent-test.json", Provider: "codex", Metadata: metadata}, publicKey, privateKey
}

func decodeCodexAgentAssertionForTest(t *testing.T, value string) map[string]string {
	t.Helper()
	if !strings.HasPrefix(value, "AgentAssertion ") {
		t.Fatalf("authorization = %q, want AgentAssertion", value)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "AgentAssertion "))
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}
	var envelope map[string]string
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("Unmarshal assertion: %v", err)
	}
	return envelope
}

func TestBuildCodexAgentAssertionMatchesProtocol(t *testing.T) {
	auth, publicKey, _ := newCodexAgentIdentityTestAuth(t, "task-test")
	key, err := codexAgentIdentityKeyFromAuth(auth)
	if err != nil {
		t.Fatalf("codexAgentIdentityKeyFromAuth: %v", err)
	}
	now := time.Date(2026, 7, 21, 8, 9, 10, 0, time.UTC)
	authorization, err := buildCodexAgentAssertion(key, now)
	if err != nil {
		t.Fatalf("buildCodexAgentAssertion: %v", err)
	}
	envelope := decodeCodexAgentAssertionForTest(t, authorization)
	if envelope["agent_runtime_id"] != "runtime-test" || envelope["task_id"] != "task-test" || envelope["timestamp"] != "2026-07-21T08:09:10Z" {
		t.Fatalf("unexpected assertion envelope: %#v", envelope)
	}
	signature, err := base64.StdEncoding.DecodeString(envelope["signature"])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	signingText := "runtime-test:task-test:2026-07-21T08:09:10Z"
	if !ed25519.Verify(publicKey, []byte(signingText), signature) {
		t.Fatal("assertion signature did not verify")
	}
}

func TestCodexAgentIdentityPrivateKeyRejectsNonPKCS8(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"agent_private_key": base64.StdEncoding.EncodeToString(make([]byte, ed25519.PrivateKeySize))}}
	if _, err := codexAgentIdentityPrivateKey(auth); err == nil || !strings.Contains(err.Error(), "PKCS#8") {
		t.Fatalf("error = %v, want PKCS#8 validation error", err)
	}
}

func TestCodexAuthorizationExplicitOAuthOverridesRetainedAgentIdentity(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "task-retained")
	auth.Metadata["auth_kind"] = "oauth"
	auth.Metadata["access_token"] = "access-retained"
	auth.Attributes = map[string]string{"auth_kind": "oauth"}

	authorization, err := NewCodexExecutor(nil).codexAuthorization(context.Background(), auth, "access-retained")
	if err != nil {
		t.Fatalf("codexAuthorization: %v", err)
	}
	if authorization != "Bearer access-retained" {
		t.Fatalf("authorization = %q, want Bearer access-retained", authorization)
	}
}

func TestRegisterCodexAgentIdentityTaskSupportsPlainAndEncryptedResponses(t *testing.T) {
	auth, _, privateKey := newCodexAgentIdentityTestAuth(t, "")
	key, err := codexAgentIdentityKeyFromAuth(auth)
	if err != nil {
		t.Fatalf("codexAgentIdentityKeyFromAuth: %v", err)
	}

	tests := []struct {
		name     string
		response func(t *testing.T) string
	}{
		{name: "plain", response: func(t *testing.T) string { return `{"task_id":"task-plain"}` }},
		{name: "encrypted", response: func(t *testing.T) string {
			digest := sha512.Sum512(privateKey.Seed())
			var curvePrivate [32]byte
			copy(curvePrivate[:], digest[:32])
			curvePrivate[0] &= 248
			curvePrivate[31] &= 127
			curvePrivate[31] |= 64
			curvePublicBytes, errCurve := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
			if errCurve != nil {
				t.Fatalf("X25519: %v", errCurve)
			}
			var curvePublic [32]byte
			copy(curvePublic[:], curvePublicBytes)
			sealed, errSeal := box.SealAnonymous(nil, []byte("task-encrypted"), &curvePublic, rand.Reader)
			if errSeal != nil {
				t.Fatalf("SealAnonymous: %v", errSeal)
			}
			encoded, _ := json.Marshal(map[string]string{"encrypted_task_id": base64.StdEncoding.EncodeToString(sealed)})
			return string(encoded)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/agent/runtime-test/task/register" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				_, _ = io.WriteString(w, test.response(t))
			}))
			defer server.Close()
			previous := codexAgentIdentityTaskRegistrationURL
			codexAgentIdentityTaskRegistrationURL = server.URL
			defer func() { codexAgentIdentityTaskRegistrationURL = previous }()

			taskID, errRegister := NewCodexExecutor(nil).registerCodexAgentIdentityTask(context.Background(), auth, key)
			if errRegister != nil {
				t.Fatalf("registerCodexAgentIdentityTask: %v", errRegister)
			}
			want := "task-" + test.name
			if taskID != want {
				t.Fatalf("taskID = %q, want %q", taskID, want)
			}
		})
	}
}

func TestCodexAgentIdentityTaskRegistrationIsDeduplicated(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "")
	var registrations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		registrations.Add(1)
		_, _ = io.WriteString(w, `{"task_id":"task-shared"}`)
	}))
	defer server.Close()
	previous := codexAgentIdentityTaskRegistrationURL
	codexAgentIdentityTaskRegistrationURL = server.URL
	defer func() { codexAgentIdentityTaskRegistrationURL = previous }()

	executor := NewCodexExecutor(nil)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clone := auth.Clone()
			_, err := executor.codexAuthorization(context.Background(), clone, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("codexAuthorization: %v", err)
		}
	}
	if got := registrations.Load(); got != 1 {
		t.Fatalf("registrations = %d, want 1", got)
	}
}

func TestCodexAgentIdentityTaskRegistrationPublishesAuthUpdate(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"task_id":"task-persisted"}`)
	}))
	defer server.Close()
	previous := codexAgentIdentityTaskRegistrationURL
	codexAgentIdentityTaskRegistrationURL = server.URL
	defer func() { codexAgentIdentityTaskRegistrationURL = previous }()

	var published *cliproxyauth.Auth
	ctx := cliproxyauth.WithAuthUpdateCallback(context.Background(), func(_ context.Context, updated *cliproxyauth.Auth) {
		published = updated
	})
	if _, err := NewCodexExecutor(nil).codexAuthorization(ctx, auth, ""); err != nil {
		t.Fatalf("codexAuthorization: %v", err)
	}
	if published == nil || codexAgentIdentityTaskID(published) != "task-persisted" {
		t.Fatalf("published auth task = %q", codexAgentIdentityTaskID(published))
	}
}

func TestCodexAgentIdentityTaskRegistrationUsesAuthProxy(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "")
	var proxyRequests atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		if got := r.URL.Host; got != "agent-registration.invalid" {
			t.Errorf("proxied registration host = %q, want agent-registration.invalid", got)
		}
		_, _ = io.WriteString(w, `{"task_id":"task-via-proxy"}`)
	}))
	defer proxyServer.Close()
	auth.ProxyURL = proxyServer.URL

	previous := codexAgentIdentityTaskRegistrationURL
	codexAgentIdentityTaskRegistrationURL = "http://agent-registration.invalid"
	defer func() { codexAgentIdentityTaskRegistrationURL = previous }()

	authorization, err := NewCodexExecutor(nil).codexAuthorization(context.Background(), auth, "")
	if err != nil {
		t.Fatalf("codexAuthorization: %v", err)
	}
	if got := decodeCodexAgentAssertionForTest(t, authorization)["task_id"]; got != "task-via-proxy" {
		t.Fatalf("task_id = %q, want task-via-proxy", got)
	}
	if got := proxyRequests.Load(); got != 1 {
		t.Fatalf("proxy requests = %d, want 1", got)
	}
}

func TestCodexHttpRequestRotatesInvalidAgentTaskAndRetriesOnce(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "task-old")
	registrationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"task_id":"task-new"}`)
	}))
	defer registrationServer.Close()
	previous := codexAgentIdentityTaskRegistrationURL
	codexAgentIdentityTaskRegistrationURL = registrationServer.URL
	defer func() { codexAgentIdentityTaskRegistrationURL = previous }()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		envelope := decodeCodexAgentAssertionForTest(t, r.Header.Get("Authorization"))
		if attempt == 1 {
			if envelope["task_id"] != "task-old" {
				t.Errorf("first task = %q", envelope["task_id"])
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_task_id"}}`)
			return
		}
		if envelope["task_id"] != "task-new" {
			t.Errorf("retry task = %q", envelope["task_id"])
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := NewCodexExecutor(nil).HttpRequest(context.Background(), auth, req)
	if err != nil {
		t.Fatalf("HttpRequest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || requests.Load() != 2 {
		t.Fatalf("status/requests = %d/%d, want 200/2", resp.StatusCode, requests.Load())
	}
	if got := codexAgentIdentityTaskID(auth); got != "task-new" {
		t.Fatalf("task_id = %q, want task-new", got)
	}
}

func TestPrepareCodexHTTPCallUsesAgentAssertionForResponsesAndCompact(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "task-http")
	auth.Metadata["fedramp"] = true
	auth.Attributes = map[string]string{"header:Authorization": "Bearer must-not-win"}
	executor := NewCodexExecutor(nil)
	payload := []byte(`{"model":"gpt-5","input":[]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}
	for _, test := range []struct {
		targetURL string
		stream    bool
		accept    string
	}{
		{targetURL: "https://chatgpt.com/backend-api/codex/responses", accept: "application/json"},
		{targetURL: "https://chatgpt.com/backend-api/codex/responses", stream: true, accept: "text/event-stream"},
		{targetURL: "https://chatgpt.com/backend-api/codex/responses/compact", accept: "application/json"},
	} {
		call, err := executor.prepareCodexHTTPCall(context.Background(), auth, sdktranslator.FromString("openai-response"), "", test.targetURL, req, payload, "", test.stream)
		if err != nil {
			t.Fatalf("prepareCodexHTTPCall(%s, stream=%t): %v", test.targetURL, test.stream, err)
		}
		envelope := decodeCodexAgentAssertionForTest(t, call.prepared.httpReq.Header.Get("Authorization"))
		if envelope["task_id"] != "task-http" {
			t.Fatalf("task_id for %s = %q", test.targetURL, envelope["task_id"])
		}
		if got := call.prepared.httpReq.Header[codexHeaderChatGPTAccountID]; len(got) != 1 || got[0] != "account-test" {
			t.Fatalf("account header for %s = %#v", test.targetURL, got)
		}
		if got := call.prepared.httpReq.Header.Get(codexHeaderOpenAIFedramp); got != "true" {
			t.Fatalf("fedramp header for %s = %q", test.targetURL, got)
		}
		if got := call.prepared.httpReq.Header.Get("Accept"); got != test.accept {
			t.Fatalf("Accept for %s stream=%t = %q, want %q", test.targetURL, test.stream, got, test.accept)
		}
		if got := call.prepared.httpReq.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type for %s = %q", test.targetURL, got)
		}
		if got := call.prepared.httpReq.Header.Get("User-Agent"); got == "" {
			t.Fatalf("User-Agent for %s is empty", test.targetURL)
		}
		if got := call.prepared.httpReq.Header.Get("Originator"); got == "" {
			t.Fatalf("Originator for %s is empty", test.targetURL)
		}
	}
}

func TestApplyCodexWebsocketHeadersUsesAgentAssertion(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "task-ws")
	auth.Attributes = map[string]string{"header:Authorization": "Bearer must-not-win"}
	authorization, err := NewCodexExecutor(nil).codexAuthorization(context.Background(), auth, "")
	if err != nil {
		t.Fatalf("codexAuthorization: %v", err)
	}
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, authorization, nil)
	envelope := decodeCodexAgentAssertionForTest(t, headers.Get("Authorization"))
	if envelope["task_id"] != "task-ws" {
		t.Fatalf("task_id = %q, want task-ws", envelope["task_id"])
	}
	if got := headers[codexHeaderChatGPTAccountID]; len(got) != 1 || got[0] != "account-test" {
		t.Fatalf("account header = %#v, want account-test", got)
	}
}

func TestCodexWebsocketReconnectRefreshesAssertionAndRotatesInvalidTask(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "task-ws-old")
	registrationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"task_id":"task-ws-new"}`)
	}))
	defer registrationServer.Close()
	previous := codexAgentIdentityTaskRegistrationURL
	codexAgentIdentityTaskRegistrationURL = registrationServer.URL
	defer func() { codexAgentIdentityTaskRegistrationURL = previous }()

	assertedTasks := make(chan string, 2)
	messageRead := make(chan struct{}, 1)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envelope := decodeCodexAgentAssertionForTest(t, r.Header.Get("Authorization"))
		assertedTasks <- envelope["task_id"]
		if handshakes.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_task_id"}}`)
			return
		}
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("websocket upgrade: %v", errUpgrade)
			return
		}
		defer conn.Close()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read retried websocket request: %v", errRead)
			return
		}
		messageRead <- struct{}{}
	}))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	executor := NewCodexWebsocketsExecutor(nil)
	sess := &codexWebsocketSession{sessionID: "agent-reconnect"}
	headers := http.Header{"Authorization": {"AgentAssertion stale-must-not-be-reused"}}
	var readCh chan codexWebsocketRead
	conn, _, err := executor.retrySessionWebsocketRequestWithReason(
		context.Background(),
		auth,
		sess,
		nil,
		&readCh,
		auth.ID,
		wsURL,
		headers,
		helps.UpstreamRequestLog{URL: wsURL, Headers: headers.Clone()},
		[]byte(`{"type":"response.create"}`),
		"read_error",
		io.EOF,
	)
	if err != nil {
		t.Fatalf("retrySessionWebsocketRequestWithReason: %v", err)
	}
	defer executor.invalidateUpstreamConn(sess, conn, "test_complete", nil)

	if got := <-assertedTasks; got != "task-ws-old" {
		t.Fatalf("first handshake task = %q, want task-ws-old", got)
	}
	if got := <-assertedTasks; got != "task-ws-new" {
		t.Fatalf("second handshake task = %q, want task-ws-new", got)
	}
	select {
	case <-messageRead:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for retried websocket request")
	}
	if got := codexAgentIdentityTaskID(auth); got != "task-ws-new" {
		t.Fatalf("persisted task_id = %q, want task-ws-new", got)
	}
}

func TestCodexAgentIdentityGenericUnauthorizedDoesNotRotateTask(t *testing.T) {
	auth, _, _ := newCodexAgentIdentityTestAuth(t, "task-old")
	if recovered, err := NewCodexExecutor(nil).recoverCodexAgentIdentityTask(context.Background(), auth, http.StatusUnauthorized, []byte(`{"error":{"code":"invalid_signature"}}`)); err != nil || recovered {
		t.Fatalf("recovered/error = %v/%v, want false/nil", recovered, err)
	}
}
