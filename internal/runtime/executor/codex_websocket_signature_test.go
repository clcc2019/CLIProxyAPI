package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexWebsocketRecoversForeignEncryptedContent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, session := range []bool{false, true} {
			for _, event := range []string{"error", "response.failed"} {
				t.Run(fmt.Sprintf("stream=%t/session=%t/%s", stream, session, event), func(t *testing.T) {
					runCodexWebsocketEncryptedContentTest(t, stream, session, event, false, false)
				})
			}
		}
	}
}

func TestCodexWebsocketEncryptedRecoveryStopsAfterOneRetry(t *testing.T) {
	runCodexWebsocketEncryptedContentTest(t, true, true, "error", true, false)
}

func TestCodexWebsocketEncryptedRecoveryDoesNotReplayEmittedOutput(t *testing.T) {
	runCodexWebsocketEncryptedContentTest(t, true, true, "response.failed", false, true)
}

func runCodexWebsocketEncryptedContentTest(t *testing.T, stream, session bool, event string, alwaysReject, emitOutput bool) {
	t.Helper()
	received := make(chan []byte, 8)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for attempt := 0; ; attempt++ {
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, body, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			received <- body
			if attempt == 0 || alwaysReject {
				if emitOutput {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"already sent"}`))
				}
				failure := `{"type":"error","status":400,"error":{"code":"invalid_encrypted_content","message":"The encrypted content for item rs_foreign could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}`
				if event == "response.failed" {
					failure = `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"invalid_encrypted_content","message":"Encrypted function output content could not be decrypted or decoded."}}}`
				}
				if conn.WriteMessage(websocket.TextMessage, []byte(failure)) != nil {
					return
				}
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[]}}`))
		}
	}))
	defer server.Close()
	e := NewCodexWebsocketsExecutor(nil)
	defer e.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "test-token"}}
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_anchor","input":[{"type":"reasoning","id":"rs_foreign","summary":[],"encrypted_content":"` + validCodexReasoningEncryptedContentForTest() + `"},{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"keep this result"},{"type":"encrypted_content","encrypted_content":"foreign-tool-ciphertext"}]}]}`)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: stream, OriginalRequest: body}
	if session {
		opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: t.Name()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: body}
	var executionErr error
	if stream {
		result, err := e.ExecuteStream(ctx, auth, req, opts)
		executionErr = err
		if err == nil {
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					executionErr = chunk.Err
				}
			}
		}
	} else {
		_, executionErr = e.Execute(ctx, auth, req, opts)
	}
	wantError := alwaysReject || emitOutput
	if (executionErr != nil) != wantError {
		t.Fatalf("execution error = %v, wantError=%t", executionErr, wantError)
	}
	wantAttempts := 2
	if emitOutput {
		wantAttempts = 1
	}
	if len(received) != wantAttempts {
		t.Fatalf("got %d attempts, want %d", len(received), wantAttempts)
	}
	first := <-received
	if !gjson.GetBytes(first, `input.#(type=="reasoning").encrypted_content`).Exists() {
		t.Fatalf("same-provider first request lost valid ciphertext: %s", first)
	}
	if wantAttempts == 2 {
		retry := <-received
		if gjson.GetBytes(retry, `input.#(type=="reasoning").encrypted_content`).Exists() || gjson.GetBytes(retry, `input.#(type=="reasoning").id`).Exists() {
			t.Fatalf("retry retained rejected reasoning ciphertext or ID: %s", retry)
		}
		if gjson.GetBytes(retry, "previous_response_id").String() != "resp_anchor" {
			t.Fatalf("retry lost explicit stored anchor: %s", retry)
		}
		if gjson.GetBytes(retry, `input.#(type=="function_call_output").output.0.text`).String() != "keep this result" ||
			gjson.GetBytes(retry, `input.#(type=="function_call_output").output.1.type`).String() != "input_text" {
			t.Fatalf("retry did not preserve readable tool output while removing ciphertext: %s", retry)
		}
	}
}

