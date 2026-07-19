package executor

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// codexWebsocketConnectionAttempt owns handshake observation and error
// classification shared by streaming and non-streaming websocket executions.
// Authentication refresh and HTTP fallback remain caller decisions because
// they restart different public execution methods.
type codexWebsocketConnectionAttempt struct {
	conn            *websocket.Conn
	response        *http.Response
	responseHeaders http.Header
	responseBody    []byte
	err             error
}

func (e *CodexWebsocketsExecutor) connectPreparedCodexWebsocket(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	prepared *codexPreparedWebsocketRequest,
) *codexWebsocketConnectionAttempt {
	attempt := &codexWebsocketConnectionAttempt{}
	if prepared == nil {
		attempt.err = statusErr{code: http.StatusInternalServerError, msg: "codex websocket request was not prepared"}
		return attempt
	}

	helps.RecordAPIWebsocketRequest(ctx, e.cfg, prepared.wsReqLog)
	attempt.conn, attempt.response, attempt.err = e.ensureUpstreamConn(
		ctx,
		auth,
		prepared.sess,
		prepared.authID,
		prepared.wsURL,
		prepared.wsHeaders,
	)
	if attempt.response != nil {
		attempt.responseHeaders = attempt.response.Header.Clone()
		codexPublishRateLimitsFromHeaders(ctx, auth, attempt.response.Header)
	}
	if attempt.err != nil {
		attempt.responseBody = websocketHandshakeBody(attempt.response)
		if attempt.response != nil {
			helps.RecordAPIWebsocketUpgradeRejection(
				ctx,
				e.cfg,
				websocketUpgradeRequestLog(prepared.wsReqLog),
				attempt.response.StatusCode,
				attempt.response.Header,
				attempt.responseBody,
			)
		}
		return attempt
	}

	recordAPIWebsocketHandshake(ctx, e.cfg, attempt.response)
	if prepared.sess != nil && attempt.response != nil {
		prepared.sess.rememberTurnStateHeader(attempt.response.Header)
	}
	if prepared.sess == nil {
		logCodexWebsocketConnected(prepared.executionSessionID, prepared.authID, prepared.wsURL)
	}
	return attempt
}

func (attempt *codexWebsocketConnectionAttempt) statusCode() int {
	if attempt == nil || attempt.response == nil {
		return 0
	}
	return attempt.response.StatusCode
}

func (attempt *codexWebsocketConnectionAttempt) upgradeRequired() bool {
	return attempt.statusCode() == http.StatusUpgradeRequired
}

func (attempt *codexWebsocketConnectionAttempt) unauthorized() bool {
	return attempt.statusCode() == http.StatusUnauthorized
}

func (attempt *codexWebsocketConnectionAttempt) failure(
	ctx context.Context,
	e *CodexWebsocketsExecutor,
	auth *cliproxyauth.Auth,
) error {
	if attempt == nil {
		return statusErr{code: http.StatusInternalServerError, msg: "codex websocket connection attempt is nil"}
	}
	if statusCode := attempt.statusCode(); statusCode > 0 {
		codexPublishRateLimitsFromErrorBody(ctx, auth, attempt.responseBody)
		return statusErrWithHeaders{
			statusErr: newCodexStatusErr(statusCode, attempt.responseBody),
			headers:   attempt.responseHeaders.Clone(),
		}
	}
	if e != nil {
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial", attempt.err)
	}
	return attempt.err
}
