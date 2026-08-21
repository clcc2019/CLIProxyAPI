package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	authorization, err := e.codexAuthorization(req.Context(), auth, apiKey)
	if err != nil {
		return err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	if codexIsAgentIdentityAuth(auth) && authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewCodexHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if resp != nil {
		codexPublishRateLimitsFromHeaders(ctx, auth, resp.Header)
	}
	if err != nil || resp == nil || !codexIsAgentIdentityAuth(auth) || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	body, errPeek := peekCodexAgentIdentityErrorBody(resp)
	if errPeek != nil || !isCodexAgentIdentityTaskInvalid(resp.StatusCode, body) {
		return resp, err
	}
	if req.Body != nil && req.GetBody == nil {
		return resp, err
	}
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("codex executor: close unauthorized response body before agent identity retry: %v", errClose)
	}
	if _, _, errRecover := e.ensureCodexAgentIdentityTask(ctx, auth, codexAgentIdentityTaskID(auth)); errRecover != nil {
		return nil, errRecover
	}
	retryReq := req.Clone(ctx)
	if req.Body != nil {
		retryBody, errBody := req.GetBody()
		if errBody != nil {
			return nil, errBody
		}
		retryReq.Body = retryBody
	}
	if errPrepare := e.PrepareRequest(retryReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	resp, err = httpClient.Do(retryReq)
	if resp != nil {
		codexPublishRateLimitsFromHeaders(ctx, auth, resp.Header)
	}
	return resp, err
}

type codexPreparedHTTPCall struct {
	url        string
	prepared   codexPreparedRequest
	requestLog helps.UpstreamRequestLog
}

func (e *CodexExecutor) prepareCodexHTTPCall(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	from sdktranslator.Format,
	executionSessionID string,
	url string,
	req cliproxyexecutor.Request,
	body []byte,
	token string,
	stream bool,
) (codexPreparedHTTPCall, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	return e.prepareCodexHTTPCallWithBaseModel(ctx, auth, from, executionSessionID, url, req, body, token, stream, baseModel)
}

func (e *CodexExecutor) prepareCodexHTTPCallWithBaseModel(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	from sdktranslator.Format,
	executionSessionID string,
	url string,
	req cliproxyexecutor.Request,
	body []byte,
	token string,
	stream bool,
	baseModel string,
) (codexPreparedHTTPCall, error) {
	return e.prepareCodexHTTPCallWithBaseModelAndFinalOptions(ctx, auth, from, executionSessionID, url, req, body, token, stream, baseModel, codexDefaultFinalUpstreamBodyOptions(auth, url))
}

func codexDefaultFinalUpstreamBodyOptions(auth *cliproxyauth.Auth, url string) codexFinalUpstreamBodyOptions {
	requestKind := codexFinalUpstreamRequestKindForURL(url)
	streamMode := codexStreamFieldTrue
	if requestKind == codexFinalUpstreamCompact {
		streamMode = codexStreamFieldDelete
	}
	return codexFinalUpstreamBodyOptions{
		requestKind:     requestKind,
		streamMode:      streamMode,
		store:           codexShouldStoreResponses(auth, url),
		omitServiceTier: auth == nil || !auth.ServiceTierPassthrough(),
	}
}

