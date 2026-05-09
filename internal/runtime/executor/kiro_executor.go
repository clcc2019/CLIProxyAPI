package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	kiroclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/claude"
	kirocommon "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/common"
	kiroopenai "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/openai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	kiroContentType   = "application/json"
	kiroAcceptStream  = "*/*"
	kiroDefaultRegion = "us-east-1"
	// kiroMaxFrameSize bounds a single event-stream frame (guards against
	// malformed upstream payloads that would otherwise let us allocate
	// arbitrary memory per frame). Real Kiro event-stream frames are tiny
	// — assistant deltas are a few hundred bytes, tool_use payloads rarely
	// exceed tens of KiB. The validator inside readEventStreamMessage
	// uses this to reject oversize frames.
	kiroMaxFrameSize = 10 << 20
	// kiroStreamReaderBuffer is the bufio.Reader buffer size used when
	// decoding an event-stream body. Undersized here still works because
	// io.ReadFull transparently issues multiple reads against the
	// underlying body when a frame exceeds the buffer — the buffer just
	// controls per-read allocation, not the maximum readable frame size.
	// Keeping it at 64 KiB saves ~10 MiB of resident memory per in-flight
	// streaming request vs. matching kiroMaxFrameSize.
	kiroStreamReaderBuffer = 64 << 10
	kiroIDEAgentMode       = "vibe"
	kiroCLIUserAgent       = "aws-sdk-rust/1.3.9 os/linux lang/rust/1.92.0"
	kiroCLIAmzAgent        = "aws-sdk-rust/1.3.9 ua/2.1 api/ssooidc/1.92.0 os/linux lang/rust/1.92.0 m/E app/AmazonQ-For-CLI"
	kiroRefreshSkew        = 2 * time.Minute
)

var (
	kiroSocialRefreshEndpoint = func(region string) string {
		return fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)
	}
	kiroSSOTokenEndpoint = func(region string) string {
		return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
	}
)

type KiroExecutor struct {
	cfg *config.Config
}

func NewKiroExecutor(cfg *config.Config) *KiroExecutor {
	return &KiroExecutor{cfg: cfg}
}

func (e *KiroExecutor) Identifier() string { return "kiro" }

type kiroEndpointConfig struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

type kiroPreparedRequest struct {
	translated   []byte
	from         sdktranslator.Format
	modelID      string
	profileArn   string
	isAgentic    bool
	isChatOnly   bool
	endpoints    []kiroEndpointConfig
	headers      http.Header
	sourceBody   []byte
	firstPayload []byte
}

func buildKiroEndpointConfigs(region string) []kiroEndpointConfig {
	if strings.TrimSpace(region) == "" {
		region = kiroDefaultRegion
	}
	return []kiroEndpointConfig{
		{
			URL:    fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region),
			Origin: "AI_EDITOR",
			Name:   "AmazonQ",
		},
		{
			URL:       fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/generateAssistantResponse", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			Name:      "CodeWhisperer",
		},
	}
}

func (e *KiroExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	accessToken, profileArn := kiroCredentials(auth)
	if strings.TrimSpace(accessToken) == "" {
		return resp, statusErr{code: http.StatusUnauthorized, msg: "kiro: access token not found in auth"}
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)

	auth, accessToken, profileArn, err = e.refreshIfKiroTokenExpiring(ctx, auth, accessToken, profileArn)
	if err != nil {
		return resp, err
	}

	prepared, err := e.buildRequestPayload(req, opts, auth, profileArn, false)
	if err != nil {
		return resp, err
	}
	httpResp, _, err := e.doKiroRequestWithFallbackRetry(ctx, auth, prepared, accessToken)
	if err != nil {
		if isUnauthorizedStatusErr(err) {
			refreshed, refreshErr := cliproxyauth.CoordinatedRefresh(ctx, auth, e.Refresh)
			if refreshErr != nil {
				if isKiroRefreshPermanent(refreshErr) {
					log.Warnf("kiro executor: refresh failed permanently for auth=%s after 401, returning permanent error: %v", auth.ID, refreshErr)
				}
				return resp, refreshErr
			} else if refreshed != nil {
				auth = refreshed
				cliproxyauth.PublishRefreshUpdate(ctx, auth)
				accessToken, profileArn = kiroCredentials(auth)
				prepared, err = e.buildRequestPayload(req, opts, auth, profileArn, false)
				if err == nil {
					httpResp, _, err = e.doKiroRequestWithFallback(ctx, auth, prepared, accessToken)
				}
			}
		}
		if err != nil {
			return resp, err
		}
	}
	defer closeKiroResponseBody(httpResp)

	content, toolUses, usageInfo, stopReason, parseErr := e.parseEventStream(httpResp.Body)
	if parseErr != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, parseErr)
		return resp, parseErr
	}
	reporter.Publish(ctx, usageInfo)
	raw := kiroclaude.BuildClaudeResponse(content, toolUses, helps.PayloadRequestedModel(opts, req.Model), usageInfo, stopReason)
	helps.AppendAPIResponseChunk(ctx, e.cfg, raw)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FromString("kiro"), prepared.from, req.Model, opts.OriginalRequest, prepared.translated, raw, &param)
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

