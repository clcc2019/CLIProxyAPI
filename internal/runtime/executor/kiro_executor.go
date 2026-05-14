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
	"runtime"
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
	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
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
	kiroIDEVersion         = "0.12.155"
	kiroIDEAWSSDKVersion   = "1.0.34"
	kiroIDEAPIClient       = "codewhispererstreaming"
	kiroCLIUserAgent       = "aws-sdk-rust/1.3.9 os/linux lang/rust/1.92.0"
	kiroCLIAmzAgent        = "aws-sdk-rust/1.3.9 ua/2.1 api/ssooidc/1.92.0 os/linux lang/rust/1.92.0 m/E app/AmazonQ-For-CLI"
	kiroRefreshSkew        = 2 * time.Minute

	// kiroStreamIdleTimeout bounds how long we'll wait on a single event-
	// stream frame from Kiro before treating the stream as dead. Kiro
	// normally emits content-delta frames every few hundred ms; a silent
	// upstream beyond this window is almost always a hung connection. The
	// value is generous enough to survive the LLM's first-token latency
	// (which can exceed 20s for large prompts + cold caches) while still
	// closing stuck connections in human-time rather than process-lifetime.
	kiroStreamIdleTimeout = 90 * time.Second
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
	httpResp, actualPayload, err := e.doKiroRequestWithFallbackRetry(ctx, auth, prepared, accessToken)
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
					httpResp, actualPayload, err = e.doKiroRequestWithFallback(ctx, auth, prepared, accessToken)
				}
			}
		}
		if err != nil {
			return resp, err
		}
	}
	defer closeKiroResponseBody(httpResp)

	content, toolUses, usageInfo, stopReason, parseErr := e.parseEventStream(ctx, httpResp.Body)
	if parseErr != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, parseErr)
		fields := log.Fields{"provider": "kiro"}
		if auth != nil {
			fields["auth_id"] = auth.ID
		}
		if requestID := firstNonEmptyKiroString(httpResp.Header.Get("x-amzn-requestid"), httpResp.Header.Get("x-request-id")); requestID != "" {
			fields["request_id"] = requestID
		}
		log.WithFields(fields).Warnf("kiro: upstream event stream failed: %v", parseErr)
		return resp, parseErr
	}
	// Kiro's upstream frequently omits tokenUsage entirely — fill the gaps
	// with a local cl100k_base estimate so aggregated statistics never show
	// zero input/output tokens for a successful request.
	if report := fillKiroUsageEstimates(&usageInfo, kiroUsageRequestPayload(prepared, actualPayload), content, toolUses); report.FilledInput || report.FilledOutput {
		log.WithFields(kiroUsageEstimateLogKV(usageInfo, report)).Debug("kiro: usage filled from local estimator (non-stream)")
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
	httpResp, actualPayload, err := e.doKiroRequestWithFallbackRetry(ctx, auth, prepared, accessToken)
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
					httpResp, actualPayload, err = e.doKiroRequestWithFallback(ctx, auth, prepared, accessToken)
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
		e.streamToChannel(ctx, httpResp.Body, out, prepared.from, helps.PayloadRequestedModel(opts, req.Model), opts.OriginalRequest, kiroUsageRequestPayload(prepared, actualPayload), reporter)
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
		if required {
			return auth, accessToken, profileArn, err
		}
		log.Debugf("kiro executor: opportunistic refresh failed for auth=%s; continuing with current access token: %v", auth.ID, err)
		return auth, accessToken, profileArn, nil
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
		return true, false
	}
	if !lastRefresh.Add(interval).After(now) {
		return true, false
	}
	if !auth.NextRefreshAfter.IsZero() {
		return !now.Before(auth.NextRefreshAfter), false
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
	updated.Metadata["machine_id"] = kiroMachineIDFromAuth(updated)
	if updated.Attributes == nil {
		updated.Attributes = map[string]string{}
	}
	updated.Attributes["machine_id"] = kiroMachineIDFromAuth(updated)
	if tokenData.ProfileArn != "" {
		updated.Metadata["profile_arn"] = tokenData.ProfileArn
		updated.Attributes["profile_arn"] = tokenData.ProfileArn
	}
	sdkauth.RemoveEmptyKiroMetadataFields(updated.Metadata)
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
	// Unknown classification with AWS client credentials should use the AWS
	// SSO-OIDC token endpoint. Some imported Builder ID / IDC credentials do
	// not carry a reliable auth_method/provider marker, but the presence of
	// clientId/clientSecret is enough to construct the refresh_token grant.
	return true
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

func kiroUsageRequestPayload(prepared *kiroPreparedRequest, actualPayload []byte) []byte {
	if len(actualPayload) > 0 {
		return actualPayload
	}
	if prepared == nil {
		return nil
	}
	if len(prepared.firstPayload) > 0 {
		return prepared.firstPayload
	}
	return prepared.translated
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
		preparedPayload, prepareStats, prepareErr := prepareKiroPayloadForUpstream(payload)
		if prepareStats.changed() {
			authID, authLabel, _, _ := authLogInfo(auth)
			fields := log.Fields{
				"endpoint":                 endpoint.Name,
				"origin":                   endpoint.Origin,
				"original_bytes":           prepareStats.OriginalBytes,
				"final_bytes":              prepareStats.FinalBytes,
				"original_history_entries": prepareStats.OriginalHistoryEntries,
				"final_history_entries":    prepareStats.FinalHistoryEntries,
				"normalized_history":       prepareStats.NormalizedHistory,
				"stripped_tool_context":    prepareStats.StrippedToolContext,
				"repaired_tool_results":    prepareStats.RepairedToolResults,
				"trimmed_history":          prepareStats.TrimmedHistory,
				"compacted":                prepareStats.Compacted,
			}
			if authID != "" {
				fields["auth_id"] = authID
			}
			if authLabel != "" {
				fields["auth_label"] = authLabel
			}
			log.WithFields(fields).Info("kiro: prepared request payload before upstream")
		}
		if prepareErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, prepareErr)
			log.WithFields(log.Fields{
				"endpoint":    endpoint.Name,
				"origin":      endpoint.Origin,
				"final_bytes": prepareStats.FinalBytes,
			}).Warnf("kiro: refusing payload before upstream: %v", prepareErr)
			return nil, preparedPayload, prepareErr
		}
		payload = preparedPayload
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
	applyKiroRuntimeIdentityHeaders(httpReq, auth)
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

	if err := validateKiroGeneratePayload(payload); err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		log.WithFields(log.Fields{
			"auth_id":  authID,
			"endpoint": endpointConfig.Name,
			"url":      endpointConfig.URL,
		}).Warnf("kiro: refusing malformed request before upstream: %v", err)
		return nil, err
	}

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
	logKiroUpstreamHTTPError(auth, endpointConfig, httpResp, b)
	retryAfter := kiroRetryAfterFromHeaders(httpResp.Header, time.Now())
	closeKiroResponseBody(httpResp)
	return nil, statusErr{code: httpResp.StatusCode, msg: string(b), retryAfter: retryAfter}
}

