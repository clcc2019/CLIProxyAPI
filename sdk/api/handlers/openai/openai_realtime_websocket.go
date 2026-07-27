package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const realtimeWebsocketUpstreamErrorBodyLimit = 64 << 10

// RealtimeWebsocket proxies the official OpenAI Realtime WebSocket API. It
// selects an OpenAI-compatible API-key credential, then relays every text and
// binary frame unchanged between the caller and that upstream.
func (h *OpenAIResponsesAPIHandler) RealtimeWebsocket(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		writeRealtimeWebsocketError(c, http.StatusBadRequest, "model is required")
		return
	}
	if h == nil || h.AuthManager == nil {
		writeRealtimeWebsocketError(c, http.StatusServiceUnavailable, "no OpenAI-compatible credential is available")
		return
	}

	providers, _ := responsesWebsocketModelRoute(model)
	opts := coreexecutor.Options{
		Headers: c.Request.Header.Clone(),
		Query:   c.Request.URL.Query(),
	}
	auth, upstreamModel, err := h.AuthManager.SelectDirectWebsocketAuth(c.Request.Context(), providers, model, opts)
	if err != nil {
		writeRealtimeWebsocketError(c, http.StatusServiceUnavailable, "no OpenAI-compatible credential is available")
		return
	}

	upstreamURL, err := openAIRealtimeWebsocketURL(auth.Attributes["base_url"], upstreamModel)
	if err != nil {
		writeRealtimeWebsocketError(c, http.StatusBadGateway, err.Error())
		return
	}
	upstreamHeaders, err := openAIRealtimeWebsocketHeaders(c.Request, auth.Attributes)
	if err != nil {
		writeRealtimeWebsocketError(c, http.StatusServiceUnavailable, err.Error())
		return
	}

	ctx, cancel := h.GetContextWithCancel(h, c, context.Background())
	defer cancel()
	dialer := runtimeexecutor.NewProxyAwareWebsocketDialer(realtimeWebsocketConfig(h), auth)
	upstream, response, err := dialer.DialContext(ctx, upstreamURL, upstreamHeaders)
	if err != nil {
		writeRealtimeWebsocketDialError(c, response, err)
		return
	}
	defer func() { _ = upstream.Close() }()
	upstream.SetReadLimit(maxResponsesWebsocketInboundBytes)
	upstream.EnableWriteCompression(false)

	downstream, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, realtimeWebsocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	defer func() { _ = downstream.Close() }()
	downstream.SetReadLimit(maxResponsesWebsocketInboundBytes)

	relayRealtimeWebsocket(downstream, upstream)
}

func realtimeWebsocketConfig(h *OpenAIResponsesAPIHandler) *internalconfig.Config {
	cfg := &internalconfig.Config{}
	if h != nil && h.Cfg != nil {
		cfg.SDKConfig = *h.Cfg
	}
	return cfg
}

func openAIRealtimeWebsocketURL(baseURL, model string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("selected OpenAI-compatible credential has no base_url")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed == nil || strings.TrimSpace(parsed.Host) == "" || parsed.User != nil {
		return "", fmt.Errorf("selected OpenAI-compatible credential has an invalid base_url")
	}
	if parsed.Fragment != "" {
		return "", fmt.Errorf("selected OpenAI-compatible credential has an invalid base_url")
	}

	path := strings.TrimRight(strings.TrimSpace(parsed.Path), "/")
	if !strings.HasSuffix(path, "/realtime") {
		switch {
		case path == "" && strings.EqualFold(parsed.Hostname(), "api.openai.com"):
			path = "/v1/realtime"
		case path == "":
			path = "/realtime"
		default:
			path += "/realtime"
		}
	}
	parsed.Path = path

	switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("selected OpenAI-compatible credential has an unsupported base_url scheme")
	}

	values := parsed.Query()
	values.Set("model", model)
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func openAIRealtimeWebsocketHeaders(request *http.Request, attrs map[string]string) (http.Header, error) {
	headers := make(http.Header, 4)
	if len(attrs) > 0 {
		util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)
	}
	if request != nil {
		if safetyID := strings.TrimSpace(request.Header.Get("OpenAI-Safety-Identifier")); safetyID != "" {
			headers.Set("OpenAI-Safety-Identifier", safetyID)
		}
	}
	if apiKey := strings.TrimSpace(attrs["api_key"]); apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	if strings.TrimSpace(headers.Get("Authorization")) == "" {
		return nil, fmt.Errorf("selected OpenAI-compatible credential has no API key")
	}
	return headers, nil
}