func (e *KiroExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	accessToken, profileArn := kiroCredentials(auth)
	if strings.TrimSpace(accessToken) == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "kiro: access token not found in auth"}
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)

	auth, accessToken, profileArn, err = e.refreshIfKiroTokenExpiring(ctx, auth, accessToken, profileArn)
	if err != nil {
		return nil, err
	}

	prepared, err := e.buildRequestPayload(req, opts, auth, profileArn, true)
	if err != nil {
		return nil, err
	}
	httpResp, _, err := e.doKiroRequestWithFallbackRetry(ctx, auth, prepared, accessToken)
	if err != nil {
		if isUnauthorizedStatusErr(err) {
			refreshed, refreshErr := cliproxyauth.CoordinatedRefresh(ctx, auth, e.Refresh)
			if refreshErr != nil {
				if isKiroRefreshPermanent(refreshErr) {
					log.Warnf("kiro executor: refresh failed permanently for auth=%s after 401 (stream), returning permanent error: %v", auth.ID, refreshErr)
				}
				return nil, refreshErr
			} else if refreshed != nil {
				auth = refreshed
				cliproxyauth.PublishRefreshUpdate(ctx, auth)
				accessToken, profileArn = kiroCredentials(auth)
				prepared, err = e.buildRequestPayload(req, opts, auth, profileArn, true)
				if err == nil {
					httpResp, _, err = e.doKiroRequestWithFallback(ctx, auth, prepared, accessToken)
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}

	out := make(chan cliproxyexecutor.StreamChunk, helps.StreamChunkBufferSize)
	go func() {
		defer close(out)
		defer closeKiroResponseBody(httpResp)
		e.streamToChannel(ctx, httpResp.Body, out, prepared.from, helps.PayloadRequestedModel(opts, req.Model), opts.OriginalRequest, prepared.translated, reporter)
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *KiroExecutor) refreshIfKiroTokenExpiring(ctx context.Context, auth *cliproxyauth.Auth, accessToken, profileArn string) (*cliproxyauth.Auth, string, string, error) {
	if auth == nil {
		return auth, accessToken, profileArn, nil
	}
	shouldRefresh, required := kiroRefreshDecisionBeforeRequest(auth, time.Now().UTC())
	if !shouldRefresh {
		return auth, accessToken, profileArn, nil
	}
	refreshed, err := cliproxyauth.CoordinatedRefresh(ctx, auth, e.Refresh)
	if err != nil {
		// Permanent refresh failure must always be surfaced — the current
		// access token might still be valid for a few more seconds, but
		// continuing would only delay the inevitable failure and burn more
		// upstream quota.
		if isKiroRefreshPermanent(err) {
			log.Warnf("kiro executor: permanent refresh failure for auth=%s (soft=%t): %v", auth.ID, !required, err)
			return auth, accessToken, profileArn, err
		}
		return auth, accessToken, profileArn, err
	}
	if refreshed == nil {
		return auth, accessToken, profileArn, nil
	}
	cliproxyauth.PublishRefreshUpdate(ctx, refreshed)
	accessToken, profileArn = kiroCredentials(refreshed)
	return refreshed, accessToken, profileArn, nil
}

func shouldRefreshKiroBeforeRequest(auth *cliproxyauth.Auth, now time.Time) bool {
	shouldRefresh, _ := kiroRefreshDecisionBeforeRequest(auth, now)
	return shouldRefresh
}

func kiroRefreshDecisionBeforeRequest(auth *cliproxyauth.Auth, now time.Time) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if expiresAt, ok := auth.ExpirationTime(); ok && !expiresAt.IsZero() && expiresAt.Sub(now) <= kiroRefreshSkew {
		return true, true
	}
	interval := kiroRefreshIntervalFromAuth(auth)
	if interval <= 0 {
		interval = time.Duration(kiroauth.DefaultRefreshIntervalMaxSeconds) * time.Second
	}
	lastRefresh, ok := kiroLastRefreshTime(auth)
	if !ok || lastRefresh.IsZero() {
		return true, true
	}
	if !lastRefresh.Add(interval).After(now) {
		return true, true
	}
	if !auth.NextRefreshAfter.IsZero() {
		return !now.Before(auth.NextRefreshAfter), true
	}
	return false, false
}

func (e *KiroExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("kiro executor: auth is nil")
	}
	if auth.Metadata == nil {
		return nil, fmt.Errorf("kiro executor: auth metadata is nil")
	}
	refreshToken := metadataString(auth.Metadata, "refresh_token", "refreshToken")
	clientID := metadataString(auth.Metadata, "client_id", "clientId")
	clientSecret := metadataString(auth.Metadata, "client_secret", "clientSecret")
	authMethod := strings.ToLower(metadataString(auth.Metadata, "auth_method", "authMethod"))
	tokenProvider := strings.ToLower(metadataString(auth.Metadata, "provider"))
	if refreshToken == "" {
		return nil, fmt.Errorf("kiro executor: refresh token not found")
	}
	region := metadataString(auth.Metadata, "region")
	if region == "" {
		region = kiroDefaultRegion
	}
	var tokenData *kiroRefreshTokenData
	var err error
	if shouldRefreshKiroWithSSO(authMethod, tokenProvider, clientID, clientSecret) {
		tokenData, err = refreshKiroSSOToken(ctx, e.cfg, auth, clientID, clientSecret, refreshToken, region)
	} else {
		tokenData, err = refreshKiroSocialToken(ctx, e.cfg, auth, refreshToken, region)
	}
	if err != nil {
		return nil, err
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	now := time.Now().UTC()
	refreshIntervalSeconds := kiroauth.RandomRefreshIntervalSeconds()
	updated.Metadata["access_token"] = tokenData.AccessToken
	updated.Metadata["refresh_token"] = tokenData.RefreshToken
	updated.Metadata["expires_at"] = tokenData.ExpiresAt
	updated.Metadata["last_refresh"] = now.Format(time.RFC3339)
	updated.Metadata["refresh_interval_seconds"] = refreshIntervalSeconds
	if tokenData.ProfileArn != "" {
		updated.Metadata["profile_arn"] = tokenData.ProfileArn
		if updated.Attributes == nil {
			updated.Attributes = map[string]string{}
		}
		updated.Attributes["profile_arn"] = tokenData.ProfileArn
	}
	updated.UpdatedAt = now
	updated.LastRefreshedAt = updated.UpdatedAt
	updated.NextRefreshAfter = now.Add(time.Duration(refreshIntervalSeconds) * time.Second)
	clearKiroCredentialErrorState(updated, now)
	return updated, nil
}

func clearKiroCredentialErrorState(auth *cliproxyauth.Auth, now time.Time) {
	if auth == nil {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if isKiroCredentialStateError(auth.LastError, auth.StatusMessage) {
		auth.Status = cliproxyauth.StatusActive
		auth.StatusMessage = ""
		auth.Unavailable = false
		auth.LastError = nil
		auth.NextRetryAfter = time.Time{}
		auth.UpdatedAt = now
	}
	for model, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if !isKiroCredentialStateError(state.LastError, state.StatusMessage) {
			continue
		}
		state.Status = cliproxyauth.StatusActive
		state.StatusMessage = ""
		state.Unavailable = false
		state.LastError = nil
		state.NextRetryAfter = time.Time{}
		state.UpdatedAt = now
		auth.ModelStates[model] = state
	}
}

func isKiroCredentialStateError(lastErr *cliproxyauth.Error, statusMessage string) bool {
	if lastErr != nil {
		switch lastErr.HTTPStatus {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true
		}
		if isKiroInvalidBearerTokenMessage(lastErr.Message) || isKiroBadCredentialsMessage(lastErr.Message) {
			return true
		}
	}
	return isKiroInvalidBearerTokenMessage(statusMessage) || isKiroBadCredentialsMessage(statusMessage)
}

func shouldRefreshKiroWithSSO(authMethod, provider, clientID, clientSecret string) bool {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return false
	}
	authMethod = strings.ToLower(strings.TrimSpace(authMethod))
	provider = strings.ToLower(strings.TrimSpace(provider))
	if isKiroSocialAuth(authMethod, provider) {
		return false
	}
	if isKiroSSOAuth(authMethod, provider) {
		return true
	}
	// Unknown classification: default to the social path rather than SSO.
	//
	// Rationale: the SSO OIDC endpoint is strict — a social token posted to
	// oidc.<region>.amazonaws.com/token returns `invalid_client`, which our
	// permanent-error classifier then converts into a 24h park. That means a
	// single metadata quirk (stray clientId on a social auth) silently
	// bricks the credential for a day. The social endpoint is more
	// forgiving: an SSO-shaped token posted to kiro.dev/refreshToken
	// returns a generic 400 that our retry/backoff can recover from once
	// the metadata is corrected. Default-to-social is therefore the safer
	// misclassification direction.
	//
	// Operators should set `auth_method` / `provider` explicitly; the Warn
	// below surfaces the ambiguity so misconfigurations are visible.
	log.Warnf("kiro: refresh path classification ambiguous for auth (auth_method=%q provider=%q) — defaulting to social endpoint; set auth_method explicitly to silence this warning", authMethod, provider)
	return false
}

func isKiroSocialAuth(authMethod, provider string) bool {
	authMethod = strings.ToLower(strings.TrimSpace(authMethod))
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch authMethod {
	case "kiro-cli-social", "kiro-social", "social", "google", "github", "gitlab":
		return true
	}
	switch provider {
	case "google", "github", "gitlab", "kiro-cli", "kiro-social", "social":
		return true
	default:
		return false
	}
}

func isKiroSSOAuth(authMethod, provider string) bool {
	authMethod = strings.ToLower(strings.TrimSpace(authMethod))
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch authMethod {
	case "builder-id", "idc", "aws_sso_oidc", "aws-sso-oidc":
		return true
	}
	switch provider {
	case "aws", "builder-id", "idc":
		return true
	default:
		return false
	}
}

func (e *KiroExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, req.Model, req.Payload, false)
	enc, err := helps.TokenizerForModel(req.Model)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("kiro executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		count = int64(len(req.Payload) / 4)
		if count == 0 && len(req.Payload) > 0 {
			count = 1
		}
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

func (e *KiroExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	accessToken, _ := kiroCredentials(auth)
	if strings.TrimSpace(accessToken) != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

func (e *KiroExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kiro executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *KiroExecutor) buildRequestPayload(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, auth *cliproxyauth.Auth, profileArn string, stream bool) (*kiroPreparedRequest, error) {
	from := opts.SourceFormat
	to := sdktranslator.FromString("kiro")
	translated := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), stream)
	kiroModelID := e.mapModelToKiro(req.Model)
	isAgentic, isChatOnly := determineAgenticMode(req.Model)
	effectiveProfileArn := getEffectiveProfileArn(auth, profileArn)
	endpoints := getKiroEndpointConfigs(auth)
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("kiro: no endpoint configured")
	}
	payload, _ := buildKiroPayloadForFormat(translated, kiroModelID, effectiveProfileArn, endpoints[0].Origin, isAgentic, isChatOnly, from, opts.Headers)
	return &kiroPreparedRequest{
		translated:   translated,
		from:         from,
		modelID:      kiroModelID,
		profileArn:   effectiveProfileArn,
		isAgentic:    isAgentic,
		isChatOnly:   isChatOnly,
		endpoints:    endpoints,
		headers:      opts.Headers,
		sourceBody:   translated,
		firstPayload: payload,
	}, nil
}