func TestCodexEncryptedRetryReconnectsWithCredentialsAndNewConnection(t *testing.T) {
	upgrader := websocket.Upgrader{}
	received := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer reconnect-test-token" || r.Header.Get("X-Test-Session") != "session-1" {
			http.Error(w, "missing handshake credentials", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, body, err := conn.ReadMessage()
		if err != nil {
			return
		}
		received <- body
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_reconnected","status":"completed","output":[]}}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := NewCodexWebsocketsExecutor(nil)
	e.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession), parked: make(map[string]*codexWebsocketSession)}
	defer e.closeAllExecutionSessions("test_cleanup")
	wsURL := "ws" + server.URL[len("http"):]
	headers := http.Header{"Authorization": {"Bearer reconnect-test-token"}, "X-Test-Session": {"session-1"}}
	conn, _, err := e.dialCodexWebsocket(ctx, nil, wsURL, headers)
	if err != nil {
		t.Fatal(err)
	}
	// Closing locally guarantees the next write fails without a timing race
	// with the upstream reader, forcing the encrypted-content reconnect path.
	_ = conn.Close()
	sess := e.getOrCreateSession(t.Name(), "")
	oldReadCh := make(chan codexWebsocketRead, codexResponsesWebsocketReadBuffer)
	readCh := oldReadCh
	sess.setActive(readCh, conn)
	body := []byte(`{"type":"response.create","input":[{"role":"user","content":"retry"}]}`)
	connRetry, retryBody, err := e.retryCodexWebsocketWithoutEncryptedContent(ctx, nil, sess, conn, &readCh, "test-auth", wsURL, headers, nil, body, fmt.Errorf("invalid_encrypted_content"))
	if err != nil {
		t.Fatal(err)
	}
	if connRetry == nil || connRetry == conn || readCh == oldReadCh {
		t.Fatal("reconnect did not return the new connection and read channel")
	}
	if string(retryBody) != string(body) {
		t.Fatalf("retry body changed: %s", retryBody)
	}
	_, payload, _, err := readCodexWebsocketMessage(ctx, sess, connRetry, readCh)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(payload, "response.id").String() != "resp_reconnected" {
		t.Fatalf("new connection response = %s", payload)
	}
	select {
	case got := <-received:
		if string(got) != string(body) {
			t.Fatalf("upstream retry body = %s", got)
		}
	default:
		t.Fatal("retry request was not received")
	}
}

func TestCodexWebsocketEncryptedRetryPreservesFullContextForFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, event := range []string{"error", "response.failed"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, event), func(t *testing.T) {
				const firstInput = `{"type":"message","role":"user","content":[{"type":"input_text","text":"remember the first turn"}]}`
				received := make(chan []byte, 4)
				upgrader := websocket.Upgrader{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for attempt := 0; attempt < 4; attempt++ {
						_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
						_, body, err := conn.ReadMessage()
						if err != nil {
							return
						}
						received <- body
						response := `{"type":"response.completed","response":{"id":"resp_first","status":"completed","output":[]}}`
						if attempt == 1 {
							if event == "error" {
								response = `{"type":"error","status":400,"error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted or parsed."}}`
							} else {
								response = `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted or parsed."}}}`
							}
						} else if attempt == 2 {
							response = `{"type":"error","status":400,"error":{"code":"previous_response_not_found","message":"Previous response not found","param":"previous_response_id"}}`
						} else if attempt == 3 {
							response = `{"type":"response.completed","response":{"id":"resp_final","status":"completed","output":[]}}`
						}
						if conn.WriteMessage(websocket.TextMessage, []byte(response)) != nil {
							return
						}
					}
				}))
				defer server.Close()
				e := NewCodexWebsocketsExecutor(nil)
				e.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession), parked: make(map[string]*codexWebsocketSession)}
				defer e.closeAllExecutionSessions("test_cleanup")
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "test-token"}}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				execute := func(body []byte) {
					t.Helper()
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: stream, OriginalRequest: body, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: t.Name()}}
					req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: body}
					if stream {
						result, err := e.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else {
						if _, err := e.Execute(ctx, auth, req, opts); err != nil {
							t.Fatal(err)
						}
					}
				}
				execute([]byte(`{"model":"gpt-5.4","input":[` + firstInput + `]}`))
				execute([]byte(`{"model":"gpt-5.4","input":[` + firstInput + `,{"type":"reasoning","id":"rs_foreign","summary":[],"encrypted_content":"` + validCodexReasoningEncryptedContentForTest() + `"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`))
				if len(received) != 4 {
					t.Fatalf("requests = %d, want 4", len(received))
				}
				<-received
				incremental, sanitized, fallback := <-received, <-received, <-received
				for _, body := range [][]byte{incremental, sanitized} {
					if gjson.GetBytes(body, "previous_response_id").String() != "resp_first" || gjson.GetBytes(body, "input.#").Int() != 2 {
						t.Fatalf("expected incremental request: %s", body)
					}
				}
				if gjson.GetBytes(fallback, "previous_response_id").Exists() || gjson.GetBytes(fallback, "input.#").Int() != 3 || gjson.GetBytes(fallback, "input.0.content.0.text").String() != "remember the first turn" {
					t.Fatalf("fallback lost full history: %s", fallback)
				}
				if gjson.GetBytes(fallback, `input.#(type=="reasoning").encrypted_content`).Exists() {
					t.Fatalf("fallback restored rejected ciphertext: %s", fallback)
				}
			})
		}
	}
}
