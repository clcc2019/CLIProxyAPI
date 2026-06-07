// Package handlers provides core API handler functionality for the CLI Proxy API server.
// It includes common types, client management, load balancing, and error handling
// shared across all API endpoint handlers.
package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// ErrorResponse represents a standard error response format for the API.
// It contains a single ErrorDetail field.
type ErrorResponse struct {
	// Error contains detailed information about the error that occurred.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides specific information about an error that occurred.
// It includes a human-readable message, an error type, and an optional error code.
type ErrorDetail struct {
	// Message is a human-readable message providing more details about the error.
	Message string `json:"message"`

	// Type is the category of error that occurred (e.g., "invalid_request_error").
	Type string `json:"type"`

	// Code is a short code identifying the error, if applicable.
	Code string `json:"code,omitempty"`
}

const idempotencyKeyMetadataKey = "idempotency_key"

const (
	defaultStreamingKeepAliveSeconds = 0
	defaultStreamingBootstrapRetries = 0
	maxLoggedHandlerAPIResponseBytes = 4 << 20
)

const (
	apiResponseContextKey          = "API_RESPONSE"
	apiResponseTimestampContextKey = "API_RESPONSE_TIMESTAMP"
	apiResponseTruncatedContextKey = "API_RESPONSE_TRUNCATED"
	apiResponseLogTruncatedMarker  = "\n...[api response log truncated]...\n"
)

type pinnedAuthContextKey struct{}
type selectedAuthCallbackContextKey struct{}
type executionSessionContextKey struct{}
type disallowFreeAuthContextKey struct{}

var (
	backgroundContext = context.Background()
	todoContext       = context.TODO()
)

// WithPinnedAuthID returns a child context that requests execution on a specific auth ID.
func WithPinnedAuthID(ctx context.Context, authID string) context.Context {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pinnedAuthContextKey{}, authID)
}

// WithSelectedAuthIDCallback returns a child context that receives the selected auth ID.
func WithSelectedAuthIDCallback(ctx context.Context, callback func(string)) context.Context {
	if callback == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, selectedAuthCallbackContextKey{}, callback)
}

// WithExecutionSessionID returns a child context tagged with a long-lived execution session ID.
func WithExecutionSessionID(ctx context.Context, sessionID string) context.Context {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionSessionContextKey{}, sessionID)
}

// WithDisallowFreeAuth returns a child context that requests skipping known free-tier credentials.
func WithDisallowFreeAuth(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, disallowFreeAuthContextKey{}, true)
}

// BuildErrorResponseBody builds an OpenAI-compatible JSON error response body.
// If errText is already valid JSON, it is returned after credential redaction to
// preserve upstream error shape without leaking secrets.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}
	errText = string(util.RedactSensitiveLogBytes([]byte(errText)))

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return util.RedactSensitiveLogBytes([]byte(trimmed))
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: errText,
			Type:    errType,
			Code:    code,
		},
	})
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText))
	}
	return payload
}

// StreamingKeepAliveInterval returns the SSE keep-alive interval for this server.
// Returning 0 disables keep-alives (default when unset).
func StreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := defaultStreamingKeepAliveSeconds
	if cfg != nil {
		seconds = cfg.Streaming.KeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// NonStreamingKeepAliveInterval returns the keep-alive interval for non-streaming responses.
// Returning 0 disables keep-alives (default when unset).
func NonStreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.NonStreamKeepAliveInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// StreamingBootstrapRetries returns how many times a streaming request may be retried before any bytes are sent.
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil {
		retries = cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

// PassthroughHeadersEnabled returns whether upstream response headers should be forwarded to clients.
// Default is false.
func PassthroughHeadersEnabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.PassthroughHeaders
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// Only include it when the client explicitly provides it.
	key := ""
	requestPath := ""
	contentType := ""
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
			contentType = strings.TrimSpace(ginCtx.GetHeader("Content-Type"))
			if ginCtx.Request.URL != nil {
				requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
			}
		}
	}

	var meta map[string]any
	appendMeta := func(key string, value any) {
		if meta == nil {
			meta = make(map[string]any, 5)
		}
		meta[key] = value
	}
	if key != "" {
		appendMeta(idempotencyKeyMetadataKey, key)
	}
	if requestPath != "" {
		appendMeta(coreexecutor.RequestPathMetadataKey, requestPath)
	}
	if contentType != "" {
		appendMeta(coreexecutor.RequestContentTypeMetadataKey, contentType)
	}
	if pinnedAuthID := pinnedAuthIDFromContext(ctx); pinnedAuthID != "" {
		appendMeta(coreexecutor.PinnedAuthMetadataKey, pinnedAuthID)
	}
	if clientPrincipal := clientPrincipalFromContext(ctx); clientPrincipal != "" {
		appendMeta(coreexecutor.ClientPrincipalMetadataKey, hashClientPrincipal(clientPrincipal))
	}
	if selectedCallback := selectedAuthIDCallbackFromContext(ctx); selectedCallback != nil {
		appendMeta(coreexecutor.SelectedAuthCallbackMetadataKey, selectedCallback)
	}
	if executionSessionID := executionSessionIDFromContext(ctx); executionSessionID != "" {
		appendMeta(coreexecutor.ExecutionSessionMetadataKey, executionSessionID)
	}
	if disallowFreeAuthFromContext(ctx) {
		appendMeta(coreexecutor.DisallowFreeAuthMetadataKey, true)
	}
	return meta
}

func clientPrincipalFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return ""
	}
	raw, exists := ginCtx.Get("apiKey")
	if !exists || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func hashClientPrincipal(principal string) string {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(principal))
	return hex.EncodeToString(sum[:])
}

func setReasoningEffortMetadata(meta map[string]any, handlerType, model string, rawJSON []byte) {
	if meta == nil {
		return
	}
	effort := thinking.ExtractReasoningEffort(rawJSON, handlerType, model)
	if effort == "" {
		return
	}
	meta[coreexecutor.ReasoningEffortMetadataKey] = effort
}

func setServiceTierMetadata(meta map[string]any, rawJSON []byte) {
	if meta == nil {
		return
	}
	serviceTier := coreusage.DefaultServiceTier
	node := gjson.GetBytes(rawJSON, "service_tier")
	if node.Exists() {
		value := strings.TrimSpace(node.String())
		if value != "" {
			serviceTier = value
		}
	}
	meta[coreexecutor.ServiceTierMetadataKey] = serviceTier
}

// requestHeadersFromContext extracts the original HTTP request headers from the gin context
// embedded in the provided context. This allows session affinity selectors to read
// client headers used for session affinity.
func requestHeadersFromContext(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil || ginCtx.Request.Header == nil {
		return nil
	}
	return ginCtx.Request.Header.Clone()
}

func pinnedAuthIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(pinnedAuthContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func selectedAuthIDCallbackFromContext(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(selectedAuthCallbackContextKey{})
	if callback, ok := raw.(func(string)); ok && callback != nil {
		return callback
	}
	return nil
}

func executionSessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(executionSessionContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func disallowFreeAuthFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw, ok := ctx.Value(disallowFreeAuthContextKey{}).(bool)
	return ok && raw
}

// BaseAPIHandler contains the handlers for API endpoints.
// It holds a pool of clients to interact with the backend service and manages
// load balancing, client selection, and configuration.
type BaseAPIHandler struct {
	// AuthManager manages auth lifecycle and execution in the new architecture.
	AuthManager *coreauth.Manager

	// Cfg holds the current application configuration.
	Cfg *config.SDKConfig
}

// NewBaseAPIHandlers creates a new API handlers instance.
// It takes a slice of clients and configuration as input.
//
// Parameters:
//   - cliClients: A slice of AI service clients
//   - cfg: The application configuration
//
// Returns:
//   - *BaseAPIHandler: A new API handlers instance
func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler {
	return &BaseAPIHandler{
		Cfg:         cfg,
		AuthManager: authManager,
	}
}

// UpdateClients updates the handlers' client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) { h.Cfg = cfg }

// GetAlt extracts the 'alt' parameter from the request query string.
// It checks both 'alt' and '$alt' parameters and returns the appropriate value.
//
// Parameters:
//   - c: The Gin context containing the HTTP request
//
// Returns:
//   - string: The alt parameter value, or empty string if it's "sse"
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string {
	var alt string
	var hasAlt bool
	alt, hasAlt = c.GetQuery("alt")
	if !hasAlt {
		alt, _ = c.GetQuery("$alt")
	}
	if alt == "sse" {
		return ""
	}
	return alt
}

// GetContextWithCancel creates a new context with cancellation capabilities.
// It embeds the Gin context into the new context for later use.
// The returned cancel function also handles logging the API response if request logging is enabled.
//
// Parameters:
//   - handler: The API handler associated with the request.
//   - c: The Gin context of the current request.
//   - ctx: The parent context (caller values/deadlines are preserved; request context adds cancellation and request ID).
//
// Returns:
//   - context.Context: The new context with cancellation and embedded values.
//   - APIHandlerCancelFunc: A function to cancel the context and log the response.
func (h *BaseAPIHandler) GetContextWithCancel(handler interfaces.APIHandler, c *gin.Context, ctx context.Context) (context.Context, APIHandlerCancelFunc) {
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = backgroundContext
	}

	var requestCtx context.Context
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}

	baseCtx := parentCtx
	bridgeRequestCancel := false
	if requestCtx != nil {
		switch {
		case parentCtx == requestCtx:
			baseCtx = requestCtx
		case parentCtx == backgroundContext || parentCtx == todoContext:
			baseCtx = requestCtx
		default:
			bridgeRequestCancel = true
		}
	}

	if logging.GetRequestID(baseCtx) == "" {
		if requestID := logging.GetRequestID(requestCtx); requestID != "" {
			baseCtx = logging.WithRequestID(baseCtx, requestID)
		} else if requestID := logging.GetGinRequestID(c); requestID != "" {
			baseCtx = logging.WithRequestID(baseCtx, requestID)
		}
	}

	newCtx, cancelContext := context.WithCancel(baseCtx)
	cancel := cancelContext
	if bridgeRequestCancel {
		stopRequestCancel := context.AfterFunc(requestCtx, cancelContext)
		cancel = func() {
			stopRequestCancel()
			cancelContext()
		}
	}
	newCtx = context.WithValue(newCtx, "gin", c)
	requestLogEnabled := h != nil && h.Cfg != nil && h.Cfg.RequestLog
	return newCtx, func(params ...interface{}) {
		if requestLogEnabled && len(params) == 1 {
			existingText := currentAPIResponseText(c)
			if strings.TrimSpace(existingText) != "" {
				switch params[0].(type) {
				case error, string:
					cancel()
					return
				}
			}

			var payload []byte
			switch data := params[0].(type) {
			case []byte:
				payload = data
			case error:
				if data != nil {
					payload = []byte(data.Error())
				}
			case string:
				payload = []byte(data)
			}
			if len(payload) > 0 {
				trimmedPayload := string(bytes.TrimSpace(payload))
				if trimmedPayload != "" && existingText != "" && strings.Contains(existingText, trimmedPayload) {
					cancel()
					return
				}
				appendAPIResponse(c, payload)
			}
		}

		cancel()
	}
}

// StartNonStreamingKeepAlive is intentionally a no-op until non-streaming
// keep-alives can be sent without committing the final HTTP status code.
// Writing body bytes before the upstream response arrives makes later upstream
// errors look like successful HTTP 200 responses to clients.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	return func() {}
}

// AppendAPIResponseLog preserves any previously captured API response and appends new data.
func AppendAPIResponseLog(c *gin.Context, data []byte) {
	appendAPIResponse(c, data)
}

// appendAPIResponse preserves any previously captured API response and appends new data.
func appendAPIResponse(c *gin.Context, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}

	// Capture timestamp on first API response
	if _, exists := c.Get(apiResponseTimestampContextKey); !exists {
		c.Set(apiResponseTimestampContextKey, time.Now())
	}

	if existing, exists := c.Get(apiResponseContextKey); exists {
		switch value := existing.(type) {
		case *strings.Builder:
			if value != nil {
				appendLimitedAPIResponseBytes(c, value, data)
				return
			}
		case []byte:
			builder := &strings.Builder{}
			builder.Grow(len(value))
			_, _ = builder.Write(value)
			c.Set(apiResponseContextKey, builder)
			appendLimitedAPIResponseBytes(c, builder, data)
			return
		case string:
			builder := &strings.Builder{}
			builder.Grow(len(value))
			_, _ = builder.WriteString(value)
			c.Set(apiResponseContextKey, builder)
			appendLimitedAPIResponseBytes(c, builder, data)
			return
		}
	}

	builder := &strings.Builder{}
	appendLimitedAPIResponseBytes(c, builder, data)
	c.Set(apiResponseContextKey, builder)
}