func (e *KiroExecutor) doKiroRequestWithFallback(ctx context.Context, auth *cliproxyauth.Auth, prepared *kiroPreparedRequest, accessToken string) (*http.Response, []byte, error) {
	if prepared == nil {
		return nil, nil, fmt.Errorf("kiro: prepared request is nil")
	}
	var lastErr error
	var lastPayload []byte
	firstOrigin := ""
	if len(prepared.endpoints) > 0 {
		firstOrigin = prepared.endpoints[0].Origin
	}
	for idx, endpoint := range prepared.endpoints {
		payload := prepared.firstPayload
		// Skip the translator pipeline rebuild when the next endpoint shares
		// the same origin as the first — `buildKiroPayloadForFormat` only
		// varies based on the origin field, so rebuilding with the same
		// origin is guaranteed to produce an identical byte stream. Saves
		// one full JSON marshal + conversation-state render per fallback.
		if idx != 0 && (len(payload) == 0 || endpoint.Origin != firstOrigin) {
			payload, _ = buildKiroPayloadForFormat(prepared.sourceBody, prepared.modelID, prepared.profileArn, endpoint.Origin, prepared.isAgentic, prepared.isChatOnly, prepared.from, prepared.headers)
		} else if idx == 0 && len(payload) == 0 {
			payload, _ = buildKiroPayloadForFormat(prepared.sourceBody, prepared.modelID, prepared.profileArn, endpoint.Origin, prepared.isAgentic, prepared.isChatOnly, prepared.from, prepared.headers)
		}
		lastPayload = payload
		httpResp, err := e.doKiroRequest(ctx, auth, endpoint, payload, accessToken)
		if err == nil {
			return httpResp, payload, nil
		}
		lastErr = err
		if idx == len(prepared.endpoints)-1 || !shouldTryNextKiroEndpoint(err) {
			// Kiro's AGENTIC_REQUEST quota is shared across models, but 429
			// can also mean short-lived throttling. wrapKiroAuthScoped only
			// upgrades clear quota/usage exhaustion responses to auth-wide
			// failures; transient throttling remains model/request scoped.
			return nil, payload, wrapKiroAuthScoped(err)
		}
		log.Debugf("kiro: endpoint %s failed, trying next endpoint %s: %v", endpoint.Name, prepared.endpoints[idx+1].Name, err)
	}
	if lastErr != nil {
		return nil, lastPayload, wrapKiroAuthScoped(lastErr)
	}
	return nil, lastPayload, fmt.Errorf("kiro: all endpoints exhausted")
}

func (e *KiroExecutor) doKiroRequest(ctx context.Context, auth *cliproxyauth.Auth, endpointConfig kiroEndpointConfig, payload []byte, accessToken string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointConfig.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", kiroContentType)
	httpReq.Header.Set("Accept", kiroAcceptStream)
	if endpointConfig.AmzTarget != "" {
		httpReq.Header.Set("X-Amz-Target", endpointConfig.AmzTarget)
	}
	httpReq.Header.Set("x-amzn-kiro-agent-mode", kiroIDEAgentMode)
	httpReq.Header.Set("x-amzn-codewhisperer-optout", "true")
	httpReq.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	httpReq.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	httpReq.Header.Set("User-Agent", kiroCLIUserAgent)
	httpReq.Header.Set("x-amz-user-agent", kiroCLIAmzAgent)
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

	authID, authLabel, authType, authValue := authLogInfo(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       endpointConfig.URL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header)
	if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
		return httpResp, nil
	}
	b, _ := helps.ReadErrorResponseBody(httpResp.Body)
	helps.AppendAPIResponseChunk(ctx, e.cfg, b)
	closeKiroResponseBody(httpResp)
	return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
}

type kiroEventStreamMessage struct {
	EventType string
	Payload   []byte
}

func (e *KiroExecutor) parseEventStream(body io.Reader) (string, []kiroclaude.KiroToolUse, usage.Detail, string, error) {
	reader := bufio.NewReaderSize(body, kiroStreamReaderBuffer)
	var content strings.Builder
	var thinking strings.Builder
	var toolUses []kiroclaude.KiroToolUse
	var usageInfo usage.Detail
	var stopReason string
	processedToolIDs := make(map[string]bool)
	var currentToolUse *kiroclaude.ToolUseState

	for {
		msg, err := e.readEventStreamMessage(reader)
		if err != nil {
			return content.String(), toolUses, usageInfo, stopReason, err
		}
		if msg == nil {
			break
		}
		helps.AppendAPIResponseChunk(context.Background(), e.cfg, msg.Payload)
		event := parseKiroEventPayload(msg.Payload, msg.EventType)
		if event.err != nil {
			return "", nil, usageInfo, stopReason, event.err
		}
		if event.content != "" {
			content.WriteString(event.content)
		}
		if event.thinking != "" {
			thinking.WriteString(event.thinking)
		}
		if event.stopReason != "" {
			stopReason = event.stopReason
		}
		mergeKiroUsage(&usageInfo, event.usage)
		for _, toolUse := range event.toolUses {
			appendKiroToolUse(&toolUses, processedToolIDs, toolUse, true)
		}
		if event.toolUseEvent != nil {
			completed, newState := kiroclaude.ProcessToolUseEvent(event.toolUseEvent, currentToolUse, processedToolIDs)
			currentToolUse = newState
			for _, toolUse := range completed {
				appendKiroToolUse(&toolUses, processedToolIDs, toolUse, false)
			}
		}
	}
	if thinking.Len() > 0 {
		contentText := content.String()
		content.Reset()
		content.WriteString(kirocommon.ThinkingStartTag)
		content.WriteString(thinking.String())
		content.WriteString(kirocommon.ThinkingEndTag)
		content.WriteString(contentText)
	}
	if flushed, ok := flushKiroToolUseState(currentToolUse, processedToolIDs); ok {
		appendKiroToolUse(&toolUses, processedToolIDs, flushed, false)
	}
	contentText := content.String()
	cleanedContent, embeddedToolUses := kiroclaude.ParseEmbeddedToolCalls(contentText, processedToolIDs)
	for _, toolUse := range embeddedToolUses {
		appendKiroToolUse(&toolUses, processedToolIDs, toolUse, true)
	}
	content.Reset()
	content.WriteString(cleanedContent)
	return content.String(), toolUses, usageInfo, stopReason, nil
}

