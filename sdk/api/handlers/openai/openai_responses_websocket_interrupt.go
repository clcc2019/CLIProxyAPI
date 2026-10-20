package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type responsesWebsocketReadFrame struct {
	payload []byte
}

type responsesWebsocketInterruptEvent struct {
	payload   []byte
	forwarded bool
	local     bool
	errMsg    *interfaces.ErrorMessage
}

type responsesWebsocketInterruptState struct {
	mu        sync.RWMutex
	active    bool
	queued    int
	sessionID string
	events    chan responsesWebsocketInterruptEvent
}

func newResponsesWebsocketInterruptState(sessionID string) *responsesWebsocketInterruptState {
	return &responsesWebsocketInterruptState{
		sessionID: strings.TrimSpace(sessionID),
		events:    make(chan responsesWebsocketInterruptEvent, 16),
	}
}

func (s *responsesWebsocketInterruptState) setSessionID(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sessionID = strings.TrimSpace(sessionID)
	s.mu.Unlock()
}

func (s *responsesWebsocketInterruptState) currentSessionID() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionID
}

func (s *responsesWebsocketInterruptState) queueRequest() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.queued++
	s.mu.Unlock()
}

func (s *responsesWebsocketInterruptState) startRequest() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.queued > 0 {
		s.queued--
	}
	s.active = true
	s.mu.Unlock()
}

func (s *responsesWebsocketInterruptState) end() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
}

func (s *responsesWebsocketInterruptState) canInterruptLocally() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active || s.queued > 0
}

func (s *responsesWebsocketInterruptState) cancelQueuedRequest() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.queued > 0 {
		s.queued--
	}
	s.mu.Unlock()
}

// readResponsesWebsocketInput owns downstream reads so response.interrupt can
// be dispatched while the main handler is forwarding an active response.
func readResponsesWebsocketInput(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	conn *websocket.Conn,
	interruptState *responsesWebsocketInterruptState,
	h *OpenAIResponsesAPIHandler,
) <-chan responsesWebsocketReadFrame {
	input := make(chan responsesWebsocketReadFrame, 16)
	go func() {
		defer close(input)
		for {
			messageType, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				cancel(errRead)
				return
			}
			if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
				continue
			}
			frameType := gjson.GetBytes(payload, "type").String()
			if frameType == "response.interrupt" && json.Valid(payload) {
				event := responsesWebsocketInterruptEvent{payload: payload}
				errInterrupt := h.forwardResponsesWebsocketInterrupt(ctx, interruptState.currentSessionID(), payload)
				switch {
				case errInterrupt == nil:
					event.forwarded = true
				case errors.Is(errInterrupt, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) && interruptState.canInterruptLocally():
					event.local = true
				default:
					if errors.Is(errInterrupt, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
						errInterrupt = fmt.Errorf("no active response to interrupt")
					}
					event.errMsg = &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errInterrupt}
				}
				select {
				case interruptState.events <- event:
				case <-ctx.Done():
					return
				}
				continue
			}
			queuedRequest := frameType == wsRequestTypeCreate || frameType == wsRequestTypeAppend
			if queuedRequest {
				interruptState.queueRequest()
			}
			select {
			case input <- responsesWebsocketReadFrame{payload: payload}:
			case <-ctx.Done():
				if queuedRequest {
					interruptState.cancelQueuedRequest()
				}
				return
			}
		}
	}()
	return input
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocketInterrupt(ctx context.Context, sessionID string, payload []byte) error {
	responseID := gjson.GetBytes(payload, "response_id")
	if responseID.Type != gjson.String || strings.TrimSpace(responseID.String()) == "" {
		return fmt.Errorf("response.interrupt requires response_id")
	}
	if h == nil || h.AuthManager == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	exec, ok := h.AuthManager.Executor("codex")
	if !ok || exec == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sender, ok := exec.(interface {
		InterruptExecutionSession(context.Context, string, []byte) error
	})
	if !ok || sender == nil {
		return fmt.Errorf("upstream executor does not support response.interrupt")
	}
	ctx = cliproxyexecutor.WithWebsocketAuthCheck(ctx, func(authID string) bool {
		auth, found := h.AuthManager.GetByID(authID)
		if !found || auth == nil {
			auth, found = h.AuthManager.GetExecutionSessionAuthByID(sessionID, authID)
		}
		return found && auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled
	})
	return sender.InterruptExecutionSession(ctx, sessionID, payload)
}

func writeResponsesWebsocketInterruptError(
	h *OpenAIResponsesAPIHandler,
	c *gin.Context,
	conn *websocket.Conn,
	wsTimelineLog *websocketTimelineBuilder,
	event responsesWebsocketInterruptEvent,
	sessionID string,
) error {
	errMsg := event.errMsg
	if errMsg == nil {
		errMsg = &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      errors.New("no active response to interrupt"),
		}
	}
	recordResponsesWebsocketAPIResponseError(h, c, errMsg)
	errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
	if errWrite == nil {
		log.Infof(
			"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
			sessionID,
			websocket.TextMessage,
			websocketPayloadEventType(errorPayload),
			websocketPayloadPreview(errorPayload),
		)
	}
	return errWrite
}
