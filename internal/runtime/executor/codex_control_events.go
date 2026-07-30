package executor

import (
	"context"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	// codexEventRateLimits is an account-scoped control event. The proxy
	// persists it against the selected credential instead of forwarding it as
	// an ordinary Responses event.
	codexEventRateLimits = "codex.rate_limits"
	// codexEventResponsesWebsocketTiming is diagnostic data emitted only when
	// the upstream timing header is enabled. Like the official client, consume
	// it locally rather than exposing an implementation-specific event to API
	// consumers.
	codexEventResponsesWebsocketTiming = "responsesapi.websocket_timing"
)

// codexConsumesUpstreamControlEvent applies the proxy's explicit contract for
// upstream-only Codex events. It returns true when callers must not pass the
// payload into downstream response translators.
//
// Other Codex extension events intentionally remain transparent: their safety,
// moderation, and model-verification semantics may be understood by a
// downstream Codex-compatible client even when this proxy has no local UI for
// them.
func codexConsumesUpstreamControlEvent(ctx context.Context, auth *cliproxyauth.Auth, payload []byte) bool {
	switch codexEventType(payload) {
	case codexEventRateLimits:
		codexPublishRateLimitsFromEvent(ctx, auth, payload)
		return true
	case codexEventResponsesWebsocketTiming:
		return true
	default:
		return false
	}
}