func appendLimitedAPIResponseBytes(c *gin.Context, builder *strings.Builder, data []byte) {
	if c == nil || builder == nil || len(data) == 0 {
		return
	}
	data = util.RedactSensitiveLogBytes(data)
	if len(data) == 0 {
		return
	}
	truncated := ginContextBool(c, apiResponseTruncatedContextKey)
	if truncated {
		return
	}
	if builder.Len() > 0 {
		text := builder.String()
		if text[len(text)-1] != '\n' {
			truncated = appendLimitedAPIResponseText(builder, "\n", truncated)
		}
	}
	truncated = appendLimitedAPIResponseBytesToBuilder(builder, data, truncated)
	if truncated {
		c.Set(apiResponseTruncatedContextKey, true)
	}
}

func appendLimitedAPIResponseString(c *gin.Context, builder *strings.Builder, data string) {
	if c == nil || builder == nil || data == "" {
		return
	}
	truncated := ginContextBool(c, apiResponseTruncatedContextKey)
	if truncated {
		return
	}
	if builder.Len() > 0 {
		text := builder.String()
		if text[len(text)-1] != '\n' {
			truncated = appendLimitedAPIResponseText(builder, "\n", truncated)
		}
	}
	truncated = appendLimitedAPIResponseText(builder, data, truncated)
	if truncated {
		c.Set(apiResponseTruncatedContextKey, true)
	}
}

func appendLimitedAPIResponseBytesToBuilder(builder *strings.Builder, data []byte, truncated bool) bool {
	if builder == nil || len(data) == 0 {
		return truncated
	}
	remaining := maxLoggedHandlerAPIResponseBytes - builder.Len()
	if remaining <= 0 {
		if !truncated {
			builder.WriteString(apiResponseLogTruncatedMarker)
		}
		return true
	}
	if len(data) <= remaining {
		builder.Write(data)
		return truncated
	}
	builder.Write(data[:remaining])
	if !truncated {
		builder.WriteString(apiResponseLogTruncatedMarker)
	}
	return true
}

func appendLimitedAPIResponseText(builder *strings.Builder, text string, truncated bool) bool {
	if builder == nil || text == "" {
		return truncated
	}
	remaining := maxLoggedHandlerAPIResponseBytes - builder.Len()
	if remaining <= 0 {
		if !truncated {
			builder.WriteString(apiResponseLogTruncatedMarker)
		}
		return true
	}
	if len(text) <= remaining {
		builder.WriteString(text)
		return truncated
	}
	builder.WriteString(text[:remaining])
	if !truncated {
		builder.WriteString(apiResponseLogTruncatedMarker)
	}
	return true
}

func ginContextBool(c *gin.Context, key string) bool {
	if c == nil {
		return false
	}
	value, exists := c.Get(key)
	if !exists {
		return false
	}
	typed, _ := value.(bool)
	return typed
}

func currentAPIResponseText(c *gin.Context) string {
	if c == nil {
		return ""
	}
	existing, exists := c.Get(apiResponseContextKey)
	if !exists {
		return ""
	}
	return apiResponseText(existing)
}

func apiResponseText(value any) string {
	switch typed := value.(type) {
	case []byte:
		return string(typed)
	case string:
		return typed
	case *strings.Builder:
		if typed != nil {
			return typed.String()
		}
	}
	return ""
}

// ExecuteWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, false)
}

// ExecuteImageWithAuthManager executes an OpenAI-compatible image endpoint request.
func (h *BaseAPIHandler) ExecuteImageWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, true)
}

func (h *BaseAPIHandler) executeWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, allowImageModel bool) ([]byte, http.Header, *interfaces.ErrorMessage) {
	if ctx == nil {
		ctx = context.Background()
	}
	providers, normalizedModel, errMsg := h.getRequestDetailsWithOptions(modelName, allowImageModel)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	if !clientModelAllowedForContext(ctx, normalizedModel) {
		return nil, nil, clientModelAccessError(normalizedModel)
	}
	reqMeta := requestExecutionMetadata(ctx)
	if reqMeta == nil {
		reqMeta = make(map[string]any, 2)
	}
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	if PassthroughHeadersEnabled(h.Cfg) {
		reqMeta[coreexecutor.NeedResponseHeadersMetadataKey] = true
	}
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		Headers:         requestHeadersFromContext(ctx),
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
	}
	opts.Metadata = reqMeta
	resp, err := h.AuthManager.Execute(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		return nil, nil, errorMessageFromError(err, http.StatusInternalServerError)
	}
	if !PassthroughHeadersEnabled(h.Cfg) {
		return resp.Payload, nil, nil
	}
	return resp.Payload, FilterUpstreamHeaders(resp.Headers), nil
}

// ExecuteCountWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	if ctx == nil {
		ctx = context.Background()
	}
	providers, normalizedModel, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	if !clientModelAllowedForContext(ctx, normalizedModel) {
		return nil, nil, clientModelAccessError(normalizedModel)
	}
	reqMeta := requestExecutionMetadata(ctx)
	if reqMeta == nil {
		reqMeta = make(map[string]any, 2)
	}
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	if PassthroughHeadersEnabled(h.Cfg) {
		reqMeta[coreexecutor.NeedResponseHeadersMetadataKey] = true
	}
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		Headers:         requestHeadersFromContext(ctx),
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
	}
	opts.Metadata = reqMeta
	resp, err := h.AuthManager.ExecuteCount(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		return nil, nil, errorMessageFromError(err, http.StatusInternalServerError)
	}
	if !PassthroughHeadersEnabled(h.Cfg) {
		return resp.Payload, nil, nil
	}
	return resp.Payload, FilterUpstreamHeaders(resp.Headers), nil
}

// ExecuteStreamWithAuthManager executes a streaming request via the core auth manager.
// This path is the only supported execution route.
// The returned http.Header carries upstream response headers captured before streaming begins.
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, false)
}

// ExecuteImageStreamWithAuthManager executes a streaming OpenAI-compatible image endpoint request.
func (h *BaseAPIHandler) ExecuteImageStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, true)
}

func (h *BaseAPIHandler) executeStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, allowImageModel bool) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	if ctx == nil {
		ctx = context.Background()
	}
	providers, normalizedModel, errMsg := h.getRequestDetailsWithOptions(modelName, allowImageModel)
	if errMsg != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	if !clientModelAllowedForContext(ctx, normalizedModel) {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- clientModelAccessError(normalizedModel)
		close(errChan)
		return nil, nil, errChan
	}
	reqMeta := requestExecutionMetadata(ctx)
	if reqMeta == nil {
		reqMeta = make(map[string]any, 2)
	}
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	if PassthroughHeadersEnabled(h.Cfg) {
		reqMeta[coreexecutor.NeedResponseHeadersMetadataKey] = true
	}
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          true,
		Alt:             alt,
		Headers:         requestHeadersFromContext(ctx),
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
	}
	opts.Metadata = reqMeta
	streamResult, err := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errorMessageFromError(err, http.StatusInternalServerError)
		close(errChan)
		return nil, nil, errChan
	}
	if streamResult == nil || streamResult.Chunks == nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		err := invalidStreamResultHandlerError("auth manager returned stream result without chunks")
		errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err}
		close(errChan)
		return nil, nil, errChan
	}
	passthroughHeadersEnabled := PassthroughHeadersEnabled(h.Cfg)
	// Capture upstream headers from the initial connection synchronously before the goroutine starts.
	// Keep a mutable map so bootstrap retries can replace it before first payload is sent.
	var upstreamHeaders http.Header
	if passthroughHeadersEnabled {
		upstreamHeaders = cloneHeader(FilterUpstreamHeaders(streamResult.Headers))
		if upstreamHeaders == nil {
			upstreamHeaders = make(http.Header)
		}
	}
	chunks := streamResult.Chunks
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		defer close(dataChan)
		defer close(errChan)
		sentPayload := false
		bootstrapRetries := 0
		maxBootstrapRetries := StreamingBootstrapRetries(h.Cfg)

		sendErr := func(msg *interfaces.ErrorMessage) bool {
			select {
			case <-ctx.Done():
				return false
			case errChan <- msg:
				return true
			}
		}

		sendData := func(chunk []byte) bool {
			select {
			case <-ctx.Done():
				return false
			case dataChan <- chunk:
				return true
			}
		}

		bootstrapEligible := func(err error) bool {
			status := statusFromError(err)
			if status == 0 {
				return true
			}
			switch status {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired,
				http.StatusRequestTimeout, http.StatusTooManyRequests:
				return true
			default:
				return status >= http.StatusInternalServerError
			}
		}

	outer:
		for {
			for {
				var chunk coreexecutor.StreamChunk
				var ok bool
				select {
				case <-ctx.Done():
					return
				case chunk, ok = <-chunks:
				}
				if !ok {
					if !sentPayload {
						streamErr := invalidStreamResultHandlerError("auth manager stream closed before sending payload")
						_ = sendErr(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: streamErr})
					}
					return
				}
				if chunk.Err != nil {
					streamErr := chunk.Err
					// Safe bootstrap recovery: if the upstream fails before any payload bytes are sent,
					// retry a few times (to allow auth rotation / transient recovery) and then attempt model fallback.
					if !sentPayload {
						if bootstrapRetries < maxBootstrapRetries && bootstrapEligible(streamErr) {
							bootstrapRetries++
							retryResult, retryErr := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
							if retryErr == nil {
								if retryResult == nil || retryResult.Chunks == nil {
									streamErr = invalidStreamResultHandlerError("auth manager returned retry stream result without chunks")
								} else {
									if passthroughHeadersEnabled {
										replaceHeader(upstreamHeaders, FilterUpstreamHeaders(retryResult.Headers))
									}
									chunks = retryResult.Chunks
									continue outer
								}
							} else {
								streamErr = enrichAuthSelectionError(retryErr, providers, normalizedModel)
							}
						}
					}

					_ = sendErr(errorMessageFromError(streamErr, http.StatusInternalServerError))
					return
				}
				if len(chunk.Payload) > 0 {
					if handlerType == "openai-response" {
						if err := validateSSEDataJSON(chunk.Payload); err != nil {
							_ = sendErr(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
							return
						}
					}
					sentPayload = true
					if okSendData := sendData(cloneBytes(chunk.Payload)); !okSendData {
						return
					}
				}
			}
		}
	}()
	return dataChan, upstreamHeaders, errChan
}

