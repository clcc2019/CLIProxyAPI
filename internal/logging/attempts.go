package logging

import (
	"context"
	"strings"
	"sync"
	"time"
)

// maxRecordedAttempts bounds the per-request attempt history. Credential
// failover is already capped by max-retry-credentials, but that cap is
// operator-configurable and can be unlimited (0), so this is a hard backstop
// against an unbounded slice on a pathological retry storm.
const maxRecordedAttempts = 32

type upstreamAttemptsKey struct{}

// UpstreamAttempt records one upstream try — a single (credential, model)
// execution — and why it did not produce the final response.
//
// One request can burn several credentials before succeeding or giving up. The
// aggregate usage record only reports the last outcome, so without this history
// "why did this request end in 502" is unanswerable after the fact: the
// credentials that failed first, and their distinct status codes, are gone.
type UpstreamAttempt struct {
	// Attempt is the 1-based ordinal of this try within the request.
	Attempt int `json:"attempt"`
	// AuthID / AuthLabel identify the credential that was tried.
	AuthID    string `json:"auth_id,omitempty"`
	AuthLabel string `json:"auth_label,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	// Status is the upstream HTTP status, when the failure carried one.
	// Transport failures leave it zero.
	Status int `json:"status,omitempty"`
	// Kind classifies the attempt so operators can filter without parsing
	// Message. See the attemptKind* constants.
	Kind string `json:"kind,omitempty"`
	// Message is the trimmed upstream error text.
	Message string `json:"message,omitempty"`
	// ElapsedMs is the wall-clock time this attempt consumed.
	ElapsedMs int64 `json:"elapsed_ms,omitempty"`
}

type upstreamAttemptsHolder struct {
	mu        sync.Mutex
	attempts  []UpstreamAttempt
	truncated int
}

// WithUpstreamAttempts installs the attempt recorder on ctx. It is idempotent,
// so nested wrapping keeps the outermost holder and the whole failover chain
// accumulates in one place.
func WithUpstreamAttempts(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(upstreamAttemptsKey{}).(*upstreamAttemptsHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, upstreamAttemptsKey{}, &upstreamAttemptsHolder{})
}

// RecordUpstreamAttempt appends one failed attempt. It is a no-op when no
// holder is installed, so callers never need to branch on whether request
// logging is enabled.
func RecordUpstreamAttempt(ctx context.Context, attempt UpstreamAttempt) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(upstreamAttemptsKey{}).(*upstreamAttemptsHolder)
	if !ok || holder == nil {
		return
	}
	attempt.Message = strings.TrimSpace(attempt.Message)
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if len(holder.attempts) >= maxRecordedAttempts {
		// Keep the first N rather than the last N: the earliest failures are
		// what explain a cascade, while the tail is usually the same error
		// repeating. The dropped count is surfaced so the log never implies
		// the request only made maxRecordedAttempts tries.
		holder.truncated++
		return
	}
	attempt.Attempt = len(holder.attempts) + 1
	holder.attempts = append(holder.attempts, attempt)
}

// UpstreamAttempts returns a copy of the recorded attempts plus the number
// dropped by the cap.
func UpstreamAttempts(ctx context.Context) ([]UpstreamAttempt, int) {
	if ctx == nil {
		return nil, 0
	}
	holder, ok := ctx.Value(upstreamAttemptsKey{}).(*upstreamAttemptsHolder)
	if !ok || holder == nil {
		return nil, 0
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if len(holder.attempts) == 0 {
		return nil, holder.truncated
	}
	out := make([]UpstreamAttempt, len(holder.attempts))
	copy(out, holder.attempts)
	return out, holder.truncated
}

// AttemptElapsed converts a start time into the ElapsedMs field, clamping a
// zero start (unknown) to 0 rather than reporting time since the epoch.
func AttemptElapsed(startedAt time.Time) int64 {
	if startedAt.IsZero() {
		return 0
	}
	elapsed := time.Since(startedAt)
	if elapsed < 0 {
		return 0
	}
	return elapsed.Milliseconds()
}
