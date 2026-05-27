package executor

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

const defaultCodexUpstreamDrainAfterDownstreamCancel = 1200 * time.Millisecond

func codexDetachUpstreamContext(ctx context.Context, cfg *config.Config) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.Background(), func() {}
	}
	drain := codexUpstreamDrainAfterDownstreamCancel(cfg)
	upstreamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if ctx.Done() == nil {
		return upstreamCtx, cancel
	}
	go func() {
		select {
		case <-ctx.Done():
			if drain <= 0 || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				cancel()
				return
			}
			timer := time.NewTimer(drain)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancel()
			case <-upstreamCtx.Done():
			}
		case <-upstreamCtx.Done():
		}
	}()
	return upstreamCtx, cancel
}

func codexUpstreamDrainAfterDownstreamCancel(cfg *config.Config) time.Duration {
	if cfg == nil {
		return defaultCodexUpstreamDrainAfterDownstreamCancel
	}
	ms := cfg.Streaming.UpstreamDrainAfterDownstreamCancelMS
	if ms < 0 {
		return 0
	}
	if ms == 0 {
		return defaultCodexUpstreamDrainAfterDownstreamCancel
	}
	return time.Duration(ms) * time.Millisecond
}

func codexRequestContextDone(ctx context.Context, err error) bool {
	if err == nil || ctx == nil || ctx.Err() == nil {
		return false
	}
	return errorsIsContextDone(err)
}

func errorsIsContextDone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "context canceled") || strings.Contains(lower, "context deadline exceeded")
}

func codexRecordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	if codexRequestContextDone(ctx, err) {
		return
	}
	helps.RecordAPIResponseError(ctx, cfg, err)
}