var sseDoneMarkerBytes = []byte("[DONE]")

func validateSSEDataJSON(chunk []byte) error {
	for len(chunk) > 0 {
		line := chunk
		if idx := bytes.IndexByte(chunk, '\n'); idx >= 0 {
			line = chunk[:idx]
			chunk = chunk[idx+1:]
		} else {
			chunk = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, sseDoneMarkerBytes) {
			continue
		}
		if json.Valid(data) {
			continue
		}
		const max = 512
		preview := data
		if len(preview) > max {
			preview = preview[:max]
		}
		return fmt.Errorf("invalid SSE data JSON (len=%d): %q", len(data), preview)
	}
	return nil
}

func statusFromError(err error) int {
	if err == nil {
		return 0
	}
	var se interface{ StatusCode() int }
	if errors.As(err, &se) && se != nil {
		if code := se.StatusCode(); code > 0 {
			return code
		}
	}
	return 0
}

func errorMessageFromError(err error, fallbackStatus int) *interfaces.ErrorMessage {
	if fallbackStatus <= 0 {
		fallbackStatus = http.StatusInternalServerError
	}
	status := fallbackStatus
	if code := statusFromError(err); code > 0 {
		status = code
	}
	return &interfaces.ErrorMessage{
		StatusCode: status,
		Error:      err,
		Addon:      filteredErrorHeaders(err),
	}
}

func filteredErrorHeaders(err error) http.Header {
	if err == nil {
		return nil
	}
	var he interface{ Headers() http.Header }
	if !errors.As(err, &he) || he == nil {
		return nil
	}
	return FilterUpstreamHeaders(he.Headers())
}

func (h *BaseAPIHandler) getRequestDetails(modelName string) (providers []string, normalizedModel string, err *interfaces.ErrorMessage) {
	return h.getRequestDetailsWithOptions(modelName, false)
}

