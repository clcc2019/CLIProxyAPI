package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// TestKiroStreamStateSendChunkHonoursCancel is the primary regression guard
// against the "CLIProxyAPI hang" failure mode: when the downstream client
// disconnects (ctx cancelled), every send into `out` must observe the
// cancellation and bail out rather than blocking on an unread channel. The
// previous implementation used bare `s.out <- chunk` sends which would pin
// the stream goroutine and its upstream HTTP connection for the lifetime of
// the process.
func TestKiroStreamStateSendChunkHonoursCancel(t *testing.T) {
	// Unbuffered channel so the send can only succeed if someone reads it.
	out := make(chan cliproxyexecutor.StreamChunk)
	ctx, cancel := context.WithCancel(context.Background())
	state := newKiroStreamState(
		ctx,
		NewKiroExecutor(nil),
		out,
		sdktranslator.FormatClaude,
		"claude-sonnet-4.5",
		[]byte(`{}`),
		[]byte(`{}`),
		nil,
	)

	// Cancel before the send so the select must choose the ctx.Done arm.
	cancel()

	done := make(chan struct{})
	go func() {
		ok := state.sendChunk(cliproxyexecutor.StreamChunk{Payload: []byte("hello")})
		if ok {
			t.Errorf("sendChunk should return false when ctx is cancelled")
		}
		close(done)
	}()

	// The send must return within a generous timeout even though nobody is
	// reading from `out`. A broken implementation would deadlock here.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("sendChunk blocked after ctx cancellation — regression in cancel-aware send path")
	}

	if !state.cancelled {
		t.Fatalf("cancelled flag should latch after the first cancelled send")
	}

	// Subsequent sends must be cheap no-ops (don't even attempt the select).
	if ok := state.sendChunk(cliproxyexecutor.StreamChunk{Payload: []byte("again")}); ok {
		t.Fatalf("subsequent sendChunk must remain false once cancelled")
	}
}

// TestKiroStreamStateEmitNoopAfterCancel verifies the higher-level emit()
// wrapper also short-circuits once cancelled is latched. Without this, a
// burst of queued translator chunks could still attempt ctx-aware sends —
// harmless, but wasteful.
func TestKiroStreamStateEmitNoopAfterCancel(t *testing.T) {
	out := make(chan cliproxyexecutor.StreamChunk, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	state := newKiroStreamState(
		ctx,
		NewKiroExecutor(nil),
		out,
		sdktranslator.FormatClaude,
		"claude-sonnet-4.5",
		[]byte(`{}`),
		[]byte(`{}`),
		nil,
	)
	state.cancelled = true

	state.emit([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`))

	// No chunks should have been queued.
	select {
	case chunk := <-out:
		t.Fatalf("emit queued a chunk after cancel: %s / %v", chunk.Payload, chunk.Err)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing happened
	}
}

// blockingReader stalls indefinitely on Read until its embedded ctx is
// cancelled or Close is called. Used to simulate a Kiro upstream that has
// accepted our request and then stopped sending bytes without closing.
type blockingReader struct {
	closed       atomic.Bool
	closeChan    chan struct{}
	readAttempts atomic.Int32
}

func newBlockingReader() *blockingReader {
	return &blockingReader{closeChan: make(chan struct{})}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.readAttempts.Add(1)
	<-b.closeChan
	return 0, io.EOF
}

func (b *blockingReader) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		close(b.closeChan)
	}
	return nil
}

// TestKiroReadFrameWithIdleTimeoutFiresOnSilentUpstream verifies the
// watchdog closes the body and surfaces a timeout error when the reader
// stalls past the idle window. This is the defense against the production
// "process locked up" failure we diagnosed — a Kiro endpoint that accepted
// the request and then stopped responding would otherwise pin a goroutine
// and HTTP connection indefinitely.
func TestKiroReadFrameWithIdleTimeoutFiresOnSilentUpstream(t *testing.T) {
	body := newBlockingReader()
	reader := bufio.NewReaderSize(body, 4096)
	e := NewKiroExecutor(nil)

	start := time.Now()
	_, err := e.readFrameWithIdleTimeout(context.Background(), reader, body, 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !containsWord(err.Error(), "idle") {
		t.Fatalf("expected idle-timeout error, got: %v", err)
	}
	// Must fire within roughly the timeout window; allow generous slack for
	// CI but fail if we waited more than a few multiples.
	if elapsed > 2*time.Second {
		t.Fatalf("watchdog fired too slowly: %s (expected ~150ms)", elapsed)
	}
	if !body.closed.Load() {
		t.Fatalf("body was not closed by the watchdog")
	}
}

// TestKiroReadFrameWithIdleTimeoutHonoursCtx verifies ctx cancellation also
// triggers body close + prompt return, not just the idle timer.
func TestKiroReadFrameWithIdleTimeoutHonoursCtx(t *testing.T) {
	body := newBlockingReader()
	reader := bufio.NewReaderSize(body, 4096)
	ctx, cancel := context.WithCancel(context.Background())
	e := NewKiroExecutor(nil)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := e.readFrameWithIdleTimeout(ctx, reader, body, 10*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ctx.Canceled, got: %v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("ctx cancellation took too long to unblock read: %s", elapsed)
	}
	if !body.closed.Load() {
		t.Fatalf("body was not closed on ctx cancellation")
	}
}

// TestKiroReadFrameWithIdleTimeoutZeroDisablesWatchdog confirms a zero
// timeout is the opt-out path — useful for tests and for any future
// operator toggle that wants to disable the watchdog entirely.
func TestKiroReadFrameWithIdleTimeoutZeroDisablesWatchdog(t *testing.T) {
	// Feed a single valid frame so the underlying reader returns instead
	// of blocking. Use a simplified frame layout with a zero-length
	// header section and empty payload.
	frame := buildMinimalKiroFrame(t)
	reader := bufio.NewReaderSize(bytes.NewReader(frame), 4096)
	e := NewKiroExecutor(nil)

	_, err := e.readFrameWithIdleTimeout(context.Background(), reader, nil, 0)
	if err != nil {
		// A valid-looking frame might still fail to decode; we only care
		// that the watchdog didn't fire. Check the error isn't our
		// timeout marker.
		if containsWord(err.Error(), "idle") {
			t.Fatalf("watchdog fired despite zero timeout")
		}
	}
}

// buildMinimalKiroFrame returns a 12-byte-prelude event-stream frame with
// an empty payload and a CRC placeholder. Good enough to satisfy the first
// io.ReadFull call in readEventStreamMessage; the decoder will return an
// error after the prelude but the key point is the read returns promptly.
func buildMinimalKiroFrame(t *testing.T) []byte {
	t.Helper()
	// totalLen = 16 (4 prelude + 4 headers-len + 4 prelude-crc + 4 message-crc)
	// headersLen = 0
	// preludeCRC = 0 (invalid but readEventStreamMessage doesn't validate it)
	var buf bytes.Buffer
	buf.Write([]byte{0x00, 0x00, 0x00, 0x10}) // total
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00}) // headers len
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00}) // prelude crc (not validated)
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00}) // message crc
	return buf.Bytes()
}

// containsWord reports whether haystack contains the needle substring.
// Defined locally to avoid importing strings just for this check.
func containsWord(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
