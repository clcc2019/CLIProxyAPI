package executor

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// CodexAutoExecutor routes Codex requests to the websocket transport when the
// selected auth enables websockets and the downstream transport is websocket.
// Other requests use the legacy HTTP implementation.
//
// The dispatcher intentionally owns both transports as dependencies rather
// than embedding one and delegating the other: hot-reload scenarios replace
// the executor instance wholesale, so isolating the two implementations lets
// operators reason about their lifecycles independently.
type CodexAutoExecutor struct {
	httpExec *CodexExecutor
	wsExec   *CodexWebsocketsExecutor
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec: NewCodexExecutor(cfg),
		wsExec:   NewCodexWebsocketsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if codexUseWebsocketTransport(ctx, auth) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if codexUseWebsocketTransport(ctx, auth) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *CodexAutoExecutor) ResetExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.ResetExecutionSession(sessionID)
}

// codexUseWebsocketTransport returns true when the downstream request context
// advertises the websocket transport AND the resolved auth has websockets
// enabled. Both gates must hold — websocket upstream only makes sense when
// the downstream is a websocket too, because the executor mirrors frames.
func codexUseWebsocketTransport(ctx context.Context, auth *cliproxyauth.Auth) bool {
	if !codexWebsocketsEnabled(auth) {
		return false
	}
	return cliproxyexecutor.DownstreamWebsocket(ctx)
}

// codexWebsocketsEnabled reads the "websockets" (or legacy "websocket") flag
// off the auth's attribute bag first, then metadata. Attributes win because
// operators configure them explicitly per-auth, whereas metadata may carry
// whatever the OAuth exchange happened to produce.
func codexWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		for _, key := range []string{"websockets", "websocket"} {
			if raw := strings.TrimSpace(auth.Attributes[key]); raw != "" {
				parsed, errParse := strconv.ParseBool(raw)
				if errParse == nil {
					return parsed
				}
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	for _, key := range []string{"websockets", "websocket"} {
		raw, ok := auth.Metadata[key]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case bool:
			return v
		case string:
			parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
			if errParse == nil {
				return parsed
			}
		default:
		}
	}
	return false
}
