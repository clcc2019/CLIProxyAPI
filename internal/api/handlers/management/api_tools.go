package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const defaultAPICallTimeout = 60 * time.Second

var apiCallTransportCache sync.Map

type apiCallRequest struct {
	AuthIndexSnake  *string           `json:"auth_index"`
	AuthIndexCamel  *string           `json:"authIndex"`
	AuthIndexPascal *string           `json:"AuthIndex"`
	Provider        string            `json:"provider"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Header          map[string]string `json:"header"`
	Data            string            `json:"data"`
}

type apiCallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

// APICall makes a generic HTTP request on behalf of the management API caller.
// It is protected by the management middleware.
//
// Endpoint:
//
//	POST /v0/management/api-call
//
// Authentication:
//
//	Same as other management APIs (requires a management key and remote-management rules).
//	You can provide the key via:
//	- Authorization: Bearer <key>
//	- X-Management-Key: <key>
//
// Request JSON:
//   - auth_index / authIndex / AuthIndex (optional):
//     The credential "auth_index" from GET /v0/management/auth-files (or other endpoints returning it).
//     If omitted or not found, credential-specific proxy/token substitution is skipped.
//   - provider (optional):
//     Provider hint for applying provider-specific default headers in management-side test calls.
//     Currently used for Codex / Claude User-Agent defaults when header["User-Agent"] is omitted.
//   - method (required): HTTP method, e.g. GET, POST, PUT, PATCH, DELETE.
//   - url (required): Absolute URL including scheme and host, e.g. "https://api.example.com/v1/ping".
//   - header (optional): Request headers map.
//     Supports magic variable "$TOKEN$" which is replaced using the selected credential:
//     1) metadata.access_token
//     2) attributes.api_key
//     3) metadata.token / metadata.id_token / metadata.cookie
//     Example: {"Authorization":"Bearer $TOKEN$"}.
//     Note: if you need to override the HTTP Host header, set header["Host"].
//   - data (optional): Raw request body as string (useful for POST/PUT/PATCH).
//
// Proxy selection (highest priority first):
//  1. Selected credential proxy_url
//  2. Global config proxy-url
//  3. Direct connect (environment proxies are not used)
//
// Response JSON (returned with HTTP 200 when the APICall itself succeeds):
//   - status_code: Upstream HTTP status code.
//   - header: Upstream response headers.
//   - body: Upstream response body as string.
//
// Example:
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"GET","url":"https://api.example.com/v1/ping","header":{"Authorization":"Bearer $TOKEN$"}}'
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer 831227" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"POST","url":"https://api.example.com/v1/fetchAvailableModels","header":{"Authorization":"Bearer $TOKEN$","Content-Type":"application/json","User-Agent":"cliproxyapi"},"data":"{}"}'
func (h *Handler) APICall(c *gin.Context) {
	var body apiCallRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing method"})
		return
	}

	urlStr := strings.TrimSpace(body.URL)
	if urlStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing url"})
		return
	}
	parsedURL, errParseURL := url.Parse(urlStr)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := h.authByIndex(authIndex)
	if h.handleCodexUsageAPICall(c, method, parsedURL, auth) {
		return
	}

	reqHeaders := body.Header
	if reqHeaders == nil {
		reqHeaders = map[string]string{}
	}
	h.applyAPICallDefaultHeaders(reqHeaders, auth, body.Provider)

	var hostOverride string
	var token string
	var tokenResolved bool
	var tokenErr error
	for key, value := range reqHeaders {
		if !strings.Contains(value, "$TOKEN$") {
			continue
		}
		if !tokenResolved {
			token, tokenErr = h.resolveTokenForAuth(c.Request.Context(), auth)
			tokenResolved = true
		}
		if auth != nil && token == "" {
			if tokenErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "auth token refresh failed"})
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth token not found"})
			return
		}
		if token == "" {
			continue
		}
		reqHeaders[key] = strings.ReplaceAll(value, "$TOKEN$", token)
	}

	var requestBody io.Reader
	if body.Data != "" {
		requestBody = strings.NewReader(body.Data)
	}

	req, errNewRequest := http.NewRequestWithContext(c.Request.Context(), method, urlStr, requestBody)
	if errNewRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to build request"})
		return
	}

	for key, value := range reqHeaders {
		if strings.EqualFold(key, "host") {
			hostOverride = strings.TrimSpace(value)
			continue
		}
		req.Header.Set(key, value)
	}
	if hostOverride != "" {
		req.Host = hostOverride
	}

	httpClient := &http.Client{
		Timeout: defaultAPICallTimeout,
	}
	httpClient.Transport = h.apiCallTransport(auth)

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		log.WithError(errDo).Debug("management APICall request failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	respBody, errReadAll := helps.ReadNonStreamResponseBody(resp.Body)
	if errReadAll != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}

	c.JSON(http.StatusOK, apiCallResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       string(respBody),
	})
}

// handleCodexUsageAPICall routes the management UI's generic quota probe
// through the guarded Codex usage path. The UI issues one /api-call per auth
// with Promise.all, so using the generic transport here would bypass usage
// caching, retry policy, shared concurrency control, and 502 sanitization.
func (h *Handler) handleCodexUsageAPICall(c *gin.Context, method string, target *url.URL, auth *coreauth.Auth) bool {
	if c == nil || !isCodexUsageAPICall(method, target, auth) {
		return false
	}

	ctx := c.Request.Context()
	auth = h.refreshCodexUsageAuthIfNeeded(ctx, auth)
	payload, upstreamStatus, err := h.fetchCodexUsageWithCache(ctx, auth, codexUsageRequestOptions{
		ttl: codexUsageCacheDefaultTTL,
	})
	if err != nil {
		if codexUsageTransientFailure(upstreamStatus, err) {
			payload = codexUsageUnavailablePayload(err, upstreamStatus)
		} else {
			status := upstreamStatus
			if status <= 0 {
				status = http.StatusBadGateway
			}
			writeAPICallJSONResponse(c, status, gin.H{"error": "codex usage request failed"})
			return true
		}
	}
	mergeCodexUsageLocalFields(payload, auth)
	writeAPICallJSONResponse(c, http.StatusOK, payload)
	return true
}

func isCodexUsageAPICall(method string, target *url.URL, auth *coreauth.Auth) bool {
	if !strings.EqualFold(strings.TrimSpace(method), http.MethodGet) || target == nil || auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	usageTarget, err := url.Parse(codexUsageURL)
	if err != nil || usageTarget.Scheme == "" || usageTarget.Host == "" {
		return false
	}
	return strings.EqualFold(target.Scheme, usageTarget.Scheme) &&
		strings.EqualFold(target.Host, usageTarget.Host) &&
		target.EscapedPath() == usageTarget.EscapedPath()
}

func writeAPICallJSONResponse(c *gin.Context, statusCode int, payload any) {
	if c == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode response"})
		return
	}
	c.JSON(http.StatusOK, apiCallResponse{
		StatusCode: statusCode,
		Header: map[string][]string{
			"Content-Type":                  {"application/json"},
			"X-CPA-Codex-Usage-Guard":       {"active"},
			"X-CPA-Codex-Usage-Outage-TTL":  {codexUsageOutageCooldown.String()},
			"X-CPA-Codex-Usage-Concurrency": {strconv.Itoa(codexManagementUpstreamConcurrency)},
		},
		Body: string(body),
	})
}

func firstNonEmptyString(values ...*string) string {
	for _, v := range values {
		if v == nil {
			continue
		}
		if out := strings.TrimSpace(*v); out != "" {
			return out
		}
	}
	return ""
}

func hasAPICallHeader(headers map[string]string, name string) bool {
	if len(headers) == 0 {
		return false
	}
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return true
		}
	}
	return false
}

func resolveAPICallProvider(auth *coreauth.Auth, providerHint string) string {
	if provider := strings.ToLower(strings.TrimSpace(providerHint)); provider != "" {
		return provider
	}
	if auth == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func (h *Handler) applyAPICallDefaultHeaders(headers map[string]string, auth *coreauth.Auth, providerHint string) {
	if headers == nil {
		return
	}
	if hasAPICallHeader(headers, "User-Agent") {
		return
	}

	switch resolveAPICallProvider(auth, providerHint) {
	case "codex":
		userAgent := strings.TrimSpace(readConfigValue(h, func(cfg *config.Config) string {
			return cfg.CodexHeaderDefaults.UserAgent
		}))
		if userAgent == "" {
			userAgent = authFileUserAgent(auth)
		}
		if userAgent != "" {
			headers["User-Agent"] = userAgent
		}
	case "claude":
		userAgent := strings.TrimSpace(readConfigValue(h, func(cfg *config.Config) string {
			return cfg.ClaudeHeaderDefaults.UserAgent
		}))
		if userAgent != "" {
			headers["User-Agent"] = userAgent
		}
	}
}

func tokenValueForAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := tokenValueFromMetadata(auth.Metadata); v != "" {
		return v
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
	}
	return ""
}

func (h *Handler) resolveTokenForAuth(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}

	_ = ctx
	return tokenValueForAuth(auth), nil
}

func tokenValueFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if v, ok := metadata["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if tokenRaw, ok := metadata["token"]; ok && tokenRaw != nil {
		switch typed := tokenRaw.(type) {
		case string:
			if v := strings.TrimSpace(typed); v != "" {
				return v
			}
		case map[string]any:
			if v, ok := typed["access_token"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
			if v, ok := typed["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]string:
			if v := strings.TrimSpace(typed["access_token"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(typed["accessToken"]); v != "" {
				return v
			}
		}
	}
	if v, ok := metadata["token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["id_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["cookie"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func (h *Handler) authByIndex(authIndex string) *coreauth.Auth {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || h == nil {
		return nil
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return nil
	}
	if auth, ok := manager.GetByIndex(authIndex); ok {
		return auth
	}
	return nil
}

func (h *Handler) apiCallTransport(auth *coreauth.Auth) http.RoundTripper {
	var proxyCandidates []string
	if auth != nil {
		if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	}
	configProxy, globalProxy := h.apiCallProxyURLs(auth)
	if configProxy != "" {
		proxyCandidates = append(proxyCandidates, configProxy)
	}
	if globalProxy != "" {
		proxyCandidates = append(proxyCandidates, globalProxy)
	}

	for _, proxyStr := range proxyCandidates {
		if transport := cachedAPICallTransport(proxyStr); transport != nil {
			return transport
		}
	}

	if cached, ok := apiCallTransportCache.Load("direct"); ok {
		if transport, okTransport := cached.(http.RoundTripper); okTransport && transport != nil {
			return transport
		}
	}

	direct := buildDirectAPICallTransport()
	actual, _ := apiCallTransportCache.LoadOrStore("direct", direct)
	if cached, okTransport := actual.(http.RoundTripper); okTransport && cached != nil {
		return cached
	}
	return direct
}

func (h *Handler) apiCallProxyURLs(auth *coreauth.Auth) (configProxy, globalProxy string) {
	if h == nil {
		return "", ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cfg == nil {
		return "", ""
	}
	if auth != nil {
		configProxy = strings.TrimSpace(proxyURLFromAPIKeyConfig(h.cfg, auth))
	}
	globalProxy = strings.TrimSpace(h.cfg.ProxyURL)
	return configProxy, globalProxy
}

func buildDirectAPICallTransport() http.RoundTripper {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport == nil {
		return proxyutil.ApplyHTTPTransportPoolSettings(&http.Transport{Proxy: nil})
	}
	clone := transport.Clone()
	clone.Proxy = nil
	return proxyutil.ApplyHTTPTransportPoolSettings(clone)
}

func cachedAPICallTransport(proxyStr string) http.RoundTripper {
	proxyStr = strings.TrimSpace(proxyStr)
	if proxyStr == "" {
		return nil
	}
	cacheKey := "proxy:" + proxyStr
	if cached, ok := apiCallTransportCache.Load(cacheKey); ok {
		if transport, okTransport := cached.(http.RoundTripper); okTransport && transport != nil {
			return transport
		}
	}
	transport := buildProxyTransport(proxyStr)
	if transport == nil {
		return nil
	}
	actual, _ := apiCallTransportCache.LoadOrStore(cacheKey, transport)
	if cached, okTransport := actual.(http.RoundTripper); okTransport && cached != nil {
		return cached
	}
	return transport
}

type apiKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T apiKeyConfigEntry](entries []T, auth *coreauth.Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func proxyURLFromAPIKeyConfig(cfg *config.Config, auth *coreauth.Auth) string {
	if cfg == nil || auth == nil {
		return ""
	}
	authKind, authAccount := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(authKind), "api_key") {
		return ""
	}

	attrs := auth.Attributes
	compatName := ""
	providerKey := ""
	if len(attrs) > 0 {
		compatName = strings.TrimSpace(attrs["compat_name"])
		providerKey = strings.TrimSpace(attrs["provider_key"])
	}
	if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return resolveOpenAICompatAPIKeyProxyURL(cfg, auth, strings.TrimSpace(authAccount), providerKey, compatName)
	}

	provider := strings.TrimSpace(auth.Provider)
	switch {
	case strings.EqualFold(provider, "claude"):
		if entry := resolveAPIKeyConfig(cfg.ClaudeKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case strings.EqualFold(provider, "codex"):
		if entry := resolveAPIKeyConfig(cfg.CodexKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	}
	return ""
}

func resolveOpenAICompatAPIKeyProxyURL(cfg *config.Config, auth *coreauth.Auth, apiKey, providerKey, compatName string) string {
	if cfg == nil || auth == nil {
		return ""
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}

	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				for j := range compat.APIKeyEntries {
					entry := &compat.APIKeyEntries[j]
					if strings.EqualFold(strings.TrimSpace(entry.APIKey), apiKey) {
						return strings.TrimSpace(entry.ProxyURL)
					}
				}
				return ""
			}
		}
	}
	return ""
}

func buildProxyTransport(proxyStr string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr)
	if errBuild != nil {
		log.WithError(errBuild).Debug("build proxy transport failed")
		return nil
	}
	return transport
}
