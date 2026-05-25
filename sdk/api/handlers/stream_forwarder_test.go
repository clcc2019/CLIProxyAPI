package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type countingFlusher struct {
	count int
}

func (f *countingFlusher) Flush() {
	f.count++
}

func newStreamForwardTestContext(t *testing.T) (*gin.Context, context.CancelFunc) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqCtx, cancel := context.WithCancel(req.Context())
	ctx.Request = req.WithContext(reqCtx)
	return ctx, cancel
}

func TestForwardStreamFlushesEveryChunkByDefault(t *testing.T) {
	ctx, cancelRequest := newStreamForwardTestContext(t)
	defer cancelRequest()

	data := make(chan []byte, 2)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("a")
	data <- []byte("b")
	close(data)
	close(errs)

	flusher := &countingFlusher{}
	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	handler.ForwardStream(ctx, flusher, func(error) {}, data, errs, StreamForwardOptions{
		WriteChunk: func([]byte) bool { return true },
	})

	if flusher.count != 3 {
		t.Fatalf("flush count = %d, want 3", flusher.count)
	}
}

func TestForwardStreamFlushesTerminalErrorImmediately(t *testing.T) {
	ctx, cancelRequest := newStreamForwardTestContext(t)
	defer cancelRequest()

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{Error: context.Canceled}
	close(data)
	close(errs)

	flusher := &countingFlusher{}
	wroteError := false
	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	handler.ForwardStream(ctx, flusher, func(error) {}, data, errs, StreamForwardOptions{
		WriteTerminalError: func(*interfaces.ErrorMessage) { wroteError = true },
	})

	if !wroteError {
		t.Fatal("terminal error writer was not called")
	}
	if flusher.count != 1 {
		t.Fatalf("flush count = %d, want 1", flusher.count)
	}
}

func TestForwardStreamRejectsDataWithoutChunkWriter(t *testing.T) {
	ctx, cancelRequest := newStreamForwardTestContext(t)
	defer cancelRequest()

	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("payload")
	close(data)
	close(errs)

	flusher := &countingFlusher{}
	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	var canceledErr error
	wroteError := false
	handler.ForwardStream(ctx, flusher, func(err error) { canceledErr = err }, data, errs, StreamForwardOptions{
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			wroteError = true
			if errMsg == nil || errMsg.StatusCode != http.StatusInternalServerError || !errors.Is(errMsg.Error, errNilStreamChunkWriter) {
				t.Fatalf("terminal error = %#v, want missing writer 500", errMsg)
			}
		},
	})

	if !errors.Is(canceledErr, errNilStreamChunkWriter) {
		t.Fatalf("cancel error = %v, want %v", canceledErr, errNilStreamChunkWriter)
	}
	if !wroteError {
		t.Fatal("terminal error writer was not called")
	}
	if flusher.count != 1 {
		t.Fatalf("flush count = %d, want 1", flusher.count)
	}
}

func TestForwardStreamRejectsNilDataAndErrorChannels(t *testing.T) {
	ctx, cancelRequest := newStreamForwardTestContext(t)
	defer cancelRequest()

	flusher := &countingFlusher{}
	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	var canceledErr error
	wroteError := false
	done := make(chan struct{})
	errCh := make(chan string, 1)
	go func() {
		handler.ForwardStream(ctx, flusher, func(err error) { canceledErr = err }, nil, nil, StreamForwardOptions{
			WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
				wroteError = true
				if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway || !errors.Is(errMsg.Error, errNilStreamChannels) {
					errCh <- "terminal error did not match nil stream channel 502"
				}
			},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ForwardStream hung with nil data and error channels")
	}
	select {
	case msg := <-errCh:
		t.Fatal(msg)
	default:
	}
	if !errors.Is(canceledErr, errNilStreamChannels) {
		t.Fatalf("cancel error = %v, want %v", canceledErr, errNilStreamChannels)
	}
	if !wroteError {
		t.Fatal("terminal error writer was not called")
	}
	if flusher.count != 1 {
		t.Fatalf("flush count = %d, want 1", flusher.count)
	}
}

func TestForwardStreamRejectsNilDataAfterErrorChannelCloses(t *testing.T) {
	ctx, cancelRequest := newStreamForwardTestContext(t)
	defer cancelRequest()

	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	flusher := &countingFlusher{}
	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	var canceledErr error
	wroteError := false
	handler.ForwardStream(ctx, flusher, func(err error) { canceledErr = err }, nil, errs, StreamForwardOptions{
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			wroteError = true
			if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway || !errors.Is(errMsg.Error, errNilStreamChannels) {
				t.Fatalf("terminal error = %#v, want nil stream channel 502", errMsg)
			}
		},
	})

	if !errors.Is(canceledErr, errNilStreamChannels) {
		t.Fatalf("cancel error = %v, want %v", canceledErr, errNilStreamChannels)
	}
	if !wroteError {
		t.Fatal("terminal error writer was not called")
	}
	if flusher.count != 1 {
		t.Fatalf("flush count = %d, want 1", flusher.count)
	}
}

func TestForwardStreamFlushesKeepAlive(t *testing.T) {
	ctx, cancelRequest := newStreamForwardTestContext(t)
	defer cancelRequest()

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	defer close(data)
	defer close(errs)

	flusher := &countingFlusher{}
	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	interval := time.Millisecond
	wroteKeepAlive := make(chan struct{})
	done := make(chan struct{})
	go func() {
		handler.ForwardStream(ctx, flusher, func(error) {}, data, errs, StreamForwardOptions{
			KeepAliveInterval: &interval,
			WriteKeepAlive: func() {
				close(wroteKeepAlive)
				cancelRequest()
			},
		})
		close(done)
	}()

	select {
	case <-wroteKeepAlive:
	case <-time.After(time.Second):
		t.Fatal("keep-alive was not written")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ForwardStream did not stop after request cancellation")
	}
	if flusher.count == 0 {
		t.Fatal("keep-alive did not flush")
	}
}