func (e *KiroExecutor) streamToChannel(ctx context.Context, body io.Reader, out chan<- cliproxyexecutor.StreamChunk, targetFormat sdktranslator.Format, model string, originalReq, kiroReq []byte, reporter *helps.UsageReporter) {
	reader := bufio.NewReaderSize(body, kiroStreamReaderBuffer)
	var translatorParam any
	var totalUsage usage.Detail
	var accumulatedContent strings.Builder
	messageStartSent := false
	textBlockOpen := false
	thinkingBlockOpen := false
	hasToolUses := false
	contentBlockIndex := -1
	stopReason := ""
	streamFailed := false

	emit := func(raw []byte) {
		chunks := sdktranslator.TranslateStream(ctx, sdktranslator.FromString("kiro"), targetFormat, model, originalReq, kiroReq, raw, &translatorParam)
		for _, chunk := range chunks {
			out <- cliproxyexecutor.StreamChunk{Payload: chunk}
		}
	}
	ensureMessageStart := func() {
		if messageStartSent {
			return
		}
		emit(kiroclaude.BuildClaudeMessageStartEvent(model, totalUsage))
		messageStartSent = true
	}
	closeText := func() {
		if !textBlockOpen {
			return
		}
		emit(kiroclaude.BuildClaudeContentBlockStopEvent(contentBlockIndex))
		textBlockOpen = false
	}
	closeThinking := func() {
		if !thinkingBlockOpen {
			return
		}
		emit(kiroclaude.BuildClaudeThinkingBlockStopEvent(contentBlockIndex))
		thinkingBlockOpen = false
	}

	defer func() {
		if streamFailed {
			return
		}
		closeThinking()
		closeText()
		if !messageStartSent {
			ensureMessageStart()
		}
		if stopReason == "" {
			if hasToolUses {
				stopReason = "tool_use"
			} else {
				stopReason = "end_turn"
			}
		}
		if totalUsage.OutputTokens == 0 && accumulatedContent.Len() > 0 {
			totalUsage.OutputTokens = int64(accumulatedContent.Len() / 4)
			if totalUsage.OutputTokens == 0 {
				totalUsage.OutputTokens = 1
			}
			totalUsage.TotalTokens = totalUsage.InputTokens + totalUsage.OutputTokens
		}
		emit(kiroclaude.BuildClaudeMessageDeltaEvent(stopReason, totalUsage))
		emit(kiroclaude.BuildClaudeMessageStopOnlyEvent())
		if reporter != nil {
			reporter.Publish(ctx, totalUsage)
			reporter.EnsurePublished(ctx)
		}
	}()

	processedToolIDs := make(map[string]bool)
	var currentToolUse *kiroclaude.ToolUseState
	emitToolUse := func(toolUse kiroclaude.KiroToolUse, mark bool) {
		if toolUse.IsTruncated {
			log.Warnf("kiro: streamToChannel skipping truncated tool: %s (ID: %s)", toolUse.Name, toolUse.ToolUseID)
			return
		}
		if mark && toolUse.ToolUseID != "" {
			if processedToolIDs[toolUse.ToolUseID] {
				return
			}
			processedToolIDs[toolUse.ToolUseID] = true
		}
		closeThinking()
		closeText()
		contentBlockIndex++
		emit(kiroclaude.BuildClaudeContentBlockStartEvent(contentBlockIndex, "tool_use", toolUse.ToolUseID, toolUse.Name))
		inputBytes, _ := json.Marshal(toolUse.Input)
		emit(kiroclaude.BuildClaudeInputJsonDeltaEvent(string(inputBytes), contentBlockIndex))
		emit(kiroclaude.BuildClaudeContentBlockStopEvent(contentBlockIndex))
		hasToolUses = true
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msg, err := e.readEventStreamMessage(reader)
		if err != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, err)
			if reporter != nil {
				reporter.PublishFailure(ctx)
			}
			streamFailed = true
			out <- cliproxyexecutor.StreamChunk{Err: err}
			return
		}
		if msg == nil {
			if flushed, ok := flushKiroToolUseState(currentToolUse, processedToolIDs); ok {
				emitToolUse(flushed, false)
				currentToolUse = nil
			}
			return
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, msg.Payload)
		event := parseKiroEventPayload(msg.Payload, msg.EventType)
		if event.err != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, event.err)
			if reporter != nil {
				reporter.PublishFailure(ctx)
			}
			streamFailed = true
			out <- cliproxyexecutor.StreamChunk{Err: event.err}
			return
		}
		ensureMessageStart()
		if event.stopReason != "" {
			stopReason = event.stopReason
		}
		mergeKiroUsage(&totalUsage, event.usage)

		if event.content != "" {
			cleanedContent, embeddedToolUses := kiroclaude.ParseEmbeddedToolCalls(event.content, processedToolIDs)
			event.content = cleanedContent
			event.toolUses = append(event.toolUses, embeddedToolUses...)
		}
		if event.thinking != "" {
			closeText()
			if !thinkingBlockOpen {
				contentBlockIndex++
				emit(kiroclaude.BuildClaudeContentBlockStartEvent(contentBlockIndex, "thinking", "", ""))
				thinkingBlockOpen = true
			}
			emit(kiroclaude.BuildClaudeThinkingDeltaEvent(event.thinking, contentBlockIndex))
		}
		if event.content != "" {
			closeThinking()
			if !textBlockOpen {
				contentBlockIndex++
				emit(kiroclaude.BuildClaudeContentBlockStartEvent(contentBlockIndex, "text", "", ""))
				textBlockOpen = true
			}
			accumulatedContent.WriteString(event.content)
			emit(kiroclaude.BuildClaudeStreamEvent(event.content, contentBlockIndex))
		}
		for _, toolUse := range event.toolUses {
			emitToolUse(toolUse, true)
		}
		if event.toolUseEvent != nil {
			completed, newState := kiroclaude.ProcessToolUseEvent(event.toolUseEvent, currentToolUse, processedToolIDs)
			currentToolUse = newState
			for _, toolUse := range completed {
				emitToolUse(toolUse, false)
			}
		}
	}
}

func appendKiroToolUse(dst *[]kiroclaude.KiroToolUse, processed map[string]bool, toolUse kiroclaude.KiroToolUse, mark bool) {
	if mark && toolUse.ToolUseID != "" && processed != nil {
		if processed[toolUse.ToolUseID] {
			return
		}
		processed[toolUse.ToolUseID] = true
	}
	*dst = append(*dst, toolUse)
}

func flushKiroToolUseState(current *kiroclaude.ToolUseState, processed map[string]bool) (kiroclaude.KiroToolUse, bool) {
	if current == nil {
		return kiroclaude.KiroToolUse{}, false
	}
	if current.ToolUseID == "" && current.Name == "" {
		return kiroclaude.KiroToolUse{}, false
	}
	if current.ToolUseID != "" && processed != nil && processed[current.ToolUseID] {
		return kiroclaude.KiroToolUse{}, false
	}

	rawInput := current.InputBuffer.String()
	finalInput := map[string]any{}
	if strings.TrimSpace(rawInput) != "" {
		repaired := kiroclaude.RepairJSON(rawInput)
		if err := json.Unmarshal([]byte(repaired), &finalInput); err != nil {
			log.Warnf("kiro: failed to parse incomplete tool input at stream end: %v", err)
			finalInput = map[string]any{}
		}
	}
	if current.ToolUseID != "" && processed != nil {
		processed[current.ToolUseID] = true
	}
	return kiroclaude.KiroToolUse{
		ToolUseID: current.ToolUseID,
		Name:      current.Name,
		Input:     finalInput,
	}, true
}

// kiroFrameBufPool recycles the scratch buffer used to stage one
// event-stream frame in readEventStreamMessage. For a typical
// streaming response with hundreds of tiny deltas, this saves one
// heap allocation per frame — under load it measurably reduces
// GC pressure. The buffer holds `totalLen - 12` bytes (headers +
// payload + trailing CRC) of a single frame, not the whole stream,
// so power-of-two bucketing isn't needed; we re-slice as required.
var kiroFrameBufPool = sync.Pool{
	New: func() any {
		// Starting capacity tuned for typical Kiro tool_use / delta
		// frames (1-4 KiB). Pool grows naturally as larger frames
		// appear; smaller frames reuse the existing capacity.
		buf := make([]byte, 0, 4096)
		return &buf
	},
}

