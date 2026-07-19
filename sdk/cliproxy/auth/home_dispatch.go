package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type homeErrorEnvelope struct {
	Error *homeErrorDetail `json:"error"`
}

type homeErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

const (
	homeUpstreamModelAttributeKey     = "home_upstream_model"
	homeRequestRetryExceededErrorCode = "request_retry_exceeded"
)

func isHomeRequestRetryExceededError(err error) bool {
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(authErr.Code), homeRequestRetryExceededErrorCode)
}

func shouldReturnLastErrorOnPickFailure(homeMode bool, lastErr error, errPick error) bool {
	if lastErr == nil {
		return false
	}
	if isCredentialFailoverFailure(lastErr) {
		return false
	}
	if !homeMode {
		return true
	}
	return isHomeRequestRetryExceededError(errPick)
}

// shouldStopCredentialFailover applies the configured credential-attempt cap
// while allowing credential-scoped failures (for example, an exhausted quota)
// to move immediately to every other eligible credential. Explicitly pinned
// requests must never escape their selected credential.
func shouldStopCredentialFailover(homeMode bool, maxRetryCredentials int, attempted map[string]struct{}, lastErr error, opts cliproxyexecutor.Options) bool {
	if homeMode || maxRetryCredentials <= 0 || len(attempted) < maxRetryCredentials {
		return false
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return true
	}
	return !isCredentialFailoverFailure(lastErr)
}

func homeAuthAlreadyTried(tried map[string]struct{}, authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(tried) == 0 {
		return false
	}
	_, ok := tried[authID]
	return ok
}

func repeatedHomeAuthError() *Error {
	return &Error{
		Code:       homeRequestRetryExceededErrorCode,
		Message:    "home returned a previously tried auth",
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

type homeAuthDispatchResponse struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	AuthIndex  string `json:"auth_index"`
	UserAPIKey string `json:"user_api_key"`
	Auth       Auth   `json:"auth"`
}

type homeAuthDispatcher interface {
	HeartbeatOK() bool
	RPopAuth(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int) ([]byte, error)
}

var currentHomeDispatcher = func() homeAuthDispatcher {
	return home.Current()
}

func setHomeUserAPIKeyOnGinContext(ctx context.Context, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || ctx == nil {
		return
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Set(string, any) })
	if !ok || ginCtx == nil {
		return
	}
	ginCtx.Set("userApiKey", apiKey)
}

func homeDispatchHeaders(ctx context.Context, headers http.Header) http.Header {
	apiKey, ok := homeQueryCredentialFromContext(ctx)
	if !ok {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = http.Header{}
	}
	if out.Get("Authorization") != "" || out.Get("X-Goog-Api-Key") != "" || out.Get("X-Api-Key") != "" {
		return out
	}
	out.Set("X-Goog-Api-Key", apiKey)
	return out
}

func homeQueryCredentialFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if queryCtx, ok := ctx.Value("gin").(interface{ Query(string) string }); ok && queryCtx != nil {
		if apiKey := strings.TrimSpace(queryCtx.Query("key")); apiKey != "" {
			return apiKey, true
		}
		if apiKey := strings.TrimSpace(queryCtx.Query("auth_token")); apiKey != "" {
			return apiKey, true
		}
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return "", false
	}
	rawMetadata, ok := ginCtx.Get("accessMetadata")
	if !ok {
		return "", false
	}
	source := accessMetadataSource(rawMetadata)
	if source != "query-key" && source != "query-auth-token" {
		return "", false
	}
	rawAPIKey, ok := ginCtx.Get("userApiKey")
	if !ok {
		return "", false
	}
	apiKey := contextStringValue(rawAPIKey)
	if apiKey == "" {
		return "", false
	}
	return apiKey, true
}

func accessMetadataSource(raw any) string {
	switch v := raw.(type) {
	case map[string]string:
		return strings.TrimSpace(v["source"])
	case map[string]any:
		return contextStringValue(v["source"])
	default:
		return ""
	}
}

func contextStringValue(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func homeExecutionSessionIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func (m *Manager) clearHomeRuntimeAuths() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.clearHomeRuntimeAuthsLocked()
	m.mu.Unlock()
}

func (m *Manager) clearHomeRuntimeAuthsLocked() {
	if m == nil {
		return
	}
	m.homeRuntimeAuths = make(map[string]map[string]*Auth)
}

func (m *Manager) clearHomeRuntimeAuthsForSessionLocked(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}
	delete(m.homeRuntimeAuths, sessionID)
}