type kiroEventStreamMessage struct {
	EventType string
	Payload   []byte
}

type kiroFrameResult struct {
	msg *kiroEventStreamMessage
	err error
}

func (e *KiroExecutor) parseEventStream(ctx context.Context, body io.Reader) (string, []kiroclaude.KiroToolUse, usage.Detail, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
		helps.AppendAPIResponseChunk(ctx, e.cfg, msg.Payload)
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

// streamToChannel drives the Kiro event-stream pipeline until the upstream
// closes or errors. Behaviour is fully encapsulated in kiroStreamState (see
// kiro_stream_state.go) — this function is intentionally short so the flow
// is legible at a glance: read a frame, parse it, ask the state machine to
// process it, repeat; on exit, finalize (which closes open blocks, infers
// stop_reason, estimates usage, and emits the terminal frames).
//
// When `body` also implements io.Closer (true for *http.Response.Body) the
// idle-timeout watchdog gets a handle it can use to forcibly close the
// connection and unblock a hung read. If the caller supplies a plain Reader
// the timeout degrades gracefully to a best-effort ctx cancellation.
func (e *KiroExecutor) streamToChannel(ctx context.Context, body io.Reader, out chan<- cliproxyexecutor.StreamChunk, targetFormat sdktranslator.Format, model string, originalReq, kiroReq []byte, reporter *helps.UsageReporter) {
	reader := bufio.NewReaderSize(body, kiroStreamReaderBuffer)
	state := newKiroStreamState(ctx, e, out, targetFormat, model, originalReq, kiroReq, reporter)
	defer state.finalize()

	// Optional closer for the idle-timeout watchdog. When the underlying
	// body is an http.Response.Body, closing it unblocks any in-flight
	// read immediately; the read returns with a "connection closed" error
	// which we surface to the client via handleReadError.
	bodyCloser, _ := body.(io.Closer)
	frames := e.startKiroFrameReader(ctx, reader)
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	if kiroStreamIdleTimeout > 0 {
		idleTimer = time.NewTimer(kiroStreamIdleTimeout)
		idleC = idleTimer.C
		defer stopKiroIdleTimer(idleTimer)
	}

	for {
		select {
		case <-ctx.Done():
			if bodyCloser != nil {
				_ = bodyCloser.Close()
			}
			return
		case <-idleC:
			if bodyCloser != nil {
				_ = bodyCloser.Close()
			}
			state.handleReadError(fmt.Errorf("kiro: upstream idle for %s (no frame received); closing stream", kiroStreamIdleTimeout))
			return
		case r, ok := <-frames:
			stopKiroIdleTimer(idleTimer)
			if !ok {
				return
			}
			if r.err != nil {
				state.handleReadError(r.err)
				return
			}
			if r.msg == nil {
				// Clean EOF: flush any tool_use that was mid-assembly.
				state.flushTrailingToolUse()
				return
			}

			helps.AppendAPIResponseChunk(ctx, e.cfg, r.msg.Payload)
			event := parseKiroEventPayload(r.msg.Payload, r.msg.EventType)
			if event.err != nil {
				state.handleParseError(event.err)
				return
			}
			state.ensureMessageStart()
			state.processEvent(event)
			resetKiroIdleTimer(idleTimer, kiroStreamIdleTimeout)
		}
	}
}

func (e *KiroExecutor) startKiroFrameReader(ctx context.Context, reader *bufio.Reader) <-chan kiroFrameResult {
	out := make(chan kiroFrameResult, 1)
	go func() {
		defer close(out)
		for {
			msg, err := e.readEventStreamMessage(reader)
			result := kiroFrameResult{msg: msg, err: err}
			select {
			case out <- result:
			case <-ctx.Done():
				return
			}
			if err != nil || msg == nil {
				return
			}
		}
	}()
	return out
}

func stopKiroIdleTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetKiroIdleTimer(timer *time.Timer, timeout time.Duration) {
	if timer == nil || timeout <= 0 {
		return
	}
	stopKiroIdleTimer(timer)
	timer.Reset(timeout)
}

// readFrameWithIdleTimeout wraps readEventStreamMessage with an idle-timeout
// watchdog. When the read doesn't complete within `timeout` we close the
// underlying body (if supported) to unblock the read and return a timeout
// error. The read runs in its own goroutine so we can select on (result,
// timer, ctx); when the watchdog fires we must still wait for that goroutine
// to return via the result channel, otherwise it leaks.
//
// This is the defense against hung upstream connections — Kiro's Amazon Q
// endpoint occasionally stops sending bytes without closing the TCP stream,
// which would otherwise pin a goroutine and a connection slot forever.
func (e *KiroExecutor) readFrameWithIdleTimeout(ctx context.Context, reader *bufio.Reader, closer io.Closer, timeout time.Duration) (*kiroEventStreamMessage, error) {
	if timeout <= 0 {
		// Opt-out path used by tests that set a zero timeout.
		return e.readEventStreamMessage(reader)
	}

	type frameResult struct {
		msg *kiroEventStreamMessage
		err error
	}
	result := make(chan frameResult, 1)
	go func() {
		msg, err := e.readEventStreamMessage(reader)
		result <- frameResult{msg: msg, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-result:
		return r.msg, r.err
	case <-timer.C:
		// Kiro upstream has been silent for `timeout`. Close the body so
		// the blocked read returns promptly; wait for the goroutine so we
		// don't leak it. The reader goroutine observes its read fail with
		// something like "use of closed network connection" — we discard
		// that error and surface the timeout reason instead, which is the
		// useful signal for operators.
		if closer != nil {
			_ = closer.Close()
		}
		<-result
		return nil, fmt.Errorf("kiro: upstream idle for %s (no frame received); closing stream", timeout)
	case <-ctx.Done():
		// Client disconnected mid-wait. Same cleanup: close upstream so
		// the reader goroutine unblocks, then surface ctx.Err().
		if closer != nil {
			_ = closer.Close()
		}
		<-result
		return nil, ctx.Err()
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
	// Fast path: assistantResponseEvent accounts for the vast majority of
	// stream frames (each token delta arrives as one). These frames carry
	// only `content` / `stop_reason` / `text` and occasionally `toolUses` —
	// no nested usage containers, no toolUseEvent. Skipping the full
	// `json.Unmarshal` into map[string]any saves ~14 allocations per frame.
	// If the fast-path detects fields it doesn't understand (e.g. toolUses
	// embedded in the assistant event), it falls through to the slow path.
	if fast, ok := tryParseKiroContentFastPath(payload); ok {
		return fast
	}

	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return parsedKiroEvent{err: fmt.Errorf("kiro API error: invalid event JSON: %w", err)}
	}
	if errType, _ := event["_type"].(string); errType != "" {
		return parsedKiroEvent{err: newKiroUpstreamEventError(event, errType)}
	}
	if typ, _ := event["type"].(string); typ == "error" || typ == "exception" {
		return parsedKiroEvent{err: newKiroUpstreamEventError(event, typ)}
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

// tryParseKiroContentFastPath handles assistantResponseEvent payloads that
// carry only content/stop_reason/text (the overwhelming majority of stream
// frames). It uses gjson to inspect fields in place rather than unmarshalling
// the whole object. Returns ok=false when the frame contains structures
// only the slow path can handle (toolUses, nested events, errors, reasoning,
// metadata), so the caller can fall through without risk.
//
// We tried a cheaper bytes.Contains-based pre-filter, but gjson.ForEach over
// top-level keys turned out to be faster in practice because the benchmark
// frames are short (<200 bytes) — the JIT keeps the whole payload in a few
// CPU cache lines either way, and the string-interning used by gjson's key
// comparisons avoids per-call allocations.
func tryParseKiroContentFastPath(payload []byte) (parsedKiroEvent, bool) {
	if len(payload) == 0 {
		return parsedKiroEvent{}, false
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return parsedKiroEvent{}, false
	}
	// Reject any frame that carries structures the slow path must handle.
	// The rejection list is intentionally broad — being wrong here means
	// silently dropping an event, which is much worse than taking the slow
	// path for a frame that happened to include an unused field.
	reject := false
	root.ForEach(func(key, _ gjson.Result) bool {
		switch key.String() {
		case "assistantResponseEvent", "stop_reason", "stopReason", "content", "text":
			return true
		case "toolUseEvent", "reasoningContentEvent", "reasoningContent",
			"reasoning", "thinking", "toolUses", "_type", "type",
			"usage", "tokenUsage", "token_usage", "messageMetadataEvent",
			"metadataEvent", "usageEvent", "metadata", "usage_metadata",
			"response_metadata":
			reject = true
			return false
		default:
			// Unknown key — play it safe and fall back.
			reject = true
			return false
		}
	})
	if reject {
		return parsedKiroEvent{}, false
	}

	parsed := parsedKiroEvent{}
	parsed.stopReason = firstKiroGjsonString(root, "stop_reason", "stopReason")
	if assistant := root.Get("assistantResponseEvent"); assistant.Exists() {
		// If the assistant event has toolUses the slow path must handle
		// them — fall back. Same for unknown nested keys.
		if assistant.Get("toolUses").Exists() {
			return parsedKiroEvent{}, false
		}
		parsed.content = firstKiroGjsonString(assistant, "content", "text")
		if parsed.stopReason == "" {
			parsed.stopReason = firstKiroGjsonString(assistant, "stop_reason", "stopReason")
		}
	}
	if parsed.content == "" {
		parsed.content = firstKiroGjsonString(root, "content", "text")
	}
	// Fast-path success requires at least one useful field; otherwise fall
	// through so future field additions aren't silently dropped.
	if parsed.content == "" && parsed.stopReason == "" {
		return parsedKiroEvent{}, false
	}
	return parsed, true
}

// firstKiroGjsonString returns the first non-empty string value among keys
// looked up via gjson. Mirrors firstKiroString but against a gjson.Result
// instead of a decoded map, so the fast path avoids the map allocation.
func firstKiroGjsonString(root gjson.Result, keys ...string) string {
	for _, key := range keys {
		if v := root.Get(key); v.Exists() {
			if s := v.String(); s != "" {
				return s
			}
		}
	}
	return ""
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

// extractKiroUsage parses a Kiro event for token-usage fields. Walks known
// usage containers (usage / tokenUsage / messageMetadataEvent / etc.),
// applying field values from the deepest container upward so that a nested
// block overrides a shallower one. Short-circuits when the root clearly has
// no usage to avoid paying the walk cost on content-delta / tool_use frames.
//
// Returns a zero Detail when the event has no usage; callers typically feed
// that through mergeKiroUsage() where zero values are ignored.
func extractKiroUsage(event map[string]any) usage.Detail {
	var detail usage.Detail
	// Short-circuit: most slow-path events still do not carry usage data
	// (e.g. toolUseEvent frames, content frames with a toolUses array).
	// Skip the recursive walk unless the root explicitly contains a
	// top-level token / usage container key. This cuts ~9 map lookups per
	// non-metadata event.
	if !kiroEventHasUsageContainer(event) {
		return detail
	}
	applyKiroUsageContainer(&detail, event)
	// Post-hoc reconciliation: if InputTokens is unset but Total > (Output +
	// Reasoning), synthesise InputTokens from the difference. Upstream
	// sometimes only reports `totalTokens` + `outputTokens` and expects
	// the client to subtract.
	if detail.InputTokens == 0 && detail.TotalTokens > detail.OutputTokens+detail.ReasoningTokens {
		detail.InputTokens = detail.TotalTokens - detail.OutputTokens - detail.ReasoningTokens
	}
	if detail.TotalTokens == 0 && (detail.InputTokens > 0 || detail.OutputTokens > 0 || detail.ReasoningTokens > 0) {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	return detail
}

var (
	kiroUsageContainerKeys = []string{
		"usage",
		"tokenUsage",
		"token_usage",
		"messageMetadataEvent",
		"metadataEvent",
		"usageEvent",
		"metadata",
		"usage_metadata",
		"response_metadata",
	}

	kiroInputTokenKeys = []string{
		"input_tokens", "inputTokens", "inputTokenCount", "input_token_count",
		"prompt_tokens", "promptTokens", "promptTokenCount", "prompt_token_count",
	}
	kiroOutputTokenKeys = []string{
		"output_tokens", "outputTokens", "outputTokenCount", "output_token_count",
		"completion_tokens", "completionTokens", "completionTokenCount", "completion_token_count",
	}
	kiroTotalTokenKeys = []string{
		"total_tokens", "totalTokens", "totalTokenCount", "total_token_count", "total",
	}
	kiroReasoningTokenKeys = []string{
		"reasoning_tokens", "reasoningTokens", "reasoningTokenCount", "reasoning_token_count",
	}
	kiroUncachedInputTokenKeys = []string{
		"uncachedInputTokens", "uncached_input_tokens", "uncachedInputTokenCount", "uncached_input_token_count",
	}
	kiroCacheReadTokenKeys = []string{
		"cacheReadInputTokens", "cacheReadInputTokenCount", "cache_read_input_tokens", "cache_read_input_token_count",
		"cache_read_tokens", "cache_read_token_count", "cacheReadTokens", "cacheReadTokenCount",
		"cacheHitInputTokens", "cacheHitInputTokenCount", "cache_hit_input_tokens", "cache_hit_input_token_count",
		"cacheHitTokens", "cacheHitTokenCount", "cache_hit_tokens", "cache_hit_token_count",
		"cachedInputTokens", "cachedInputTokenCount", "cached_input_tokens", "cached_input_token_count",
		"cached_tokens", "cached_token_count", "cache_tokens", "cache_token_count", "cacheTokens", "cacheTokenCount",
	}
	kiroCacheWriteTokenKeys = []string{
		"cacheWriteInputTokens", "cacheWriteInputTokenCount", "cache_write_input_tokens", "cache_write_input_token_count",
		"cache_creation_input_tokens", "cache_creation_input_token_count", "cacheCreationInputTokens", "cacheCreationInputTokenCount",
	}
	kiroInputDetailContainerKeys = []string{
		"input_tokens_details", "inputTokenDetails", "input_token_details",
		"prompt_tokens_details", "promptTokenDetails", "prompt_token_details",
	}
	kiroOutputDetailContainerKeys = []string{
		"output_tokens_details", "outputTokenDetails", "output_token_details",
		"completion_tokens_details", "completionTokenDetails", "completion_token_details",
	}
	kiroUsageFlatKeys = concatKiroUsageKeys(
		kiroInputTokenKeys,
		kiroOutputTokenKeys,
		kiroTotalTokenKeys,
		kiroReasoningTokenKeys,
		kiroUncachedInputTokenKeys,
		kiroCacheReadTokenKeys,
		kiroCacheWriteTokenKeys,
		kiroInputDetailContainerKeys,
		kiroOutputDetailContainerKeys,
	)
	kiroUsageRootKeySet = makeKiroUsageKeySet(kiroUsageFlatKeys, kiroUsageContainerKeys)
)

func concatKiroUsageKeys(groups ...[]string) []string {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	out := make([]string, 0, total)
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

func makeKiroUsageKeySet(groups ...[]string) map[string]struct{} {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	out := make(map[string]struct{}, total)
	for _, group := range groups {
		for _, key := range group {
			out[key] = struct{}{}
		}
	}
	return out
}

// applyKiroUsageContainer recursively walks `m` applying usage fields at
// each level. Kept iterative where possible; the recursion depth is bounded
// by the number of nested container keys (≤ 9) so stack use is trivial.
func applyKiroUsageContainer(detail *usage.Detail, m map[string]any) {
	if len(m) == 0 {
		return
	}
	applyKiroUsageMap(detail, m)
	for _, key := range kiroUsageContainerKeys {
		if nested, ok := m[key].(map[string]any); ok {
			applyKiroUsageContainer(detail, nested)
		}
	}
}

// applyKiroUsageMap pulls token counts out of a single usage map (one level
// of nesting). Split out of extractKiroUsage so each transform is
// independently readable; the function writes into *detail rather than
// returning a value because all fields are independently optional.
func applyKiroUsageMap(detail *usage.Detail, m map[string]any) {
	if len(m) == 0 {
		return
	}
	inputTokens := int64Field(m, kiroInputTokenKeys...)
	outputTokens := int64Field(m, kiroOutputTokenKeys...)
	totalTokens := int64Field(m, kiroTotalTokenKeys...)
	reasoningTokens := int64Field(m, kiroReasoningTokenKeys...)
	uncachedInputTokens := int64Field(m, kiroUncachedInputTokenKeys...)
	cacheReadTokens := int64Field(m, kiroCacheReadTokenKeys...)
	cacheWriteTokens := int64Field(m, kiroCacheWriteTokenKeys...)

	if cacheReadTokens == 0 {
		cacheReadTokens = kiroNestedInt64Field(m, kiroInputDetailContainerKeys, kiroCacheReadTokenKeys)
	}
	if cacheWriteTokens == 0 {
		cacheWriteTokens = kiroNestedInt64Field(m, kiroInputDetailContainerKeys, kiroCacheWriteTokenKeys)
	}
	if reasoningTokens == 0 {
		reasoningTokens = kiroNestedInt64Field(m, kiroOutputDetailContainerKeys, kiroReasoningTokenKeys)
	}
	// When the upstream splits input into uncached + cache-read + cache-write
	// but omits the total, synthesise the top-level InputTokens from the
	// sum so downstream consumers have a single billed value.
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

func kiroNestedInt64Field(m map[string]any, containerKeys, valueKeys []string) int64 {
	if len(m) == 0 || len(containerKeys) == 0 || len(valueKeys) == 0 {
		return 0
	}
	for _, containerKey := range containerKeys {
		child, ok := m[containerKey].(map[string]any)
		if !ok {
			continue
		}
		if value := int64Field(child, valueKeys...); value > 0 {
			return value
		}
	}
	return 0
}

// kiroEventHasUsageContainer checks whether `event` has any top-level key
// that could carry token usage. Pre-filter for extractKiroUsage to avoid
// recursing into content / tool_use frames that clearly have no usage.
// Keep this list in sync with applyUsageContainer inside extractKiroUsage.
func kiroEventHasUsageContainer(event map[string]any) bool {
	if len(event) == 0 {
		return false
	}
	for key := range event {
		if _, ok := kiroUsageRootKeySet[key]; ok {
			return true
		}
	}
	return false
}

// mergeKiroUsage folds a newly-extracted usage block into the running
// accumulator using max() semantics on every field. Kiro / Amazon Q can emit
// multiple messageMetadataEvent frames in a single response — occasionally
// with incomplete or rolled-back counts (e.g. a preliminary "partial" usage
// followed by the final billed one). A straight overwrite risked dropping
// non-zero values if a later event omitted a field; max() keeps the most
// complete number we've seen so far.
//
// TotalTokens is special: if src supplies a positive total we prefer it (the
// upstream knows the true billed total), otherwise we synthesise it from the
// component fields once merging is done.
func mergeKiroUsage(dst *usage.Detail, src usage.Detail) {
	if dst == nil {
		return
	}
	if src.InputTokens > dst.InputTokens {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > dst.OutputTokens {
		dst.OutputTokens = src.OutputTokens
	}
	if src.ReasoningTokens > dst.ReasoningTokens {
		dst.ReasoningTokens = src.ReasoningTokens
	}
	if src.CachedTokens > dst.CachedTokens {
		dst.CachedTokens = src.CachedTokens
	}
	if src.CacheCreationTokens > dst.CacheCreationTokens {
		dst.CacheCreationTokens = src.CacheCreationTokens
	}
	if src.TotalTokens > dst.TotalTokens {
		dst.TotalTokens = src.TotalTokens
	}
	if dst.TotalTokens == 0 && (dst.InputTokens > 0 || dst.OutputTokens > 0 || dst.ReasoningTokens > 0) {
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

func applyKiroRuntimeIdentityHeaders(req *http.Request, auth *cliproxyauth.Auth) {
	if req == nil {
		return
	}
	if shouldUseKiroIDEIdentity(auth) {
		machineID := kiroMachineIDFromAuth(auth)
		req.Header.Set("User-Agent", kiroIDEUserAgent(machineID))
		req.Header.Set("x-amz-user-agent", kiroIDEAmzUserAgent(machineID))
		return
	}
	req.Header.Set("User-Agent", kiroCLIUserAgent)
	req.Header.Set("x-amz-user-agent", kiroCLIAmzAgent)
}

func shouldUseKiroIDEIdentity(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	authMethod := strings.ToLower(metadataString(auth.Metadata, "auth_method", "authMethod"))
	provider := strings.ToLower(metadataString(auth.Metadata, "provider"))
	if isKiroSocialAuth(authMethod, provider) {
		return true
	}
	if kiroauth.NormalizeKiroMachineID(metadataString(auth.Metadata, "machine_id", "machineId", "device_id", "deviceId")) != "" {
		return !isKiroSSOAuth(authMethod, provider)
	}
	return false
}

func kiroMachineIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return kiroauth.StableKiroMachineID("")
	}
	seed := firstNonEmptyKiroString(
		auth.ID,
		auth.FileName,
		auth.Label,
		metadataString(auth.Metadata, "email"),
		metadataString(auth.Metadata, "profile_arn", "profileArn"),
		metadataString(auth.Metadata, "client_id", "clientId"),
	)
	return kiroauth.MachineIDFromMaps(auth.Metadata, auth.Attributes, seed)
}

func kiroIDEUserAgent(machineID string) string {
	platform, release := kiroDevicePlatform()
	suffix := "KiroIDE-" + kiroIDEVersion
	if machineID = kiroauth.NormalizeKiroMachineID(machineID); machineID != "" {
		suffix += "-" + machineID
	}
	return fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/%s#%s lang/js md/nodejs#22.22.0 api/%s#%s m/E %s", kiroIDEAWSSDKVersion, platform, release, kiroIDEAPIClient, kiroIDEAWSSDKVersion, suffix)
}

func kiroIDEAmzUserAgent(machineID string) string {
	suffix := "KiroIDE-" + kiroIDEVersion
	if machineID = kiroauth.NormalizeKiroMachineID(machineID); machineID != "" {
		suffix = "KiroIDE " + kiroIDEVersion + " " + machineID
	}
	return fmt.Sprintf("aws-sdk-js/%s %s", kiroIDEAWSSDKVersion, suffix)
}

func kiroDevicePlatform() (string, string) {
	switch runtime.GOOS {
	case "windows":
		return "win32", "10.0.19043"
	case "darwin":
		return "macos", "14.0.0"
	default:
		return "linux", "6.0.0"
	}
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
	applyKiroRuntimeIdentityHeaders(httpReq, auth)

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
		// The Kiro social endpoint may return only a fresh access token. In
		// that flow the refresh token is stable, unlike AWS SSO-OIDC rotation.
		refreshTokenResult = refreshToken
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

func kiroRetryAfterFromHeaders(headers http.Header, now time.Time) *time.Duration {
	if headers == nil {
		return nil
	}
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if value == "" {
		return nil
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		d := time.Duration(seconds) * time.Second
		return &d
	}
	if now.IsZero() {
		now = time.Now()
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		d := retryAt.Sub(now)
		return &d
	}
	return nil
}

func logKiroUpstreamHTTPError(auth *cliproxyauth.Auth, endpoint kiroEndpointConfig, resp *http.Response, body []byte) {
	if resp == nil {
		return
	}
	fields := log.Fields{
		"provider": "kiro",
		"status":   resp.StatusCode,
		"endpoint": endpoint.Name,
		"url":      endpoint.URL,
	}
	if auth != nil {
		fields["auth_id"] = auth.ID
		if auth.Label != "" {
			fields["auth_label"] = auth.Label
		}
	}
	if requestID := firstNonEmptyKiroString(
		resp.Header.Get("x-amzn-requestid"),
		resp.Header.Get("x-amz-request-id"),
		resp.Header.Get("x-request-id"),
	); requestID != "" {
		fields["request_id"] = requestID
	}
	if retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")); retryAfter != "" {
		fields["retry_after"] = retryAfter
	}
	if kind, message := kiroErrorSummary(body); kind != "" || message != "" {
		if kind != "" {
			fields["error_type"] = kind
		}
		if message != "" {
			fields["error_message"] = message
		}
	}
	if len(body) > 0 {
		fields["body"] = truncateKiroLogBody(string(body), 2048)
	}
	log.WithFields(fields).Warn("kiro: upstream request failed")
}

func kiroErrorSummary(body []byte) (kind, message string) {
	if len(body) == 0 {
		return "", ""
	}
	root := gjson.ParseBytes(body)
	if root.IsObject() {
		kind = firstNonEmptyKiroString(
			root.Get("__type").String(),
			root.Get("_type").String(),
			root.Get("error.type").String(),
			root.Get("error.code").String(),
			root.Get("errorType").String(),
			root.Get("code").String(),
			root.Get("type").String(),
		)
		message = firstNonEmptyKiroString(
			root.Get("message").String(),
			root.Get("error.message").String(),
			root.Get("error_description").String(),
			root.Get("detail").String(),
		)
		return kind, message
	}
	return "", truncateKiroLogBody(string(body), 512)
}

func truncateKiroLogBody(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
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