func (e *CodexExecutor) prepareCodexHTTPCallWithBaseModelAndFinalOptions(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	from sdktranslator.Format,
	executionSessionID string,
	url string,
	req cliproxyexecutor.Request,
	body []byte,
	token string,
	stream bool,
	baseModel string,
	finalOpts codexFinalUpstreamBodyOptions,
) (codexPreparedHTTPCall, error) {
	// Cache the inbound gin headers once so every helper invoked via this ctx
	// (prompt-cache resolution, client metadata, installation-id fallback)
	// shares a single context lookup instead of re-deriving the gin request on
	// every call.
	ctx = contextWithCachedCodexGinHeaders(ctx)
	requestKind := finalOpts.requestKind
	ginHeaders := codexGinHeadersFromContext(ctx)
	finalOpts.preserveNativeFields = codexNativeClientRequestFromContext(ctx) || codexNativeClientRequest(from, ginHeaders, body)
	finalOpts.preserveCompactionTrigger = codexRemoteCompactionV2Enabled(auth, e.cfg, ginHeaders)
	body = normalizeCodexFinalUpstreamBodyBorrowed(body, baseModel, auth, finalOpts)
	responsesAPIClientMetadata := codexResponsesAPIClientMetadataFromBody(body)
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "codex executor", body)
	body = helps.SanitizeCodexInputItemIDs(body)
	prepared, err := e.prepareCodexRequestWithKindBodyOptions(ctx, from, executionSessionID, url, requestKind, req, body, codexPrepareRequestBodyOptions{
		attachBody:          requestKind == codexFinalUpstreamCompact,
		deferPromptCacheKey: requestKind != codexFinalUpstreamCompact,
	})
	if err != nil {
		return codexPreparedHTTPCall{}, err
	}
	authorization, err := e.codexAuthorization(ctx, auth, token)
	if err != nil {
		return codexPreparedHTTPCall{}, err
	}
	profileHeaders := applyCodexHeadersForRequestKindWithGinHeaders(prepared.httpReq, auth, authorization, stream, e.cfg, requestKind, ginHeaders)
	// Header preparation intentionally consumes inbound session aliases. Apply
	// a forced session again afterwards so caller-owned aliases cannot replace
	// the forced upstream continuity chain.
	codexApplyForcedUpstreamSessionHeaders(ctx, prepared.httpReq.Header)
	codexApplyModelHeaderOverrides(prepared.httpReq.Header, baseModel)
	codexApplyResponsesLiteHeader(prepared.httpReq.Header, baseModel, auth)
	codexApplyRoutingHintHeader(prepared.httpReq.Header, auth, prepared.body)
	codexMergeResponsesAPIClientMetadataIntoTurnMetadataHeader(prepared.httpReq.Header, responsesAPIClientMetadata)
	if requestKind != codexFinalUpstreamCompact {
		prepared.body = codexApplyHTTPClientMetadataWithSourceAndPromptCacheKey(prepared.body, prepared.httpReq.Header, profileHeaders, auth, e.cfg, prepared.promptCacheID)
		codexResetRequestBody(prepared.httpReq, prepared.body)
		e.applyCodexHTTPTurnState(auth, executionSessionID, prepared.httpReq.Header)
	}
	if requestKind == codexFinalUpstreamCompact {
		if installationID := codexResolvedInstallationID(prepared.httpReq.Header, ginHeaders, auth, e.cfg); installationID != "" {
			codexSetSingleHeaderValue(prepared.httpReq.Header, codexHeaderInstallationID, installationID)
		}
	}
	if requestKind != codexFinalUpstreamCompact {
		if err := maybeEnableCodexRequestCompressionWithConfigForURL(prepared.httpReq, auth, e.cfg, prepared.body, url); err != nil {
			return codexPreparedHTTPCall{}, fmt.Errorf("codex executor: request compression failed: %w", err)
		}
	}
	logCodexFinalUpstreamRequestDiagnostics(ctx, requestKind, from, req.Model, baseModel, prepared.body, prepared.httpReq.Header.Get("Content-Encoding"))
	return codexPreparedHTTPCall{
		url:      url,
		prepared: prepared,
		requestLog: codexUpstreamRequestLog(
			url,
			http.MethodPost,
			prepared.httpReq.Header,
			prepared.body,
			e.Identifier(),
			auth,
		),
	}, nil
}

func logCodexFinalUpstreamRequestDiagnostics(ctx context.Context, requestKind codexFinalUpstreamRequestKind, from sdktranslator.Format, requestedModel, baseModel string, body []byte, contentEncoding string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	root := gjson.ParseBytes(body)
	fields := log.Fields{
		"endpoint":              codexFinalUpstreamRequestKindLogName(requestKind),
		"source_format":         from.String(),
		"requested_model":       strings.TrimSpace(requestedModel),
		"base_model":            strings.TrimSpace(baseModel),
		"body_bytes":            len(body),
		"content_encoding":      strings.TrimSpace(contentEncoding),
		"json_object":           root.IsObject(),
		"top_level_fields":      codexTopLevelFieldCount(root),
		"input_items":           codexJSONArrayLen(root.Get("input")),
		"tools":                 codexJSONArrayLen(root.Get("tools")),
		"has_instructions":      codexJSONNonNullExists(root.Get("instructions")),
		"has_reasoning":         codexJSONNonNullExists(root.Get("reasoning")),
		"has_text_format":       codexJSONNonNullExists(root.Get("text.format")),
		"has_previous_response": codexJSONStringPresent(root.Get("previous_response_id")),
		"has_prompt_cache_key":  codexJSONStringPresent(root.Get("prompt_cache_key")),
	}
	if enc, err := tokenizerForCodexModel(baseModel); err != nil {
		fields["estimated_input_tokens_error"] = err.Error()
	} else if count, err := countCodexInputTokens(enc, body); err != nil {
		fields["estimated_input_tokens_error"] = err.Error()
	} else {
		fields["estimated_input_tokens"] = count
	}
	helps.LogWithRequestID(ctx).WithFields(fields).Debug("codex executor: prepared final upstream request")
}

func codexFinalUpstreamRequestKindLogName(kind codexFinalUpstreamRequestKind) string {
	if kind == codexFinalUpstreamCompact {
		return "responses/compact"
	}
	return "responses"
}

func codexTopLevelFieldCount(root gjson.Result) int {
	if !root.IsObject() {
		return 0
	}
	count := 0
	root.ForEach(func(_, _ gjson.Result) bool {
		count++
		return true
	})
	return count
}

func codexJSONArrayLen(result gjson.Result) int {
	if !result.IsArray() {
		return 0
	}
	count := 0
	result.ForEach(func(_, _ gjson.Result) bool {
		count++
		return true
	})
	return count
}

func codexJSONNonNullExists(result gjson.Result) bool {
	return result.Exists() && result.Type != gjson.Null
}

func codexJSONStringPresent(result gjson.Result) bool {
	return result.Type == gjson.String && strings.TrimSpace(result.String()) != ""
}

func codexUpstreamRequestLog(
	url string,
	method string,
	headers http.Header,
	body []byte,
	provider string,
	auth *cliproxyauth.Auth,
) helps.UpstreamRequestLog {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	return helps.UpstreamRequestLog{
		URL:       url,
		Method:    method,
		Headers:   headers,
		Body:      body,
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
}
