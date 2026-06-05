package wsrelay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newTestSession(t *testing.T) (*session, *websocket.Conn, func()) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		serverConnCh <- conn
	}))

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("dial websocket: %v", err)
	}

	var serverConn *websocket.Conn
	select {
	case err := <-errCh:
		_ = clientConn.Close()
		server.Close()
		t.Fatalf("upgrade websocket: %v", err)
	case serverConn = <-serverConnCh:
	case <-time.After(time.Second):
		_ = clientConn.Close()
		server.Close()
		t.Fatal("timed out waiting for websocket upgrade")
	}

	s := newSession(serverConn, nil, "test-session")
	cleanup := func() {
		s.cleanup(errClosed)
		_ = clientConn.Close()
		server.Close()
	}
	return s, clientConn, cleanup
}

func TestSessionRequestRejectsCanceledContext(t *testing.T) {
	s, clientConn, cleanup := newTestSession(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.request(ctx, Message{ID: "req-1", Type: MessageTypeHTTPReq})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("request error = %v, want context.Canceled", err)
	}
	if _, ok := s.pending.Load("req-1"); ok {
		t.Fatal("request left canceled message in pending map")
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	var msg Message
	if err := clientConn.ReadJSON(&msg); err == nil {
		t.Fatalf("canceled request wrote websocket message: %+v", msg)
	}
}

func TestSessionRequestAcceptsNilContext(t *testing.T) {
	s, clientConn, cleanup := newTestSession(t)
	defer cleanup()

	respCh, err := s.request(nil, Message{ID: "req-1", Type: MessageTypeHTTPReq})
	if err != nil {
		t.Fatalf("request with nil context returned error: %v", err)
	}
	if respCh == nil {
		t.Fatal("request with nil context returned nil response channel")
	}
	if _, ok := s.pending.Load("req-1"); !ok {
		t.Fatal("request with nil context was not tracked as pending")
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
	var msg Message
	if err := clientConn.ReadJSON(&msg); err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	if msg.ID != "req-1" || msg.Type != MessageTypeHTTPReq {
		t.Fatalf("websocket message = %+v, want req-1 http_request", msg)
	}
}

func TestSessionDispatchKeepsTerminalMessageWhenPendingBufferFull(t *testing.T) {
	req := &pendingRequest{ch: make(chan Message, 2)}
	req.ch <- Message{ID: "req-1", Type: MessageTypeStreamChunk, Payload: map[string]any{"data": "old-1"}}
	req.ch <- Message{ID: "req-1", Type: MessageTypeStreamChunk, Payload: map[string]any{"data": "old-2"}}

	s := &session{closed: make(chan struct{})}
	s.pending.Store("req-1", req)
	s.dispatch(Message{ID: "req-1", Type: MessageTypeError, Payload: map[string]any{"error": "upstream failed"}})

	if _, ok := s.pending.Load("req-1"); ok {
		t.Fatal("terminal dispatch left request in pending map")
	}
	var got []Message
	for msg := range req.ch {
		got = append(got, msg)
	}
	for _, msg := range got {
		if msg.Type == MessageTypeError {
			return
		}
	}
	t.Fatalf("terminal error was not delivered; got %+v", got)
}

func TestPendingRequestSendAfterCloseDoesNotPanic(t *testing.T) {
	req := &pendingRequest{ch: make(chan Message, 1)}
	req.close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("send after close panicked: %v", r)
		}
	}()
	if req.send(Message{ID: "req-1", Type: MessageTypeStreamChunk}) {
		t.Fatal("send after close returned true")
	}
	if req.sendTerminal(Message{ID: "req-1", Type: MessageTypeError}) {
		t.Fatal("terminal send after close returned true")
	}
}
