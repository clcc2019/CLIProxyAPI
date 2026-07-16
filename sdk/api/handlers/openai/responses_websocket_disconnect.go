package openai

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

type responsesWebsocketDisconnectSubscriber interface {
	UpstreamDisconnectChanIfExists(sessionID string) <-chan error
}

type responsesWebsocketDisconnectSubscription struct {
	done         chan struct{}
	suppressNext atomic.Bool
}

// responsesWebsocketDisconnectMonitor bridges the lifetime of the currently
// active upstream execution session to its downstream websocket. A downstream
// connection can switch execution session IDs, so subscriptions are retained
// per ID while only the active ID is allowed to close the client connection.
type responsesWebsocketDisconnectMonitor struct {
	conn       *websocket.Conn
	downstream <-chan struct{}
	subscriber responsesWebsocketDisconnectSubscriber

	activeMu sync.RWMutex
	activeID string

	subscriptions sync.Map
}

func newResponsesWebsocketDisconnectMonitor(
	h *OpenAIResponsesAPIHandler,
	conn *websocket.Conn,
	downstream <-chan struct{},
) *responsesWebsocketDisconnectMonitor {
	monitor := &responsesWebsocketDisconnectMonitor{conn: conn, downstream: downstream}
	if h == nil || h.AuthManager == nil {
		return monitor
	}
	exec, ok := h.AuthManager.Executor("codex")
	if !ok || exec == nil {
		return monitor
	}
	monitor.subscriber, _ = exec.(responsesWebsocketDisconnectSubscriber)
	return monitor
}

func (monitor *responsesWebsocketDisconnectMonitor) setActive(sessionID string) {
	if monitor == nil {
		return
	}
	monitor.activeMu.Lock()
	monitor.activeID = strings.TrimSpace(sessionID)
	monitor.activeMu.Unlock()
}

func (monitor *responsesWebsocketDisconnectMonitor) active() string {
	if monitor == nil {
		return ""
	}
	monitor.activeMu.RLock()
	defer monitor.activeMu.RUnlock()
	return monitor.activeID
}

func (monitor *responsesWebsocketDisconnectMonitor) suppressNext(sessionID string) {
	if monitor == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	actual, ok := monitor.subscriptions.Load(sessionID)
	if !ok {
		return
	}
	subscription, _ := actual.(*responsesWebsocketDisconnectSubscription)
	if subscription != nil {
		subscription.suppressNext.Store(true)
	}
}

func (monitor *responsesWebsocketDisconnectMonitor) subscribe(sessionID string) {
	if monitor == nil || monitor.subscriber == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}

	subscription := &responsesWebsocketDisconnectSubscription{done: make(chan struct{})}
	for {
		actual, loaded := monitor.subscriptions.LoadOrStore(sessionID, subscription)
		if !loaded {
			break
		}
		existing, _ := actual.(*responsesWebsocketDisconnectSubscription)
		if existing == nil || existing.done == nil {
			return
		}
		select {
		case <-existing.done:
			monitor.subscriptions.Delete(sessionID)
			continue
		default:
			return
		}
	}

	disconnectCh := monitor.subscriber.UpstreamDisconnectChanIfExists(sessionID)
	if disconnectCh == nil {
		monitor.subscriptions.Delete(sessionID)
		close(subscription.done)
		return
	}
	go monitor.waitForDisconnect(sessionID, subscription, disconnectCh)
}

func (monitor *responsesWebsocketDisconnectMonitor) waitForDisconnect(
	sessionID string,
	subscription *responsesWebsocketDisconnectSubscription,
	disconnectCh <-chan error,
) {
	defer close(subscription.done)
	defer monitor.subscriptions.Delete(sessionID)
	select {
	case <-monitor.downstream:
		return
	case <-disconnectCh:
		if subscription.suppressNext.Swap(false) {
			return
		}
		if monitor.active() == sessionID && monitor.conn != nil {
			_ = monitor.conn.Close()
		}
	}
}