func (e *KiroExecutor) readEventStreamMessage(reader *bufio.Reader) (*kiroEventStreamMessage, error) {
	prelude := make([]byte, 12)
	if n, err := io.ReadFull(reader, prelude); err != nil {
		if err == io.EOF && n == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("kiro: failed to read event stream prelude: %w", err)
	}
	totalLen := int(binary.BigEndian.Uint32(prelude[0:4]))
	headersLen := int(binary.BigEndian.Uint32(prelude[4:8]))
	if totalLen < 16 || totalLen > kiroMaxFrameSize || headersLen < 0 || headersLen > totalLen-16 {
		return nil, fmt.Errorf("kiro: invalid event stream frame length total=%d headers=%d", totalLen, headersLen)
	}
	restLen := totalLen - 12
	bufPtr := kiroFrameBufPool.Get().(*[]byte)
	buf := *bufPtr
	if cap(buf) < restLen {
		// Grow to fit; discard the old tiny capacity. Upper bound is
		// kiroMaxFrameSize (10 MiB) enforced by the check above.
		buf = make([]byte, restLen)
	} else {
		buf = buf[:restLen]
	}
	if _, err := io.ReadFull(reader, buf); err != nil {
		// Return the buffer to the pool even on error; the caller
		// doesn't touch `msg` when err != nil.
		*bufPtr = buf[:0]
		kiroFrameBufPool.Put(bufPtr)
		return nil, err
	}
	headers := buf[:headersLen]
	payloadEnd := len(buf) - 4
	if payloadEnd < headersLen {
		*bufPtr = buf[:0]
		kiroFrameBufPool.Put(bufPtr)
		return nil, fmt.Errorf("kiro: invalid event stream payload length")
	}
	// Copy payload bytes out of the pooled buffer so the caller can hold
	// onto msg.Payload across loop iterations safely while we return the
	// scratch buffer to the pool. The extra copy is small (≤ totalLen)
	// and avoids subtle aliasing bugs if downstream code ever decides to
	// retain the slice (e.g., via request-log chunk buffering).
	payloadSrc := buf[headersLen:payloadEnd]
	payload := make([]byte, len(payloadSrc))
	copy(payload, payloadSrc)
	eventType := parseEventStreamHeaderString(headers, ":event-type")

	*bufPtr = buf[:0]
	kiroFrameBufPool.Put(bufPtr)

	return &kiroEventStreamMessage{
		EventType: eventType,
		Payload:   payload,
	}, nil
}

func parseEventStreamHeaderString(headers []byte, target string) string {
	offset := 0
	for offset < len(headers) {
		nameLen := int(headers[offset])
		offset++
		if nameLen <= 0 || offset+nameLen > len(headers) {
			return ""
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			return ""
		}
		valueType := headers[offset]
		offset++
		if valueType != 7 {
			next, ok := skipEventStreamHeaderValue(headers, offset, valueType)
			if !ok {
				return ""
			}
			offset = next
			continue
		}
		if offset+2 > len(headers) {
			return ""
		}
		valueLen := int(binary.BigEndian.Uint16(headers[offset : offset+2]))
		offset += 2
		if offset+valueLen > len(headers) {
			return ""
		}
		value := string(headers[offset : offset+valueLen])
		offset += valueLen
		if name == target {
			return value
		}
	}
	return ""
}

func skipEventStreamHeaderValue(headers []byte, offset int, valueType byte) (int, bool) {
	switch valueType {
	case 0, 1:
		return offset, offset <= len(headers)
	case 2:
		return offset + 1, offset+1 <= len(headers)
	case 3, 4:
		if valueType == 3 {
			return offset + 2, offset+2 <= len(headers)
		}
		return offset + 4, offset+4 <= len(headers)
	case 5, 8:
		return offset + 8, offset+8 <= len(headers)
	case 6:
		if offset+2 > len(headers) {
			return 0, false
		}
		valueLen := int(binary.BigEndian.Uint16(headers[offset : offset+2]))
		return offset + 2 + valueLen, offset+2+valueLen <= len(headers)
	case 7:
		if offset+2 > len(headers) {
			return 0, false
		}
		valueLen := int(binary.BigEndian.Uint16(headers[offset : offset+2]))
		return offset + 2 + valueLen, offset+2+valueLen <= len(headers)
	case 9:
		return offset + 16, offset+16 <= len(headers)
	default:
		return 0, false
	}
}

type parsedKiroEvent struct {
	content      string
	thinking     string
	toolUses     []kiroclaude.KiroToolUse
	toolUseEvent map[string]any
	usage        usage.Detail
	stopReason   string
	err          error
}

func parseKiroEventPayload(payload []byte, eventTypes ...string) parsedKiroEvent {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return parsedKiroEvent{}
	}
	if errType, _ := event["_type"].(string); errType != "" {
		msg, _ := event["message"].(string)
		return parsedKiroEvent{err: fmt.Errorf("kiro API error: %s - %s", errType, msg)}
	}
	if typ, _ := event["type"].(string); typ == "error" || typ == "exception" {
		msg, _ := event["message"].(string)
		return parsedKiroEvent{err: fmt.Errorf("kiro API error: %s", msg)}
	}

	parsed := parsedKiroEvent{}
	parsed.stopReason = firstKiroString(event, "stop_reason", "stopReason")
	if assistant, ok := event["assistantResponseEvent"].(map[string]any); ok {
		parsed.content = firstKiroString(assistant, "content", "text")
		if parsed.stopReason == "" {
			parsed.stopReason = firstKiroString(assistant, "stop_reason", "stopReason")
		}
		parsed.toolUses = append(parsed.toolUses, extractKiroToolUses(assistant["toolUses"])...)
	}
	if parsed.content == "" {
		parsed.content = firstKiroString(event, "content", "text")
	}
	if reasoning, ok := event["reasoningContentEvent"].(map[string]any); ok {
		parsed.thinking = firstKiroString(reasoning, "content", "text", "reasoning", "reasoningText")
	}
	if parsed.thinking == "" {
		parsed.thinking = firstKiroString(event, "reasoningContent", "reasoning", "thinking")
	}
	parsed.toolUses = append(parsed.toolUses, extractKiroToolUses(event["toolUses"])...)
	if toolUse, ok := event["toolUseEvent"].(map[string]any); ok {
		parsed.toolUseEvent = toolUse
	} else if len(eventTypes) > 0 && eventTypes[0] == "toolUseEvent" {
		parsed.toolUseEvent = event
	}
	parsed.usage = extractKiroUsage(event)
	return parsed
}

func extractKiroToolUses(raw any) []kiroclaude.KiroToolUse {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]kiroclaude.KiroToolUse, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			if tu := buildKiroToolUse(m); tu.Name != "" || tu.ToolUseID != "" {
				out = append(out, tu)
			}
		}
	}
	return out
}

func buildKiroToolUse(m map[string]any) kiroclaude.KiroToolUse {
	tu := kiroclaude.KiroToolUse{
		ToolUseID: firstKiroString(m, "toolUseId", "id"),
		Name:      firstKiroString(m, "name", "toolName"),
		Input:     map[string]any{},
	}
	switch input := m["input"].(type) {
	case map[string]any:
		tu.Input = input
	case string:
		var parsed map[string]any
		if err := json.Unmarshal([]byte(input), &parsed); err == nil {
			tu.Input = parsed
		}
	}
	return tu
}

