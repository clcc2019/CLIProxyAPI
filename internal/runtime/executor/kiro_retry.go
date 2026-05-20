package executor

import (
	"context"
	"net/http"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const kiroMonthlyRequestCountSameAuthRetries = 3

// doKiroRequestWithFallbackRetry avoids generic Kiro request replay because
// generateAssistantResponse carries conversation state. The only same-auth
// replay allowed here is Kiro's explicit 402 MONTHLY_REQUEST_COUNT response,
// which is rejected before generation state advances. After three same-auth
// retries, the wrapped error tells the conductor to fail over to another auth.
func (e *KiroExecutor) doKiroRequestWithFallbackRetry(ctx context.Context, auth *cliproxyauth.Auth, prepared *kiroPreparedRequest, accessToken string) (*http.Response, []byte, error) {
	var lastPayload []byte
	for attempt := 0; ; attempt++ {
		resp, payload, err := e.doKiroRequestWithFallback(ctx, auth, prepared, accessToken)
		lastPayload = payload
		if err == nil || !isKiroMonthlyRequestCount402Error(err) || attempt >= kiroMonthlyRequestCountSameAuthRetries {
			return resp, payload, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, lastPayload, ctxErr
		}
	}
}
