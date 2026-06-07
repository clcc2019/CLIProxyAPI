package management

import (
	"context"
	"errors"
	"testing"
)

func TestCodexUsageRetryableTransportErrorMatchesMixedCaseMarker(t *testing.T) {
	err := errors.New("HTTP2: Stream Closed")
	if !codexUsageRetryableTransportError(context.Background(), err) {
		t.Fatal("mixed-case HTTP/2 stream marker should be retryable")
	}
}

func TestCodexUsageRetryableTransportErrorHonorsCanceledParentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if codexUsageRetryableTransportError(ctx, errors.New("HTTP2: Stream Closed")) {
		t.Fatal("transport error should not retry after parent context is canceled")
	}
}

func BenchmarkCodexUsageRetryableTransportErrorMarker(b *testing.B) {
	err := errors.New("HTTP2: Stream Closed")
	for b.Loop() {
		if !codexUsageRetryableTransportError(context.Background(), err) {
			b.Fatal("expected retryable transport error")
		}
	}
}
