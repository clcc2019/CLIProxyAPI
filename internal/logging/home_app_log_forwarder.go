package logging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	log "github.com/sirupsen/logrus"
)

const defaultHomeAppLogQueueSize = 1024

type homeAppLogClient interface {
	HeartbeatOK() bool
	RPushAppLog(ctx context.Context, payload []byte) error
}

type homeAppLogPayload struct {
	Line      string `json:"line"`
	Level     string `json:"level,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

var currentHomeAppLogClient = func() homeAppLogClient {
	return home.Current()
}

var (
	homeAppLogForwarderMu sync.Mutex
	activeHomeAppLogHook  *HomeAppLogForwarder
)

type HomeAppLogForwarderStats struct {
	Enqueued   uint64
	Dropped    uint64
	Pushed     uint64
	PushErrors uint64
}

// HomeAppLogForwarder forwards application logs to Home after the control connection is healthy.
type HomeAppLogForwarder struct {
	formatter log.Formatter
	queue     chan homeAppLogPayload
	stop      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	enabled   atomic.Bool
	enqueued  atomic.Uint64
	dropped   atomic.Uint64
	pushed    atomic.Uint64
	pushErrs  atomic.Uint64
}

// StartHomeAppLogForwarder installs a logrus hook that forwards future application logs to Home.
func StartHomeAppLogForwarder(queueSize int) *HomeAppLogForwarder {
	homeAppLogForwarderMu.Lock()
	defer homeAppLogForwarderMu.Unlock()
	if activeHomeAppLogHook != nil && activeHomeAppLogHook.enabled.Load() {
		return activeHomeAppLogHook
	}
	if queueSize <= 0 {
		queueSize = defaultHomeAppLogQueueSize
	}
	forwarder := &HomeAppLogForwarder{
		formatter: &LogFormatter{},
		queue:     make(chan homeAppLogPayload, queueSize),
		stop:      make(chan struct{}),
	}
	forwarder.enabled.Store(true)
	forwarder.wg.Add(1)
	go forwarder.run()
	log.AddHook(forwarder)
	activeHomeAppLogHook = forwarder
	return forwarder
}

// Stop disables forwarding and waits for the background sender to exit.
func (f *HomeAppLogForwarder) Stop() {
	if f == nil {
		return
	}
	f.stopOnce.Do(func() {
		f.enabled.Store(false)
		close(f.stop)
		f.wg.Wait()
		homeAppLogForwarderMu.Lock()
		if activeHomeAppLogHook == f {
			activeHomeAppLogHook = nil
		}
		homeAppLogForwarderMu.Unlock()
	})
}

func (f *HomeAppLogForwarder) Stats() HomeAppLogForwarderStats {
	if f == nil {
		return HomeAppLogForwarderStats{}
	}
	return HomeAppLogForwarderStats{
		Enqueued:   f.enqueued.Load(),
		Dropped:    f.dropped.Load(),
		Pushed:     f.pushed.Load(),
		PushErrors: f.pushErrs.Load(),
	}
}

// Levels implements logrus.Hook.
func (f *HomeAppLogForwarder) Levels() []log.Level {
	return log.AllLevels
}

// Fire implements logrus.Hook.
func (f *HomeAppLogForwarder) Fire(entry *log.Entry) error {
	if f == nil || entry == nil || !f.enabled.Load() {
		return nil
	}
	client := currentHomeAppLogClient()
	if client == nil || !client.HeartbeatOK() {
		return nil
	}
	line, errFormat := f.formatEntry(entry)
	if errFormat != nil || strings.TrimSpace(line) == "" {
		return nil
	}

	payload := homeAppLogPayload{
		Line:      line,
		Level:     entry.Level.String(),
		Timestamp: entry.Time.Format(time.RFC3339Nano),
		RequestID: appLogRequestID(entry),
	}
	select {
	case f.queue <- payload:
		f.enqueued.Add(1)
	default:
		f.dropped.Add(1)
	}
	return nil
}

func appLogRequestID(entry *log.Entry) string {
	if entry == nil {
		return ""
	}
	requestID, _ := entry.Data["request_id"].(string)
	requestID = strings.TrimSpace(requestID)
	if requestID == "--------" {
		return ""
	}
	return requestID
}

func (f *HomeAppLogForwarder) formatEntry(entry *log.Entry) (string, error) {
	formatter := f.formatter
	if formatter == nil {
		formatter = &LogFormatter{}
	}
	raw, errFormat := formatter.Format(entry)
	if errFormat != nil {
		return "", errFormat
	}
	return string(raw), nil
}

func (f *HomeAppLogForwarder) run() {
	defer f.wg.Done()
	for {
		select {
		case <-f.stop:
			return
		case payload := <-f.queue:
			f.forward(payload)
		}
	}
}

func (f *HomeAppLogForwarder) forward(payload homeAppLogPayload) {
	if !f.enabled.Load() {
		return
	}
	client := currentHomeAppLogClient()
	if client == nil || !client.HeartbeatOK() {
		return
	}
	raw, errMarshal := json.Marshal(&payload)
	if errMarshal != nil {
		return
	}
	if errPush := client.RPushAppLog(context.Background(), raw); errPush != nil {
		f.pushErrs.Add(1)
		if isHomeAppLogUnsupported(errPush) {
			f.enabled.Store(false)
		}
		return
	}
	f.pushed.Add(1)
}

func isHomeAppLogUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if msg == "" {
		return false
	}
	for {
		switch {
		case strings.Contains(msg, "unsupported key"):
			return true
		case strings.Contains(msg, "unknown command"):
			return true
		case strings.Contains(msg, "unsupported command"):
			return true
		}
		err = errors.Unwrap(err)
		if err == nil {
			return false
		}
		msg = strings.ToLower(strings.TrimSpace(err.Error()))
	}
}