func extractKiroUsage(event map[string]any) usage.Detail {
	var detail usage.Detail
	applyUsageMap := func(m map[string]any) {
		inputTokens := int64Field(m, "input_tokens", "inputTokens", "prompt_tokens", "promptTokens")
		outputTokens := int64Field(m, "output_tokens", "outputTokens", "completion_tokens", "completionTokens")
		totalTokens := int64Field(m, "total_tokens", "totalTokens", "total")
		reasoningTokens := int64Field(m, "reasoning_tokens", "reasoningTokens")
		uncachedInputTokens := int64Field(m, "uncachedInputTokens", "uncached_input_tokens")
		cacheReadTokens := int64Field(m,
			"cacheReadInputTokens",
			"cache_read_input_tokens",
			"cache_read_tokens",
			"cacheReadTokens",
			"cachedInputTokens",
			"cached_input_tokens",
			"cached_tokens",
			"cache_tokens",
			"cacheTokens",
		)
		cacheWriteTokens := int64Field(m,
			"cacheWriteInputTokens",
			"cache_write_input_tokens",
			"cache_creation_input_tokens",
			"cacheCreationInputTokens",
		)
		if nested, ok := m["input_tokens_details"].(map[string]any); ok {
			cacheReadTokens = firstPositiveInt64(cacheReadTokens, int64Field(nested, "cached_tokens", "cachedTokens", "cache_tokens", "cacheTokens"))
		}
		if nested, ok := m["prompt_tokens_details"].(map[string]any); ok {
			cacheReadTokens = firstPositiveInt64(cacheReadTokens, int64Field(nested, "cached_tokens", "cachedTokens", "cache_tokens", "cacheTokens"))
		}
		if nested, ok := m["output_tokens_details"].(map[string]any); ok {
			reasoningTokens = firstPositiveInt64(reasoningTokens, int64Field(nested, "reasoning_tokens", "reasoningTokens"))
		}
		if nested, ok := m["completion_tokens_details"].(map[string]any); ok {
			reasoningTokens = firstPositiveInt64(reasoningTokens, int64Field(nested, "reasoning_tokens", "reasoningTokens"))
		}
		if uncachedInputTokens > 0 || cacheReadTokens > 0 || cacheWriteTokens > 0 {
			splitInputTokens := uncachedInputTokens + cacheReadTokens + cacheWriteTokens
			if inputTokens < splitInputTokens {
				inputTokens = splitInputTokens
			}
		}
		if inputTokens > 0 {
			detail.InputTokens = inputTokens
		}
		if outputTokens > 0 {
			detail.OutputTokens = outputTokens
		}
		if totalTokens > 0 {
			detail.TotalTokens = totalTokens
		}
		if reasoningTokens > 0 {
			detail.ReasoningTokens = reasoningTokens
		}
		if cacheReadTokens > 0 {
			// CachedTokens is the discounted cache-read bucket consumed by
			// cost calculators. Claude clients render this as
			// `usage.cache_read_input_tokens`.
			detail.CachedTokens = cacheReadTokens
		}
		if cacheWriteTokens > 0 {
			// CacheCreationTokens captures the tokens billed for populating a
			// new cache entry. Kept separate from CachedTokens so downstream
			// translators can emit both `cache_read_input_tokens` and
			// `cache_creation_input_tokens` — Claude Code and other prompt-
			// cache-aware clients inspect both fields to verify caching
			// efficacy.
			detail.CacheCreationTokens = cacheWriteTokens
		}
	}
	var applyUsageContainer func(map[string]any)
	applyUsageContainer = func(m map[string]any) {
		if len(m) == 0 {
			return
		}
		applyUsageMap(m)
		for _, key := range []string{
			"usage",
			"tokenUsage",
			"token_usage",
			"messageMetadataEvent",
			"metadataEvent",
			"usageEvent",
			"metadata",
			"usage_metadata",
			"response_metadata",
		} {
			if nested, ok := m[key].(map[string]any); ok {
				applyUsageContainer(nested)
			}
		}
	}
	applyUsageContainer(event)
	if detail.InputTokens == 0 && detail.TotalTokens > detail.OutputTokens+detail.ReasoningTokens {
		detail.InputTokens = detail.TotalTokens - detail.OutputTokens - detail.ReasoningTokens
	}
	if detail.TotalTokens == 0 && (detail.InputTokens > 0 || detail.OutputTokens > 0 || detail.ReasoningTokens > 0) {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	return detail
}

func mergeKiroUsage(dst *usage.Detail, src usage.Detail) {
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.ReasoningTokens > 0 {
		dst.ReasoningTokens = src.ReasoningTokens
	}
	if src.CachedTokens > 0 {
		dst.CachedTokens = src.CachedTokens
	}
	if src.CacheCreationTokens > 0 {
		dst.CacheCreationTokens = src.CacheCreationTokens
	}
	if src.TotalTokens > 0 {
		dst.TotalTokens = src.TotalTokens
	} else if dst.TotalTokens == 0 && (dst.InputTokens > 0 || dst.OutputTokens > 0 || dst.ReasoningTokens > 0) {
		dst.TotalTokens = dst.InputTokens + dst.OutputTokens + dst.ReasoningTokens
	}
}

func numberField(m map[string]any, keys ...string) float64 {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case json.Number:
			f, _ := v.Float64()
			return f
		case string:
			f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return f
		}
	}
	return 0
}

func int64Field(m map[string]any, keys ...string) int64 {
	v := numberField(m, keys...)
	if v <= 0 {
		return 0
	}
	return int64(v)
}

func firstPositiveInt64(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstKiroString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func buildKiroPayloadForFormat(body []byte, modelID, profileArn, origin string, isAgentic, isChatOnly bool, sourceFormat sdktranslator.Format, headers http.Header) ([]byte, bool) {
	switch sourceFormat.String() {
	case "openai":
		return kiroopenai.BuildKiroPayloadFromOpenAI(body, modelID, profileArn, origin, isAgentic, isChatOnly, headers, nil)
	case "kiro":
		return sanitizeKiroPayload(body, modelID, profileArn, origin), false
	default:
		return kiroclaude.BuildKiroPayload(body, modelID, profileArn, origin, isAgentic, isChatOnly, headers, nil)
	}
}

func sanitizeKiroPayload(body []byte, modelID, profileArn, origin string) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	delete(payload, "user")
	if strings.TrimSpace(profileArn) != "" {
		payload["profileArn"] = strings.TrimSpace(profileArn)
	} else {
		delete(payload, "profileArn")
	}
	normalizeKiroNativePayload(payload, strings.TrimSpace(modelID), strings.TrimSpace(origin))
	rewriteKiroNativeFields(payload, strings.TrimSpace(modelID), strings.TrimSpace(origin))
	sanitized, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return sanitized
}

func normalizeKiroNativePayload(payload map[string]any, modelID, origin string) {
	if payload == nil {
		return
	}
	state, _ := payload["conversationState"].(map[string]any)
	if state == nil {
		state = map[string]any{}
		payload["conversationState"] = state
	}
	if strings.TrimSpace(stringFromAny(state["chatTriggerType"])) == "" {
		state["chatTriggerType"] = "MANUAL"
	}
	if strings.TrimSpace(stringFromAny(state["agentTaskType"])) == "" {
		state["agentTaskType"] = kiroIDEAgentMode
	}
	if strings.TrimSpace(stringFromAny(state["conversationId"])) == "" {
		state["conversationId"] = uuid.New().String()
	}

	currentMessage, _ := state["currentMessage"].(map[string]any)
	if currentMessage == nil {
		return
	}
	userInput, _ := currentMessage["userInputMessage"].(map[string]any)
	if userInput == nil {
		return
	}
	if strings.TrimSpace(stringFromAny(userInput["modelId"])) == "" && modelID != "" {
		userInput["modelId"] = modelID
	}
	if strings.TrimSpace(stringFromAny(userInput["origin"])) == "" && origin != "" {
		userInput["origin"] = origin
	}
}

func rewriteKiroNativeFields(value any, modelID, origin string) {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			switch key {
			case "modelId":
				if modelID != "" {
					node[key] = modelID
				}
			case "origin":
				if origin != "" {
					node[key] = origin
				}
			default:
				rewriteKiroNativeFields(child, modelID, origin)
			}
		}
	case []any:
		for _, child := range node {
			rewriteKiroNativeFields(child, modelID, origin)
		}
	}
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return ""
	}
}

func kiroCredentials(auth *cliproxyauth.Auth) (accessToken, profileArn string) {
	if auth == nil {
		return "", ""
	}
	// Read each field independently so Metadata-only and Attributes-only
	// configurations (or any mix) all work. Previously this function
	// treated Attributes as a whole-record fallback: if Metadata supplied
	// an access_token but no profile_arn, the Attributes profile_arn was
	// silently dropped and the request was sent with an empty ARN —
	// IdC / Identity Center endpoints then return a 400 that looks like
	// a transient upstream issue.
	if auth.Metadata != nil {
		accessToken = metadataString(auth.Metadata, "access_token", "accessToken")
		profileArn = metadataString(auth.Metadata, "profile_arn", "profileArn")
	}
	if auth.Attributes != nil {
		if accessToken == "" {
			accessToken = strings.TrimSpace(auth.Attributes["access_token"])
		}
		if profileArn == "" {
			profileArn = strings.TrimSpace(auth.Attributes["profile_arn"])
		}
	}
	return accessToken, profileArn
}

func metadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func kiroRefreshIntervalFromAuth(auth *cliproxyauth.Auth) time.Duration {
	if auth == nil {
		return 0
	}
	if d := kiroDurationFromMap(auth.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if len(auth.Attributes) == 0 {
		return 0
	}
	for _, key := range []string{"refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"} {
		if d := kiroParseDurationValue(auth.Attributes[key]); d > 0 {
			return d
		}
	}
	return 0
}

func kiroLastRefreshTime(auth *cliproxyauth.Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	if ts, ok := kiroTimeFromMap(auth.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
		return ts, true
	}
	if len(auth.Attributes) == 0 {
		return time.Time{}, false
	}
	for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
		if ts, ok := kiroParseTimeValue(auth.Attributes[key]); ok {
			return ts, true
		}
	}
	return time.Time{}, false
}

