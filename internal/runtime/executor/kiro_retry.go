package executor

import (
	"context"
	"net/http"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// doKiroRequestWithFallbackRetry is intentionally a pass-through for Kiro.
// Kiro's generateAssistantResponse carries conversation state and is not
// safe to replay as a generic whole-request retry: even a transient-looking
// network/5xx error can correspond to an upstream attempt that already
// consumed quota or advanced continuation state. The retry recovery we keep
// is the explicit 401 path in Execute/ExecuteStream, which refreshes the
// token and sends the request once with fresh credentials.
//
// The function name is retained to keep the surrounding executor flow stable.
func (e *KiroExecutor) doKiroRequestWithFallbackRetry(ctx context.Context, auth *cliproxyauth.Auth, prepared *kiroPreparedRequest, accessToken string) (*http.Response, []byte, error) {
	return e.doKiroRequestWithFallback(ctx, auth, prepared, accessToken)
}
