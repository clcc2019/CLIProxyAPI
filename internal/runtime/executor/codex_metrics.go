package executor

import (
	"sync/atomic"
)

// codexMetricsState holds process-wide counters for the codex proxy path.
// All fields are atomic.Int64 so callers can bump them from any goroutine
// without acquiring a lock. The counters are intentionally zero-dependency
// (no prometheus, no expvar) so they compose with whatever metrics sink the
// operator wires in later: a periodic snapshot call is enough to surface the
// full set to logs, /debug endpoints, or a dedicated exporter.
//
// Design notes:
//   - Counters are monotonic. Resets (if ever added) should happen through a
//     dedicated Reset() method, never by overwriting individual fields, so
//     snapshots stay self-consistent.
//   - New counters should be added to both the struct and CodexMetricsSnapshot
//     below. Keeping the snapshot type hand-maintained beats reflection: we
//     trade one line of maintenance for a 50x speedup on hot-path exports.
type codexMetricsState struct {
	dedupeHit         atomic.Int64 // incremented when a non-stream request is served from an in-flight peer
	dedupeMiss        atomic.Int64 // incremented when a non-stream request actually hit the upstream
	memoBodyHit       atomic.Int64 // normalizeCodexFinalUpstreamBody cache hits
	memoBodyMiss      atomic.Int64 // normalizeCodexFinalUpstreamBody cache misses
	memoPromptHit     atomic.Int64 // prompt-cache resolution memo hits
	memoPromptMiss    atomic.Int64 // prompt-cache resolution memo misses
	wsUpstreamError   atomic.Int64 // readUpstreamLoop surfaced a terminal error to the consumer
	wsUpstreamBinary  atomic.Int64 // readUpstreamLoop rejected an unexpected binary frame
	wsActiveChMissing atomic.Int64 // readUpstreamLoop had a frame but no active consumer channel
	terminalIncomplete atomic.Int64 // upstream sent response.incomplete
	terminalFailed    atomic.Int64 // upstream sent response.failed
	captureTruncated  atomic.Int64 // request-log capture dropped tail bytes over budget
}

// codexMetrics is the process-wide singleton. The zero value is ready to use.
var codexMetrics codexMetricsState

// CodexMetricsSnapshot is the immutable value returned by CodexMetrics().
// Exported so callers outside this package (admin handlers, debug endpoints)
// can read a consistent tuple of counters without importing internal types.
type CodexMetricsSnapshot struct {
	DedupeHit          int64
	DedupeMiss         int64
	MemoBodyHit        int64
	MemoBodyMiss       int64
	MemoPromptHit      int64
	MemoPromptMiss     int64
	WSUpstreamError    int64
	WSUpstreamBinary   int64
	WSActiveChMissing  int64
	TerminalIncomplete int64
	TerminalFailed     int64
	CaptureTruncated   int64
}

// CodexMetrics returns a point-in-time snapshot of the codex counters.
// The snapshot is not atomic across fields: under sustained load, individual
// counters may move between the per-field loads below. That is acceptable for
// observability; callers that need a consistent across-field view should
// synchronise externally.
func CodexMetrics() CodexMetricsSnapshot {
	return CodexMetricsSnapshot{
		DedupeHit:          codexMetrics.dedupeHit.Load(),
		DedupeMiss:         codexMetrics.dedupeMiss.Load(),
		MemoBodyHit:        codexMetrics.memoBodyHit.Load(),
		MemoBodyMiss:       codexMetrics.memoBodyMiss.Load(),
		MemoPromptHit:      codexMetrics.memoPromptHit.Load(),
		MemoPromptMiss:     codexMetrics.memoPromptMiss.Load(),
		WSUpstreamError:    codexMetrics.wsUpstreamError.Load(),
		WSUpstreamBinary:   codexMetrics.wsUpstreamBinary.Load(),
		WSActiveChMissing:  codexMetrics.wsActiveChMissing.Load(),
		TerminalIncomplete: codexMetrics.terminalIncomplete.Load(),
		TerminalFailed:     codexMetrics.terminalFailed.Load(),
		CaptureTruncated:   codexMetrics.captureTruncated.Load(),
	}
}

// ResetCodexMetrics zeroes all codex counters. Exported for tests; production
// code should never call this — counters are monotonic by design and
// operators expect cumulative values.
func ResetCodexMetrics() {
	codexMetrics.dedupeHit.Store(0)
	codexMetrics.dedupeMiss.Store(0)
	codexMetrics.memoBodyHit.Store(0)
	codexMetrics.memoBodyMiss.Store(0)
	codexMetrics.memoPromptHit.Store(0)
	codexMetrics.memoPromptMiss.Store(0)
	codexMetrics.wsUpstreamError.Store(0)
	codexMetrics.wsUpstreamBinary.Store(0)
	codexMetrics.wsActiveChMissing.Store(0)
	codexMetrics.terminalIncomplete.Store(0)
	codexMetrics.terminalFailed.Store(0)
	codexMetrics.captureTruncated.Store(0)
}