func kiroDurationFromMap(metadata map[string]any, keys ...string) time.Duration {
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if d := kiroParseDurationValue(value); d > 0 {
				return d
			}
		}
	}
	return 0
}

func kiroParseDurationValue(value any) time.Duration {
	switch v := value.(type) {
	case int:
		if v > 0 {
			return time.Duration(v) * time.Second
		}
	case int64:
		if v > 0 {
			return time.Duration(v) * time.Second
		}
	case float64:
		if v > 0 {
			return time.Duration(v) * time.Second
		}
	case json.Number:
		if i, err := v.Int64(); err == nil && i > 0 {
			return time.Duration(i) * time.Second
		}
		if f, err := strconv.ParseFloat(v.String(), 64); err == nil && f > 0 {
			return time.Duration(f) * time.Second
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0
		}
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil && i > 0 {
			return time.Duration(i) * time.Second
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return time.Duration(f) * time.Second
		}
	}
	return 0
}

func kiroTimeFromMap(metadata map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if ts, ok := kiroParseTimeValue(value); ok {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func kiroParseTimeValue(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		if !v.IsZero() {
			return v, true
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}, false
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts, true
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts, true
		}
	case json.Number:
		if i, err := v.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0), true
		}
	case int64:
		if v > 0 {
			return time.Unix(v, 0), true
		}
	case float64:
		if v > 0 {
			return time.Unix(int64(v), 0), true
		}
	}
	return time.Time{}, false
}

func determineAgenticMode(model string) (bool, bool) {
	return strings.HasSuffix(model, "-agentic"), strings.HasSuffix(model, "-chat")
}

func getEffectiveProfileArn(auth *cliproxyauth.Auth, profileArn string) string {
	if auth != nil && auth.Metadata != nil {
		authMethod := strings.ToLower(metadataString(auth.Metadata, "auth_method", "authMethod"))
		provider := strings.ToLower(metadataString(auth.Metadata, "provider"))
		authType := strings.ToLower(metadataString(auth.Metadata, "auth_type", "authType"))
		if authType == "aws_sso_oidc" || isKiroSSOAuth(authMethod, provider) {
			return ""
		}
	}
	return profileArn
}

func getKiroEndpointConfigs(auth *cliproxyauth.Auth) []kiroEndpointConfig {
	region := kiroDefaultRegion
	if auth != nil && auth.Metadata != nil {
		if r := metadataString(auth.Metadata, "api_region"); r != "" {
			region = r
		} else if arn := metadataString(auth.Metadata, "profile_arn", "profileArn"); arn != "" {
			if arnRegion := extractRegionFromProfileARN(arn); arnRegion != "" {
				region = arnRegion
			}
		}
	}
	endpoints := buildKiroEndpointConfigs(region)
	preference := ""
	if auth != nil {
		if auth.Metadata != nil {
			preference = metadataString(auth.Metadata, "preferred_endpoint", "preferredEndpoint", "preferred-endpoint")
		}
		if preference == "" && auth.Attributes != nil {
			preference = strings.TrimSpace(auth.Attributes["preferred_endpoint"])
			if preference == "" {
				preference = strings.TrimSpace(auth.Attributes["preferred-endpoint"])
			}
		}
	}
	return sortKiroEndpointsByPreference(endpoints, preference)
}

func extractRegionFromProfileARN(profileArn string) string {
	parts := strings.Split(profileArn, ":")
	if len(parts) >= 4 {
		return strings.TrimSpace(parts[3])
	}
	return ""
}

func sortKiroEndpointsByPreference(endpoints []kiroEndpointConfig, preference string) []kiroEndpointConfig {
	preference = strings.ToLower(strings.TrimSpace(preference))
	if preference == "" || len(endpoints) < 2 {
		return endpoints
	}
	matchesPreference := func(endpoint kiroEndpointConfig) bool {
		name := strings.ToLower(strings.TrimSpace(endpoint.Name))
		switch preference {
		case "q", "amazonq", "amazon-q":
			return name == "amazonq"
		case "codewhisperer", "code-whisperer", "ide", "kiro-ide":
			return name == "codewhisperer"
		default:
			return name == preference
		}
	}
	out := make([]kiroEndpointConfig, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if matchesPreference(endpoint) {
			out = append(out, endpoint)
		}
	}
	if len(out) == 0 {
		return endpoints
	}
	for _, endpoint := range endpoints {
		if !matchesPreference(endpoint) {
			out = append(out, endpoint)
		}
	}
	return out
}

func shouldTryNextKiroEndpoint(err error) bool {
	if err == nil {
		return false
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) {
		return false
	}
	switch status.StatusCode() {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusNotImplemented:
		return true
	case http.StatusBadRequest:
		msg := strings.ToLower(err.Error())
		return strings.Contains(msg, "unknownoperation") ||
			strings.Contains(msg, "unsupportedoperation") ||
			strings.Contains(msg, "unknown operation") ||
			strings.Contains(msg, "unsupported operation")
	default:
		return false
	}
}

func (e *KiroExecutor) mapModelToKiro(model string) string {
	model = strings.TrimSpace(model)
	modelMap := map[string]string{
		"amazonq-auto":                    "auto",
		"amazonq-claude-opus-4.5":         "claude-opus-4.5",
		"amazonq-claude-sonnet-4.5":       "claude-sonnet-4.5",
		"amazonq-claude-sonnet-4":         "claude-sonnet-4",
		"amazonq-claude-haiku-4.5":        "claude-haiku-4.5",
		"kiro-auto":                       "auto",
		"kiro-claude-opus-4-7":            "claude-opus-4.7",
		"kiro-claude-opus-4-6":            "claude-opus-4.6",
		"kiro-claude-sonnet-4-6":          "claude-sonnet-4.6",
		"kiro-claude-opus-4-5":            "claude-opus-4.5",
		"kiro-claude-sonnet-4-5":          "claude-sonnet-4.5",
		"kiro-claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
		"kiro-claude-sonnet-4":            "claude-sonnet-4",
		"kiro-claude-sonnet-4-20250514":   "claude-sonnet-4",
		"kiro-claude-haiku-4-5":           "claude-haiku-4.5",
		"kiro-deepseek-3-2":               "deepseek-3.2",
		"kiro-minimax-m2-5":               "minimax-m2.5",
		"kiro-minimax-m2-1":               "minimax-m2.1",
		"kiro-glm-5":                      "glm-5",
		"kiro-qwen3-coder-next":           "qwen3-coder-next",
		"claude-opus-4.7":                 "claude-opus-4.7",
		"claude-opus-4-7":                 "claude-opus-4.7",
		"claude-opus-4.6":                 "claude-opus-4.6",
		"claude-opus-4-6":                 "claude-opus-4.6",
		"claude-sonnet-4.6":               "claude-sonnet-4.6",
		"claude-sonnet-4-6":               "claude-sonnet-4.6",
		"claude-sonnet-4.7":               "claude-sonnet-4.7",
		"claude-sonnet-4-7":               "claude-sonnet-4.7",
		"claude-opus-4.5":                 "claude-opus-4.5",
		"claude-opus-4-5":                 "claude-opus-4.5",
		"claude-sonnet-4.5":               "claude-sonnet-4.5",
		"claude-sonnet-4-5":               "claude-sonnet-4.5",
		"claude-sonnet-4":                 "claude-sonnet-4",
		"claude-haiku-4.5":                "claude-haiku-4.5",
		"claude-haiku-4-5":                "claude-haiku-4.5",
		"deepseek-3.2":                    "deepseek-3.2",
		"deepseek-3-2":                    "deepseek-3.2",
		"minimax-m2.5":                    "minimax-m2.5",
		"minimax-m2-5":                    "minimax-m2.5",
		"minimax-m2.1":                    "minimax-m2.1",
		"minimax-m2-1":                    "minimax-m2.1",
		"glm-5":                           "glm-5",
		"qwen3-coder-next":                "qwen3-coder-next",
		"auto":                            "auto",
	}
	trimmed := strings.TrimSuffix(model, "-agentic")
	trimmed = strings.TrimSuffix(trimmed, "-chat")
	if kiroID, ok := modelMap[trimmed]; ok {
		return kiroID
	}
	if strings.HasPrefix(trimmed, "kiro-") {
		return denormalizeKiroModelID(strings.TrimPrefix(trimmed, "kiro-"))
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.Contains(lower, "haiku"):
		return "claude-haiku-4.5"
	case strings.Contains(lower, "opus"):
		if strings.Contains(lower, "4-7") || strings.Contains(lower, "4.7") {
			return "claude-opus-4.7"
		}
		if strings.Contains(lower, "4-6") || strings.Contains(lower, "4.6") {
			return "claude-opus-4.6"
		}
		return "claude-opus-4.5"
	case strings.Contains(lower, "sonnet"):
		if strings.Contains(lower, "4-7") || strings.Contains(lower, "4.7") {
			return "claude-sonnet-4.7"
		}
		if strings.Contains(lower, "4-6") || strings.Contains(lower, "4.6") {
			return "claude-sonnet-4.6"
		}
		if strings.Contains(lower, "4-5") || strings.Contains(lower, "4.5") {
			return "claude-sonnet-4.5"
		}
		if strings.Contains(lower, "4") {
			return "claude-sonnet-4"
		}
		return "claude-sonnet-4.5"
	default:
		return "claude-sonnet-4.5"
	}
}

