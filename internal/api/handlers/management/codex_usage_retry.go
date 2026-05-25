package management

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var (
	codexUsageMaxRequestRetries = 4
	codexUsageRetryBaseDelay    = 200 * time.Millisecond
)

var codexUsageRetryableTransportMarkers = []string{
	"eof",
	"connection reset",
	"connection refused",
	"server closed idle connection",
	"use of closed network connection",
	"http2: stream closed",
	"http2: server sent goaway",
	"stream error",
	"internal_error",
	"timeout",
	"deadline exceeded",
}

func codexUsageRetryableStatus(status int) bool {
	return status >= http.StatusInternalServerError && status <= 599
}

func codexUsageTransientFailure(status int, err error) bool {
	if codexUsageRetryableStatus(status) {
		return true
	}
	return codexUsageRetryableTransportError(context.Background(), err)
}

func codexUsageShouldRetry(ctx context.Context, status int, err error) bool {
	if codexUsageRetryableStatus(status) {
		return true
	}
	return codexUsageRetryableTransportError(ctx, err)
}

func codexUsageRetryableTransportError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	lower := strings.ToLower(err.Error())
	for _, marker := range codexUsageRetryableTransportMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func codexUsageSleepBeforeRetry(ctx context.Context, attempt int) error {
	if attempt <= 0 || codexUsageRetryBaseDelay <= 0 {
		return nil
	}
	delay := codexUsageRetryBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	if ctx == nil {
		time.Sleep(delay)
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