func (h *BaseAPIHandler) getRequestDetailsWithOptions(modelName string, allowImageModel bool) (providers []string, normalizedModel string, err *interfaces.ErrorMessage) {
	resolvedModelName := modelName
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			resolvedModelName = modelName
		} else {
			resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
			if initialSuffix.HasSuffix {
				resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
			} else {
				resolvedModelName = resolvedBase
			}
		}
	} else {
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			resolvedModelName = modelName
		} else {
			resolvedModelName = util.ResolveAutoModel(modelName)
		}
	}

	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)

	if isOpenAIImageOnlyModel(baseModel) {
		return nil, "", &interfaces.ErrorMessage{
			StatusCode: http.StatusServiceUnavailable,
			Error:      fmt.Errorf("model %s is only supported on /v1/images/generations, /v1/images/edits, and /v1/images/variations", baseModel),
		}
	}

	if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return []string{"home"}, resolvedModelName, nil
	}

	providers = util.GetProviderName(baseModel)
	// Fallback: if baseModel has no provider but differs from resolvedModelName,
	// try using the full model name. This handles edge cases where custom models
	// may be registered with their full suffixed name (e.g., "my-model(8192)").
	// Evaluated in Story 11.8: This fallback is intentionally preserved to support
	// custom model registrations that include thinking suffixes.
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}

	if len(providers) == 0 {
		return nil, "", &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("unknown provider for model %s", modelName)}
	}

	// The thinking suffix is preserved in the model name itself, so no
	// metadata-based configuration passing is needed.
	return providers, resolvedModelName, nil
}

func isOpenAIImageOnlyModel(modelName string) bool {
	const prefix = "gpt-image-"
	modelName = strings.TrimSpace(modelName)
	if len(modelName) < len(prefix) {
		return false
	}
	return strings.EqualFold(modelName[:len(prefix)], prefix)
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func replaceHeader(dst http.Header, src http.Header) {
	for key := range dst {
		delete(dst, key)
	}
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

func invalidStreamResultHandlerError(message string) error {
	return &coreauth.Error{Code: "invalid_stream_result", Message: message, Retryable: true, HTTPStatus: http.StatusBadGateway}
}

func enrichAuthSelectionError(err error, providers []string, model string) error {
	if err == nil {
		return nil
	}

	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return err
	}

	code := strings.TrimSpace(authErr.Code)
	if code != "auth_not_found" && code != "auth_unavailable" {
		return err
	}

	providerText := strings.Join(providers, ",")
	if providerText == "" {
		providerText = "unknown"
	}
	modelText := strings.TrimSpace(model)
	if modelText == "" {
		modelText = "unknown"
	}

	baseMessage := strings.TrimSpace(authErr.Message)
	if baseMessage == "" {
		baseMessage = "no auth available"
	}
	detail := fmt.Sprintf("%s (providers=%s, model=%s)", baseMessage, providerText, modelText)

	// Clarify the most common alias confusion between Anthropic route names and internal provider keys.
	if strings.Contains(","+providerText+",", ",claude,") {
		detail += "; check Claude auth/key session and cooldown state via /v0/management/auth-files"
	}

	status := authErr.HTTPStatus
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}

	return &coreauth.Error{
		Code:       authErr.Code,
		Message:    detail,
		Retryable:  authErr.Retryable,
		HTTPStatus: status,
	}
}

// WriteErrorResponse writes an error message to the response writer using the HTTP status embedded in the message.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.Addon != nil && PassthroughHeadersEnabled(h.Cfg) {
		for key, values := range FilterUpstreamHeaders(msg.Addon) {
			if len(values) == 0 {
				continue
			}
			c.Writer.Header().Del(key)
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
	}

	errText := http.StatusText(status)
	if msg != nil && msg.Error != nil {
		if v := strings.TrimSpace(msg.Error.Error()); v != "" {
			errText = v
		}
	}

	body := BuildErrorResponseBody(status, errText)
	// Append first to preserve upstream response logs, then drop duplicate payloads if already recorded.
	previous := currentAPIResponseText(c)
	appendAPIResponse(c, body)
	trimmedErrText := strings.TrimSpace(errText)
	trimmedBody := string(bytes.TrimSpace(body))
	if previous != "" {
		if (trimmedErrText != "" && strings.Contains(previous, trimmedErrText)) ||
			(trimmedBody != "" && strings.Contains(previous, trimmedBody)) {
			c.Set(apiResponseContextKey, []byte(previous))
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage) {
	if h.Cfg.RequestLog {
		if ginContext, ok := ctx.Value("gin").(*gin.Context); ok {
			if apiResponseErrors, isExist := ginContext.Get("API_RESPONSE_ERROR"); isExist {
				if slicesAPIResponseError, isOk := apiResponseErrors.([]*interfaces.ErrorMessage); isOk {
					slicesAPIResponseError = append(slicesAPIResponseError, err)
					ginContext.Set("API_RESPONSE_ERROR", slicesAPIResponseError)
				}
			} else {
				// Create new response data entry
				ginContext.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{err})
			}
		}
	}
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...interface{})