func denormalizeKiroModelID(modelID string) string {
	parts := strings.Split(strings.TrimSpace(modelID), "-")
	for i := 0; i+1 < len(parts); i++ {
		if isKiroNumericVersionPart(parts[i]) && isKiroNumericVersionPart(parts[i+1]) {
			parts[i] = parts[i] + "." + parts[i+1]
			return strings.Join(append(parts[:i+1], parts[i+2:]...), "-")
		}
	}
	return modelID
}

func isKiroNumericVersionPart(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

type kiroRefreshTokenData struct {
	AccessToken  string
	RefreshToken string
	ProfileArn   string
	ExpiresAt    string
}

func refreshKiroSocialToken(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, refreshToken, region string) (*kiroRefreshTokenData, error) {
	if !isSafeAWSRegion(region) {
		return nil, fmt.Errorf("kiro: invalid AWS region %q", region)
	}
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("kiro: refresh token is required")
	}
	payload := map[string]string{"refreshToken": refreshToken}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	endpoint := kiroSocialRefreshEndpoint(region)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("User-Agent", kiroCLIUserAgent)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 30*time.Second)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer closeKiroResponseBody(httpResp)
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		if permErr := classifyKiroOAuthError(httpResp.StatusCode, respBody); permErr != nil {
			log.Warnf("kiro: social refresh rejected as permanent (auth=%s status=%d): %v", auth.ID, httpResp.StatusCode, permErr)
			return nil, permErr
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(respBody)}
	}
	var result struct {
		AccessToken       string `json:"accessToken"`
		AccessTokenLegacy string `json:"access_token"`
		RefreshToken      string `json:"refreshToken"`
		RefreshTokenOld   string `json:"refresh_token"`
		ProfileArn        string `json:"profileArn"`
		ProfileArnOld     string `json:"profile_arn"`
		ExpiresAt         string `json:"expiresAt"`
		ExpiresAtOld      string `json:"expires_at"`
		ExpiresIn         int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	accessToken := firstNonEmptyKiroString(result.AccessToken, result.AccessTokenLegacy)
	refreshTokenResult := firstNonEmptyKiroString(result.RefreshToken, result.RefreshTokenOld)
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("kiro: social token refresh response missing access token")
	}
	if strings.TrimSpace(refreshTokenResult) == "" {
		// Upstream rotated but returned empty refreshToken — refuse to persist
		// a mixed (new access + old refresh) state because the next refresh
		// would fail with invalid_grant. Log and bail so the caller keeps
		// the previous credentials intact.
		log.Warnf("kiro: social refresh returned empty refreshToken for auth=%s; discarding rotation to avoid invalid_grant on next call", auth.ID)
		return nil, fmt.Errorf("kiro: social token refresh response missing refresh token (will retry with existing credentials)")
	}
	expiresAt := firstNonEmptyKiroString(result.ExpiresAt, result.ExpiresAtOld)
	if expiresAt == "" || !isRFC3339Time(expiresAt) {
		if result.ExpiresIn <= 0 {
			result.ExpiresIn = 3600
		}
		expiresAt = time.Now().UTC().Add(time.Duration(result.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	return &kiroRefreshTokenData{
		AccessToken:  strings.TrimSpace(accessToken),
		RefreshToken: strings.TrimSpace(refreshTokenResult),
		ProfileArn:   firstNonEmptyKiroString(result.ProfileArn, result.ProfileArnOld),
		ExpiresAt:    expiresAt,
	}, nil
}

func refreshKiroSSOToken(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, clientID, clientSecret, refreshToken, region string) (*kiroRefreshTokenData, error) {
	if !isSafeAWSRegion(region) {
		return nil, fmt.Errorf("kiro: invalid AWS region %q", region)
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 30*time.Second)
	ssoClient := kiroauth.NewSSOOIDCClientWithHTTPClient(kiroSSOTokenBaseEndpoint(kiroSSOTokenEndpoint(region)), httpClient)
	result, err := ssoClient.RefreshToken(ctx, clientID, clientSecret, refreshToken)
	if err != nil {
		var status *kiroauth.StatusError
		if errors.As(err, &status) {
			body := []byte(status.Body)
			if permErr := classifyKiroOAuthError(status.StatusCode, body); permErr != nil {
				log.Warnf("kiro: SSO refresh rejected as permanent (auth=%s status=%d): %v", auth.ID, status.StatusCode, permErr)
				return nil, permErr
			}
			return nil, statusErr{code: status.StatusCode, msg: status.Body}
		}
		return nil, err
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return nil, fmt.Errorf("kiro: token refresh response missing access token")
	}
	if strings.TrimSpace(result.RefreshToken) == "" {
		// AWS SSO-OIDC rotates the refresh token on every successful call.
		// If upstream returned 200 but no refreshToken, keeping the old one
		// would fail next time. Surface the anomaly instead of silently
		// preserving the old value.
		log.Warnf("kiro: SSO refresh returned empty refreshToken for auth=%s; discarding rotation to avoid invalid_grant on next call", auth.ID)
		return nil, fmt.Errorf("kiro: SSO token refresh response missing refresh token (will retry with existing credentials)")
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 3600
	}
	return &kiroRefreshTokenData{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    time.Now().UTC().Add(time.Duration(result.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

func kiroSSOTokenBaseEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if strings.HasSuffix(endpoint, "/token") {
		endpoint = strings.TrimRight(strings.TrimSuffix(endpoint, "/token"), "/")
	}
	return endpoint
}

func isRFC3339Time(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}

func firstNonEmptyKiroString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isSafeAWSRegion(region string) bool {
	if region == "" {
		return false
	}
	for _, r := range region {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return strings.Count(region, "-") >= 2
}

func closeKiroResponseBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	if err := resp.Body.Close(); err != nil {
		log.Errorf("kiro executor: close response body error: %v", err)
	}
}

func authLogInfo(auth *cliproxyauth.Auth) (id, label, accountType, accountValue string) {
	if auth == nil {
		return "", "", "", ""
	}
	accountType, accountValue = auth.AccountInfo()
	return auth.ID, auth.Label, accountType, accountValue
}

func isUnauthorizedStatusErr(err error) bool {
	if err == nil {
		return false
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) {
		return isKiroInvalidBearerTokenMessage(err.Error())
	}
	code := status.StatusCode()
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return true
	}
	return isKiroInvalidBearerTokenMessage(err.Error())
}

func isKiroInvalidBearerTokenMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "bearer token") &&
		(strings.Contains(lower, "invalid") ||
			strings.Contains(lower, "expired") ||
			strings.Contains(lower, "unauthorized"))
}

func isKiroBadCredentialsMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "bad credentials")
}
