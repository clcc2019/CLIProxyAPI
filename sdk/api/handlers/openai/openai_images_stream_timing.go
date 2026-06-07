package openai

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

type imagesStreamExecutionResult struct {
	Data            <-chan []byte
	UpstreamHeaders http.Header
	Errs            <-chan *interfaces.ErrorMessage
}

type imageStreamTiming struct {
	keepAliveInterval   time.Duration
	dataIntervalTimeout time.Duration
	lastDataAt          time.Time
	lastWriteAt         time.Time
	keepAliveTicker     *time.Ticker
	dataIntervalTicker  *time.Ticker
	keepAliveC          <-chan time.Time
	dataIntervalC       <-chan time.Time
}

func newImageStreamTiming(keepAliveInterval, dataIntervalTimeout time.Duration) *imageStreamTiming {
	now := time.Now()
	timing := &imageStreamTiming{
		keepAliveInterval:   keepAliveInterval,
		dataIntervalTimeout: dataIntervalTimeout,
		lastDataAt:          now,
		lastWriteAt:         now,
	}
	if keepAliveInterval > 0 {
		timing.keepAliveTicker = time.NewTicker(keepAliveInterval)
		timing.keepAliveC = timing.keepAliveTicker.C
	}
	if dataIntervalTimeout > 0 {
		timing.dataIntervalTicker = time.NewTicker(dataIntervalTimeout)
		timing.dataIntervalC = timing.dataIntervalTicker.C
	}
	return timing
}

func (h *OpenAIAPIHandler) newImageStreamTiming() *imageStreamTiming {
	return newImageStreamTiming(h.imageStreamKeepAliveInterval(), h.imageStreamDataIntervalTimeout())
}

func (t *imageStreamTiming) Stop() {
	if t == nil {
		return
	}
	if t.keepAliveTicker != nil {
		t.keepAliveTicker.Stop()
	}
	if t.dataIntervalTicker != nil {
		t.dataIntervalTicker.Stop()
	}
}

func (t *imageStreamTiming) MarkData() {
	if t != nil {
		t.lastDataAt = time.Now()
	}
}

func (t *imageStreamTiming) MarkWrite() {
	if t != nil {
		t.lastWriteAt = time.Now()
	}
}

func (t *imageStreamTiming) KeepAliveDue(now time.Time) bool {
	return t != nil && t.keepAliveInterval > 0 && now.Sub(t.lastWriteAt) >= t.keepAliveInterval
}

func (t *imageStreamTiming) IdleTimedOut(now time.Time) bool {
	return t != nil && t.dataIntervalTimeout > 0 && now.Sub(t.lastDataAt) >= t.dataIntervalTimeout
}

func (h *OpenAIAPIHandler) imageStreamKeepAliveInterval() time.Duration {
	seconds := 0
	if h != nil && h.Cfg != nil {
		seconds = h.Cfg.ImageStreamKeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (h *OpenAIAPIHandler) imageStreamDataIntervalTimeout() time.Duration {
	seconds := 0
	if h != nil && h.Cfg != nil {
		seconds = h.Cfg.ImageStreamDataIntervalTimeoutSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func maybeWriteImageStreamKeepAlive(timing *imageStreamTiming, now time.Time, writeKeepAlive func()) {
	if timing == nil || !timing.KeepAliveDue(now) || writeKeepAlive == nil {
		return
	}
	writeKeepAlive()
}

func imageStreamIdleTimeoutError(timeout time.Duration) *interfaces.ErrorMessage {
	if timeout <= 0 {
		timeout = 0
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusGatewayTimeout,
		Error:      fmt.Errorf("upstream image stream idle for %s", timeout),
	}
}

func waitImagesStreamExecution(c *gin.Context, timing *imageStreamTiming, writeKeepAlive func(), execute func() imagesStreamExecutionResult) (imagesStreamExecutionResult, bool) {
	resultChan := make(chan imagesStreamExecutionResult, 1)
	go func() {
		resultChan <- execute()
	}()

	for {
		select {
		case <-c.Request.Context().Done():
			return imagesStreamExecutionResult{}, true
		case result := <-resultChan:
			return result, false
		case now := <-timing.keepAliveC:
			maybeWriteImageStreamKeepAlive(timing, now, writeKeepAlive)
		}
	}
}