func realtimeWebsocketUpgradeHeaders(request *http.Request) http.Header {
	headers := websocketUpgradeHeaders(request)
	for _, protocol := range websocket.Subprotocols(request) {
		if strings.EqualFold(strings.TrimSpace(protocol), "realtime") {
			headers.Set("Sec-WebSocket-Protocol", "realtime")
			break
		}
	}
	return headers
}

func relayRealtimeWebsocket(downstream, upstream *websocket.Conn) {
	errs := make(chan relayRealtimeWebsocketResult, 2)
	downstreamWriter := &realtimeWebsocketWriter{conn: downstream}
	upstreamWriter := &realtimeWebsocketWriter{conn: upstream}
	go relayRealtimeWebsocketFrames(downstream, downstreamWriter, upstreamWriter, errs)
	go relayRealtimeWebsocketFrames(upstream, upstreamWriter, downstreamWriter, errs)
	result := <-errs
	propagateRealtimeWebsocketClose(result.peer, result.err)
}

type relayRealtimeWebsocketResult struct {
	peer *realtimeWebsocketWriter
	err  error
}

type realtimeWebsocketWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *realtimeWebsocketWriter) writeMessage(messageType int, payload []byte) error {
	if w == nil || w.conn == nil {
		return fmt.Errorf("realtime websocket destination is nil")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout))
	err := w.conn.WriteMessage(messageType, payload)
	_ = w.conn.SetWriteDeadline(time.Time{})
	return err
}

func relayRealtimeWebsocketFrames(source *websocket.Conn, sourceWriter, destination *realtimeWebsocketWriter, results chan<- relayRealtimeWebsocketResult) {
	for {
		messageType, payload, err := source.ReadMessage()
		if err != nil {
			results <- relayRealtimeWebsocketResult{peer: destination, err: err}
			return
		}
		err = destination.writeMessage(messageType, payload)
		if err != nil {
			results <- relayRealtimeWebsocketResult{peer: sourceWriter, err: err}
			return
		}
	}
}

func propagateRealtimeWebsocketClose(destination *realtimeWebsocketWriter, err error) {
	if destination == nil || destination.conn == nil || err == nil {
		return
	}
	code := websocket.CloseInternalServerErr
	text := ""
	if closeErr, ok := err.(*websocket.CloseError); ok {
		code = closeErr.Code
		text = closeErr.Text
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	_ = destination.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(responsesWebsocketWriteTimeout))
}

func writeRealtimeWebsocketError(c *gin.Context, status int, message string) {
	if c == nil {
		return
	}
	errorType := "invalid_request_error"
	if status >= http.StatusInternalServerError {
		errorType = "server_error"
	}
	c.JSON(status, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: message, Type: errorType}})
}

func writeRealtimeWebsocketDialError(c *gin.Context, response *http.Response, _ error) {
	status := http.StatusBadGateway
	message := "failed to connect to the Realtime upstream"
	if response != nil {
		if response.StatusCode >= http.StatusBadRequest && response.StatusCode < http.StatusInternalServerError {
			status = response.StatusCode
		}
		if response.Body != nil {
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(io.LimitReader(response.Body, realtimeWebsocketUpstreamErrorBodyLimit))
			if err == nil && len(strings.TrimSpace(string(body))) > 0 {
				c.Data(status, "application/json", handlers.BuildErrorResponseBody(status, string(body)))
				return
			}
		}
	}
	writeRealtimeWebsocketError(c, status, message)
}
