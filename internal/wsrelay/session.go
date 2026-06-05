package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	readTimeout           = 60 * time.Second
	writeTimeout          = 10 * time.Second
	maxInboundMessageLen  = 64 << 20 // 64 MiB
	heartbeatInterval     = 30 * time.Second
	pendingResponseBuffer = 32
)

var errClosed = errors.New("websocket session closed")

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

type pendingRequest struct {
	ch        chan Message
	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
}

func (pr *pendingRequest) close() {
	if pr == nil {
		return
	}
	pr.closeOnce.Do(func() {
		pr.mu.Lock()
		defer pr.mu.Unlock()
		pr.closed = true
		close(pr.ch)
	})
}

func (pr *pendingRequest) send(msg Message) bool {
	if pr == nil {
		return false
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.closed {
		return false
	}
	select {
	case pr.ch <- msg:
		return true
	default:
		return false
	}
}

func (pr *pendingRequest) sendTerminal(msg Message) bool {
	if pr.send(msg) {
		return true
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.closed {
		return false
	}
	select {
	case pr.ch <- msg:
		return true
	default:
	}
	select {
	case <-pr.ch:
	default:
	}
	select {
	case pr.ch <- msg:
		return true
	default:
		return false
	}
}

type session struct {
	conn       *websocket.Conn
	manager    *Manager
	provider   string
	id         string
	closed     chan struct{}
	closeOnce  sync.Once
	writeMutex sync.Mutex
	pending    sync.Map // map[string]*pendingRequest
}

func newSession(conn *websocket.Conn, mgr *Manager, id string) *session {
	s := &session{
		conn:     conn,
		manager:  mgr,
		provider: "",
		id:       id,
		closed:   make(chan struct{}),
	}
	conn.SetReadLimit(maxInboundMessageLen)
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})
	s.startHeartbeat()
	return s
}

func (s *session) startHeartbeat() {
	if s == nil || s.conn == nil {
		return
	}
	ticker := time.NewTicker(heartbeatInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-s.closed:
				return
			case <-ticker.C:
				s.writeMutex.Lock()
				err := s.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(writeTimeout))
				s.writeMutex.Unlock()
				if err != nil {
					s.cleanup(err)
					return
				}
			}
		}
	}()
}

func (s *session) run() {
	defer s.cleanup(errClosed)
	for {
		var msg Message
		if err := s.conn.ReadJSON(&msg); err != nil {
			s.cleanup(err)
			return
		}
		s.dispatch(msg)
	}
}

func (s *session) dispatch(msg Message) {
	if msg.Type == MessageTypePing {
		_ = s.send(context.Background(), Message{ID: msg.ID, Type: MessageTypePong})
		return
	}
	if value, ok := s.pending.Load(msg.ID); ok {
		req := value.(*pendingRequest)
		if terminalMessage(msg) {
			req.sendTerminal(msg)
			if actual, loaded := s.pending.LoadAndDelete(msg.ID); loaded {
				actual.(*pendingRequest).close()
			}
		} else {
			req.send(msg)
		}
		return
	}
	if terminalMessage(msg) && s.manager != nil {
		s.manager.logDebugf("wsrelay: received terminal message for unknown id %s (provider=%s)", msg.ID, s.provider)
	}
}

func terminalMessage(msg Message) bool {
	return msg.Type == MessageTypeHTTPResp || msg.Type == MessageTypeError || msg.Type == MessageTypeStreamEnd
}

func (s *session) send(ctx context.Context, msg Message) error {
	ctx = contextOrBackground(ctx)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return errClosed
	default:
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return errClosed
	default:
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if err := s.conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

func (s *session) request(ctx context.Context, msg Message) (<-chan Message, error) {
	ctx = contextOrBackground(ctx)
	if msg.ID == "" {
		return nil, fmt.Errorf("wsrelay: message id is required")
	}
	if _, loaded := s.pending.LoadOrStore(msg.ID, &pendingRequest{ch: make(chan Message, pendingResponseBuffer)}); loaded {
		return nil, fmt.Errorf("wsrelay: duplicate message id %s", msg.ID)
	}
	value, _ := s.pending.Load(msg.ID)
	req := value.(*pendingRequest)
	if err := s.send(ctx, msg); err != nil {
		if actual, loaded := s.pending.LoadAndDelete(msg.ID); loaded {
			req := actual.(*pendingRequest)
			req.close()
		}
		return nil, err
	}
	go func() {
		select {
		case <-ctx.Done():
			if actual, loaded := s.pending.LoadAndDelete(msg.ID); loaded {
				actual.(*pendingRequest).close()
			}
		case <-s.closed:
		}
	}()
	return req.ch, nil
}

func (s *session) cleanup(cause error) {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.pending.Range(func(key, value any) bool {
			req := value.(*pendingRequest)
			msg := Message{ID: key.(string), Type: MessageTypeError, Payload: map[string]any{"error": cause.Error()}}
			req.sendTerminal(msg)
			req.close()
			return true
		})
		s.pending = sync.Map{}
		_ = s.conn.Close()
		if s.manager != nil {
			s.manager.handleSessionClosed(s, cause)
		}
	})
}