func (m *Manager) rememberHomeRuntimeAuth(sessionID string, auth *Auth) {
	sessionID = strings.TrimSpace(sessionID)
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	if m == nil || auth == nil || sessionID == "" || authID == "" || !authWebsocketsEnabled(auth) {
		return
	}
	m.mu.Lock()
	if m.homeRuntimeAuths == nil {
		m.homeRuntimeAuths = make(map[string]map[string]*Auth)
	}
	sessionAuths := m.homeRuntimeAuths[sessionID]
	if sessionAuths == nil {
		sessionAuths = make(map[string]*Auth)
		m.homeRuntimeAuths[sessionID] = sessionAuths
	}
	sessionAuths[authID] = auth.Clone()
	m.mu.Unlock()
}

func (m *Manager) homeRuntimeAuthByID(sessionID string, authID string) (*Auth, ProviderExecutor, string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, nil, "", false
	}
	m.mu.RLock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	m.mu.RUnlock()
	if auth == nil || !authWebsocketsEnabled(auth) {
		return nil, nil, "", false
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" {
		return nil, nil, "", false
	}
	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", false
	}
	return auth.Clone(), executor, providerKey, true
}

func (m *Manager) pickNextViaHome(ctx context.Context, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	executionSessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	count := homeAuthCountFromMetadata(opts.Metadata)
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && count <= 1 {
		if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" {
			_, alreadyTried := tried[pinnedAuthID]
			if !alreadyTried {
				if auth, executor, providerKey, ok := m.homeRuntimeAuthByID(executionSessionID, pinnedAuthID); ok {
					return auth, executor, providerKey, nil
				}
			}
		}
	}

	client := currentHomeDispatcher()
	if client == nil || !client.HeartbeatOK() {
		return nil, nil, "", &Error{Code: "home_unavailable", Message: "home control center unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}

	requestedModel := requestedModelFromMetadata(opts.Metadata, model)
	sessionID := ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	dispatchHeaders := homeDispatchHeaders(ctx, opts.Headers)

	raw, err := client.RPopAuth(ctx, requestedModel, sessionID, dispatchHeaders, count)
	if err != nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: err.Error(), HTTPStatus: http.StatusServiceUnavailable}
	}

	var env homeErrorEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal == nil && env.Error != nil {
		code := strings.TrimSpace(env.Error.Type)
		if code == "" {
			code = strings.TrimSpace(env.Error.Code)
		}
		msg := strings.TrimSpace(env.Error.Message)
		if msg == "" {
			msg = "home returned error"
		}
		status := http.StatusBadGateway
		switch {
		case strings.EqualFold(code, "model_not_found"):
			status = http.StatusNotFound
		case strings.EqualFold(code, "authentication_error"), strings.EqualFold(code, "unauthorized"):
			status = http.StatusUnauthorized
		}
		return nil, nil, "", &Error{Code: code, Message: msg, HTTPStatus: status}
	}

	var dispatch homeAuthDispatchResponse
	if errUnmarshal := json.Unmarshal(raw, &dispatch); errUnmarshal != nil {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
	}
	setHomeUserAPIKeyOnGinContext(ctx, dispatch.UserAPIKey)
	auth := dispatch.Auth
	if strings.TrimSpace(auth.ID) == "" {
		// Backward compatibility: older home instances returned the auth directly.
		if errUnmarshal := json.Unmarshal(raw, &auth); errUnmarshal != nil {
			return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
		}
	}
	if upstreamModel := strings.TrimSpace(dispatch.Model); upstreamModel != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 1)
		}
		auth.Attributes[homeUpstreamModelAttributeKey] = upstreamModel
	}
	if strings.TrimSpace(auth.ID) == "" {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned auth without id", HTTPStatus: http.StatusBadGateway}
	}
	if homeAuthAlreadyTried(tried, auth.ID) {
		return nil, nil, "", repeatedHomeAuthError()
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned auth without provider", HTTPStatus: http.StatusBadGateway}
	}

	homeAuthIndex := strings.TrimSpace(dispatch.AuthIndex)
	if homeAuthIndex != "" {
		auth.Index = homeAuthIndex
		auth.indexAssigned = true
	} else {
		auth.EnsureIndex()
	}

	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered", HTTPStatus: http.StatusBadGateway}
	}

	authCopy := auth.Clone()
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && authWebsocketsEnabled(authCopy) {
		m.rememberHomeRuntimeAuth(executionSessionID, authCopy)
	}
	return authCopy, executor, providerKey, nil
}
