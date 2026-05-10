package codex

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	refreshRetryBaseDelay  = 200 * time.Millisecond
	refreshRetryJitterSpan = 100 * time.Millisecond
	// refreshRetryMaxDelay caps the exponential backoff so callers passing a
	// large maxRetries do not stall for tens of seconds on a single attempt.
	refreshRetryMaxDelay = 10 * time.Second
)

var (
	refreshRetryJitter = func() time.Duration {
		return randomizedRefreshRetryJitter(refreshRetryJitterSpan)
	}
	refreshRetryWait = waitForRefreshRetry
)

func refreshRetryDelay(attempt int) time.Duration {
	return refreshRetryDelayWithJitter(attempt, refreshRetryJitter())
}

func refreshRetryDelayWithJitter(attempt int, jitter time.Duration) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return exponentialRefreshRetryDelay(attempt) + jitter
}

func exponentialRefreshRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	// Guard against overflow when attempt is large: 1<<62 already overflows
	// time.Duration arithmetic; cap first.
	if attempt >= 16 {
		return refreshRetryMaxDelay
	}
	delay := refreshRetryBaseDelay * time.Duration(1<<(attempt-1))
	if delay > refreshRetryMaxDelay {
		return refreshRetryMaxDelay
	}
	return delay
}

func randomizedRefreshRetryJitter(span time.Duration) time.Duration {
	if span <= 0 {
		return 0
	}
	maxOffset := int64(span)
	return time.Duration(rand.Int64N(maxOffset*2+1)) - span
}

func waitForRefreshRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
