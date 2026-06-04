package executor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexDefaultImageToolModel = "gpt-image-2"

var errCodexStopStream = errors.New("codex executor: stop stream after terminal event")

type codexUnauthorizedRetryContextKey struct{}

func codexStreamClosedBeforeCompletedErr() error {
	return statusErr{code: http.StatusRequestTimeout, msg: "stream error: stream disconnected before completion: stream closed before response.completed"}
}

func codexShouldRetryStreamRead(ctx context.Context, err error, emittedPayload bool, completedStreamObserved bool, pendingTerminalErr error, terminalFailure bool, attempt int) bool {
	if err == nil || emittedPayload || completedStreamObserved || pendingTerminalErr != nil || terminalFailure {
		return false
	}
	if attempt >= codexHTTPMaxStreamReadRetries {
		return false
	}
	return codexShouldRetryHTTPTransportError(ctx, err)
}

func codexUnauthorizedRetryAlreadyUsed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	used, _ := ctx.Value(codexUnauthorizedRetryContextKey{}).(bool)
	return used
}

func contextWithCodexUnauthorizedRetryUsed(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexUnauthorizedRetryContextKey{}, true)
}

// CodexExecutor executes Codex requests and reuses per-proxy auth services for refresh flows.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg            *config.Config
	codexAuthCache sync.Map
	httpTurnState  *codexHTTPTurnStateStore
	responseDedupe helps.InFlightGroup[codexNonStreamHTTPResult]
	// refreshDedupe serialises concurrent token refreshes per auth.ID. Without
	// it multiple in-flight requests sharing an expired access_token would each
	// call RefreshTokensWithRetry with the same refresh_token; OpenAI treats
	// that as refresh_token reuse and invalidates the credential.
	refreshDedupe helps.InFlightGroup[*codexauth.CodexTokenData]
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor {
	return &CodexExecutor{cfg: cfg, httpTurnState: newCodexHTTPTurnStateStore()}
}

func (e *CodexExecutor) Identifier() string { return "codex" }

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = contextWithCodexForcedUpstreamSessionFromOptions(ctx, opts)
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImage(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	needResponseHeaders := needResponseHeadersFromOptions(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	reporter.CaptureModelReasoningEffort(opts.OriginalRequest, req.Payload)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	body, originalTranslated := helps.TranslateRequestWithOriginal(e.cfg, from, to, baseModel, req.Payload, originalPayload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth)
	}
	body, replayScope := applyCodexReasoningReplayCache(ctx, from, req, opts, body)
	reporter.SetTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	preparedBody := body
	call, err := e.prepareCodexHTTPCall(ctx, auth, from, executionSessionIDFromOptions(opts), url, req, preparedBody, apiKey, true)
	if err != nil {
		return resp, err
	}
	body = call.prepared.body
	helps.RecordAPIRequest(ctx, e.cfg, call.requestLog)
	result, usageOwner, err := e.fetchCodexResponsesAggregate(ctx, auth, call.url, call.prepared, needResponseHeaders)
	if err != nil {
		return resp, err
	}
	if result.statusCode == http.StatusUnauthorized {
		refreshedAuth, retried, refreshErr := e.refreshCodexAuthAfterUnauthorized(ctx, auth)
		if refreshErr != nil {
			return resp, refreshErr
		}
		if retried {
			auth = refreshedAuth
			apiKey, _ = codexCreds(auth)
			call, err = e.prepareCodexHTTPCall(ctx, auth, from, executionSessionIDFromOptions(opts), url, req, preparedBody, apiKey, true)
			if err != nil {
				return resp, err
			}
			helps.RecordAPIRequest(ctx, e.cfg, call.requestLog)
			result, usageOwner, err = e.fetchCodexResponsesAggregate(ctx, auth, call.url, call.prepared, needResponseHeaders)
			if err != nil {
				return resp, err
			}
		}
	}
	if result.statusCode < 200 || result.statusCode >= 300 {
		clearCodexReasoningReplayOnInvalidSignature(replayScope, result.statusCode, result.body)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", result.statusCode, helps.SummarizeErrorBody(result.headers.Get("Content-Type"), result.body))
		err = newCodexStatusErr(result.statusCode, result.body)
		return resp, err
	}
	if len(result.completedData) > 0 {
		cacheCodexReasoningReplayFromCompleted(replayScope, result.completedData)
		if usageOwner {
			if detail, ok := helps.ParseCodexUsage(result.completedData); ok {
				reporter.Publish(ctx, detail)
			}
			reporter.EnsurePublished(ctx)
		}

		var param any
		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, result.completedData, &param)
		resp = cliproxyexecutor.Response{Payload: out}
		if needResponseHeaders {
			resp.Headers = result.headers
		}
		return resp, nil
	}
	if len(result.errorBody) > 0 {
		clearCodexReasoningReplayOnInvalidSignature(replayScope, result.errorStatus, result.errorBody)
		err = newCodexStatusErr(result.errorStatus, result.errorBody)
		return resp, err
	}
	err = codexStreamClosedBeforeCompletedErr()
	return resp, err
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	needResponseHeaders := needResponseHeadersFromOptions(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	reporter.CaptureModelReasoningEffort(opts.OriginalRequest, req.Payload)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	body, originalTranslated := helps.TranslateRequestWithOriginal(e.cfg, from, to, baseModel, req.Payload, originalPayload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth)
	}
	body, replayScope := applyCodexReasoningReplayCache(ctx, from, req, opts, body)
	reporter.SetTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	preparedBody := body
	call, err := e.prepareCodexHTTPCall(ctx, auth, from, executionSessionIDFromOptions(opts), url, req, preparedBody, apiKey, false)
	if err != nil {
		return resp, err
	}
	body = call.prepared.body
	helps.RecordAPIRequest(ctx, e.cfg, call.requestLog)
	result, usageOwner, err := e.fetchCodexNonStreamResponse(ctx, auth, call.url, call.prepared, needResponseHeaders)
	if err != nil {
		return resp, err
	}
	if result.statusCode == http.StatusUnauthorized {
		refreshedAuth, retried, refreshErr := e.refreshCodexAuthAfterUnauthorized(ctx, auth)
		if refreshErr != nil {
			return resp, refreshErr
		}
		if retried {
			auth = refreshedAuth
			apiKey, _ = codexCreds(auth)
			call, err = e.prepareCodexHTTPCall(ctx, auth, from, executionSessionIDFromOptions(opts), url, req, preparedBody, apiKey, false)
			if err != nil {
				return resp, err
			}
			helps.RecordAPIRequest(ctx, e.cfg, call.requestLog)
			result, usageOwner, err = e.fetchCodexNonStreamResponse(ctx, auth, call.url, call.prepared, needResponseHeaders)
			if err != nil {
				return resp, err
			}
		}
	}
	if result.statusCode < 200 || result.statusCode >= 300 {
		clearCodexReasoningReplayOnInvalidSignature(replayScope, result.statusCode, result.body)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", result.statusCode, helps.SummarizeErrorBody(result.headers.Get("Content-Type"), result.body))
		err = newCodexStatusErr(result.statusCode, result.body)
		return resp, err
	}
	data := result.body
	if usageOwner {
		reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
		reporter.EnsurePublished(ctx)
		codexAdvanceWindowGeneration(codexWindowStateKey(call.prepared.httpReq.Header))
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: out}
	if needResponseHeaders {
		resp.Headers = result.headers
	}
	return resp, nil
}

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = contextWithCodexForcedUpstreamSessionFromOptions(ctx, opts)
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImageStream(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	upstreamCtx, releaseUpstreamCtx := codexDetachUpstreamContext(ctx, e.cfg)
	upstreamCtx = contextWithCodexForcedUpstreamSessionFromOptions(upstreamCtx, opts)
	releaseUpstreamCtxOnReturn := true
	defer func() {
		if releaseUpstreamCtxOnReturn {
			releaseUpstreamCtx()
		}
	}()
	needResponseHeaders := needResponseHeadersFromOptions(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(upstreamCtx, e, baseModel, auth)
	reporter.CaptureModelReasoningEffort(opts.OriginalRequest, req.Payload)
	defer func() {
		if !codexRequestContextDone(ctx, err) {
			reporter.TrackFailure(upstreamCtx, &err)
		}
	}()

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	body, originalTranslated := helps.TranslateRequestWithOriginal(e.cfg, from, to, baseModel, req.Payload, originalPayload, true)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth)
	}
	body, replayScope := applyCodexReasoningReplayCache(upstreamCtx, from, req, opts, body)
	reporter.SetTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	preparedBody := body
	call, err := e.prepareCodexHTTPCall(upstreamCtx, auth, from, executionSessionIDFromOptions(opts), url, req, preparedBody, apiKey, true)
	if err != nil {
		return nil, err
	}
	body = call.prepared.body
	helps.RecordAPIRequest(ctx, e.cfg, call.requestLog)

	httpResp, err := e.doCodexHTTPRequest(upstreamCtx, auth, call.prepared)
	if err != nil {
		codexRecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header)
	e.rememberCodexHTTPTurnState(auth, call.prepared, httpResp.Header)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := helps.ReadErrorResponseBody(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			codexRecordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		if httpResp.StatusCode == http.StatusUnauthorized {
			refreshedAuth, retried, refreshErr := e.refreshCodexAuthAfterUnauthorized(upstreamCtx, auth)
			if refreshErr != nil {
				codexRecordAPIResponseError(ctx, e.cfg, refreshErr)
				return nil, refreshErr
			}
			if retried {
				auth = refreshedAuth
				apiKey, _ = codexCreds(auth)
				call, err = e.prepareCodexHTTPCall(upstreamCtx, auth, from, executionSessionIDFromOptions(opts), url, req, preparedBody, apiKey, true)
				if err != nil {
					return nil, err
				}
				body = call.prepared.body
				helps.RecordAPIRequest(ctx, e.cfg, call.requestLog)
				httpResp, err = e.doCodexHTTPRequest(upstreamCtx, auth, call.prepared)
				if err != nil {
					codexRecordAPIResponseError(ctx, e.cfg, err)
					return nil, err
				}
				helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header)
				e.rememberCodexHTTPTurnState(auth, call.prepared, httpResp.Header)
				if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
					goto codexStreamResponseOK
				}
				data, readErr = helps.ReadErrorResponseBody(httpResp.Body)
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close response body error: %v", errClose)
				}
				if readErr != nil {
					codexRecordAPIResponseError(ctx, e.cfg, readErr)
					return nil, readErr
				}
			}
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		clearCodexReasoningReplayOnInvalidSignature(replayScope, httpResp.StatusCode, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return nil, err
	}

codexStreamResponseOK:
	out := make(chan cliproxyexecutor.StreamChunk, helps.StreamChunkBufferSize)
	releaseUpstreamCtxOnReturn = false
	go func() {
		defer releaseUpstreamCtx()
		defer close(out)
		downstreamClosed := false
		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			if downstreamClosed {
				return false
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				downstreamClosed = true
				return false
			}
		}
		turnStateRetryUsed := false
		for streamAttempt := 0; ; streamAttempt++ {
			if streamAttempt > 0 {
				retryResp, retryErr := e.doCodexHTTPRequest(upstreamCtx, auth, call.prepared)
				if retryErr != nil {
					if codexRequestContextDone(ctx, retryErr) {
						return
					}
					codexRecordAPIResponseError(ctx, e.cfg, retryErr)
					reporter.PublishFailureWithError(upstreamCtx, retryErr)
					_ = send(cliproxyexecutor.StreamChunk{Err: retryErr})
					reporter.EnsurePublished(upstreamCtx)
					return
				}
				httpResp = retryResp
				helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header)
				e.rememberCodexHTTPTurnState(auth, call.prepared, httpResp.Header)
				if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
					data, readErr := helps.ReadErrorResponseBody(httpResp.Body)
					if errClose := httpResp.Body.Close(); errClose != nil {
						log.Errorf("codex executor: close response body error: %v", errClose)
					}
					if readErr != nil {
						if codexRequestContextDone(ctx, readErr) {
							return
						}
						codexRecordAPIResponseError(ctx, e.cfg, readErr)
						reporter.PublishFailureWithError(upstreamCtx, readErr)
						_ = send(cliproxyexecutor.StreamChunk{Err: readErr})
						reporter.EnsurePublished(upstreamCtx)
						return
					}
					helps.AppendAPIResponseChunk(ctx, e.cfg, data)
					clearCodexReasoningReplayOnInvalidSignature(replayScope, httpResp.StatusCode, data)
					statusErr := newCodexStatusErr(httpResp.StatusCode, data)
					codexRecordAPIResponseError(ctx, e.cfg, statusErr)
					reporter.PublishFailureWithError(upstreamCtx, statusErr)
					_ = send(cliproxyexecutor.StreamChunk{Err: statusErr})
					reporter.EnsurePublished(upstreamCtx)
					return
				}
			}

			idleReader := newIdleTimeoutReadCloser(httpResp.Body, codexResponsesStreamIdleTimeout)
			streamBody := idleReader
			var param any
			streamState := newCodexStreamCompletionState()
			terminalFailure := false
			var terminalFailureErr error
			emittedPayload := false
			completedStreamObserved := false
			// pendingTerminalErr captures the terminal upstream error observed
			// mid-stream (e.g. usage_limit_reached) so the downstream client is
			// informed even when we had to stop reading after already sending
			// partial payload chunks. Without this the client would treat a
			// partially-delivered response as a successful completion.
			var pendingTerminalErr error
			errRead := helps.ReadStreamLines(streamBody, func(line []byte) error {
				if err := upstreamCtx.Err(); err != nil {
					return err
				}
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				stopAfterForward := false
				if eventData, ok := codexEventData(line); ok {
					eventType := codexEventType(eventData)
					if codexShouldSuppressUsageWarningEvent(eventType, eventData) {
						return nil
					}
					if terminalErr, ok := parseCodexStreamTerminalError(eventType, eventData); ok {
						log.Warnf("codex stream terminated with %s: %s", eventType, terminalErr.Error())
						if eventType == "response.failed" {
							clearCodexReasoningReplayOnInvalidSignature(replayScope, terminalErr.StatusCode(), normalizeCodexResponseFailedErrorBody(eventData))
						}
						terminalFailure = true
						terminalFailureErr = terminalErr
						if !emittedPayload {
							return terminalErr
						}
						pendingTerminalErr = terminalErr
						return errCodexStopStream
					}
					switch eventType {
					case "response.incomplete":
						// Mirror codex-rs: treat response.incomplete as a terminal
						// failure. Forward the event once, then stop reading instead
						// of waiting for the upstream connection to close.
						reason := codexResponseIncompleteReason(eventData)
						log.Warnf("codex stream terminated with response.incomplete: reason=%s", reason)
						terminalFailure = true
						terminalFailureErr = codexResponseIncompleteEventErr(eventData)
						pendingTerminalErr = terminalFailureErr
						stopAfterForward = true
					case "response.failed":
						message := gjson.GetBytes(eventData, "response.error.message").String()
						if message == "" {
							message = "response.failed"
						}
						log.Warnf("codex stream terminated with response.failed: %s", message)
						terminalFailure = true
						terminalFailureErr = errors.New(message)
					}
					if completed, isCompleted := streamState.processEventDataWithType(eventType, eventData, true); isCompleted {
						completedStreamObserved = true
						stopAfterForward = true
						if detail, ok := helps.ParseCodexUsage(completed.data); ok {
							reporter.Publish(upstreamCtx, detail)
						}
						cacheCodexReasoningReplayFromCompleted(replayScope, completed.data)
						if completed.recoveredCount > 0 {
							log.Warnf(
								"codex stream completed with empty response.output; recovered_items=%d cached_done_items=%d cached_function_calls=%d",
								completed.recoveredCount,
								len(streamState.outputItemsByIndex)+len(streamState.outputItemsFallback),
								len(streamState.functionCallsByItem),
							)
							line = codexSSEDataLine(completed.data)
						}
					}
				}

				if !downstreamClosed {
					chunks := sdktranslator.TranslateStream(upstreamCtx, to, from, req.Model, originalPayload, body, line, &param)
					for i := range chunks {
						if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
							break
						}
						if len(chunks[i]) > 0 {
							emittedPayload = true
						}
					}
				}
				if stopAfterForward {
					return errCodexStopStream
				}
				return nil
			})
			if errRead != nil {
				if errors.Is(errRead, errCodexStopStream) {
					errRead = nil
				}
			}
			if idleReader.TimedOut() && !completedStreamObserved && pendingTerminalErr == nil {
				errRead = codexStreamIdleTimeoutErr()
			} else if errRead != nil && idleReader.TimedOut() {
				errRead = codexStreamIdleTimeoutErr()
			}
			idleReader.StopTimer()
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
			if !turnStateRetryUsed && !emittedPayload && !completedStreamObserved && pendingTerminalErr == nil && codexShouldRetryHTTPWithoutTurnState(call.prepared, codexErrorBodyForTurnStateRetry(errRead)) {
				turnStateRetryUsed = true
				statusCode := statusCodeFromCodexError(errRead)
				e.dropCodexHTTPTurnStateForRetry(upstreamCtx, auth, call.prepared, "stream terminal error", statusCode)
				continue
			}
			if codexShouldRetryStreamRead(ctx, errRead, emittedPayload, completedStreamObserved, pendingTerminalErr, terminalFailure, streamAttempt) {
				helps.LogWithRequestID(ctx).Debugf("codex executor: retrying stream after transport read error (attempt=%d/%d): %v", streamAttempt+1, codexHTTPMaxStreamReadRetries, errRead)
				if errSleep := codexSleepBeforeHTTPRetry(upstreamCtx, streamAttempt+1); errSleep != nil {
					if codexRequestContextDone(ctx, errSleep) {
						return
					}
					codexRecordAPIResponseError(ctx, e.cfg, errSleep)
					reporter.PublishFailureWithError(upstreamCtx, errSleep)
					_ = send(cliproxyexecutor.StreamChunk{Err: errSleep})
					reporter.EnsurePublished(upstreamCtx)
					return
				}
				continue
			}
			if errRead != nil {
				if codexRequestContextDone(ctx, errRead) {
					return
				}
				codexRecordAPIResponseError(ctx, e.cfg, errRead)
				reporter.PublishFailureWithError(upstreamCtx, errRead)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
			} else if pendingTerminalErr != nil {
				// The stream ended gracefully after we detected a terminal upstream
				// event post-partial-payload. Surface the error to the downstream
				// client so it can render a failure instead of treating the partial
				// payload as a complete response.
				codexRecordAPIResponseError(ctx, e.cfg, pendingTerminalErr)
				reporter.PublishFailureWithError(upstreamCtx, pendingTerminalErr)
				_ = send(cliproxyexecutor.StreamChunk{Err: pendingTerminalErr})
			} else if !completedStreamObserved && !terminalFailure {
				closedErr := codexStreamClosedBeforeCompletedErr()
				codexRecordAPIResponseError(ctx, e.cfg, closedErr)
				reporter.PublishFailureWithError(upstreamCtx, closedErr)
				_ = send(cliproxyexecutor.StreamChunk{Err: closedErr})
			} else if terminalFailure {
				reporter.PublishFailureWithError(upstreamCtx, terminalFailureErr)
			}
			reporter.EnsurePublished(upstreamCtx)
			return
		}
	}()
	var headers http.Header
	if needResponseHeaders {
		headers = httpResp.Header.Clone()
	}
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	refreshToken := metadataString(auth.Metadata, "refresh_token", "refreshToken")
	if refreshToken == "" {
		return auth, nil
	}
	svc := e.codexAuthService(auth)
	// Deduplicate concurrent refreshes for the same auth. OpenAI returns
	// refresh_token_reused once a refresh_token has been exchanged, which
	// invalidates the entire credential — a race between two requests racing
	// through Refresh would otherwise kill the auth.
	flightKey := "codex-refresh|" + strings.TrimSpace(auth.ID)
	if flightKey == "codex-refresh|" {
		// Fall back to payload-derived key when the auth carries no ID so
		// per-credential deduplication still applies.
		flightKey = "codex-refresh|" + refreshToken
	}
	td, _, _, err := e.refreshDedupe.Do(ctx, flightKey, func() (*codexauth.CodexTokenData, error) {
		return svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	})
	if err != nil {
		return nil, err
	}
	if td == nil {
		return nil, statusErr{code: 500, msg: "codex executor: refresh returned nil token data"}
	}
	applyCodexTokenDataToAuth(auth, td, time.Now().UTC())
	return auth, nil
}

func applyCodexTokenDataToAuth(auth *cliproxyauth.Auth, td *codexauth.CodexTokenData, now time.Time) {
	if auth == nil || td == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	oldAccessToken := metadataString(auth.Metadata, "access_token", "accessToken")
	if td.IDToken != "" {
		auth.Metadata["id_token"] = td.IDToken
	}
	if td.AccessToken != "" {
		auth.Metadata["access_token"] = td.AccessToken
		if auth.Attributes != nil && oldAccessToken != "" && strings.TrimSpace(auth.Attributes["api_key"]) == oldAccessToken {
			auth.Attributes["api_key"] = td.AccessToken
		}
	}
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	if td.Email != "" {
		auth.Metadata["email"] = td.Email
	}
	if td.PlanType != "" {
		auth.Metadata["plan_type"] = td.PlanType
		auth.Metadata["chatgpt_plan_type"] = td.PlanType
		if auth.Attributes == nil {
			auth.Attributes = map[string]string{}
		}
		auth.Attributes["plan_type"] = td.PlanType
	}
	if td.Expire != "" {
		auth.Metadata["expired"] = td.Expire
	}
	auth.Metadata["type"] = "codex"
	auth.Metadata["last_refresh"] = now.UTC().Format(time.RFC3339)
}

func (e *CodexExecutor) refreshCodexAuthAfterUnauthorized(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, bool, error) {
	if auth == nil || codexIsAPIKeyAuth(auth) {
		return auth, false, nil
	}
	if metadataString(auth.Metadata, "refresh_token", "refreshToken") == "" {
		return auth, false, nil
	}
	coord := cliproxyauth.RefreshCoordinatorFrom(ctx)
	if coord == nil {
		return auth, false, nil
	}
	previousToken, _ := codexCreds(auth)
	refreshed, err := coord(ctx, auth)
	if err != nil {
		return nil, false, err
	}
	if refreshed == nil {
		return auth, false, nil
	}
	nextToken, _ := codexCreds(refreshed)
	if strings.TrimSpace(nextToken) == "" || strings.TrimSpace(nextToken) == strings.TrimSpace(previousToken) {
		return refreshed, false, nil
	}
	log.Debugf("codex executor: retrying request after coordinated auth refresh")
	return refreshed, true, nil
}

func (e *CodexExecutor) codexAuthService(auth *cliproxyauth.Auth) *codexauth.CodexAuth {
	proxyURL := e.codexAuthProxyURL(auth)
	// Composite key: proxyURL plus a fingerprint of env-driven CA configuration.
	// If CODEX_CA_CERTIFICATE or SSL_CERT_FILE changes at runtime, cached
	// transports would otherwise silently keep the stale root pool. The env
	// is read lazily so the overhead is a single hash of a usually-empty value.
	cacheKey := proxyURL + "\x00" + os.Getenv("CODEX_CA_CERTIFICATE") + "\x00" + os.Getenv("SSL_CERT_FILE")
	if cached, ok := e.codexAuthCache.Load(cacheKey); ok {
		if svc, okSvc := cached.(*codexauth.CodexAuth); okSvc {
			return svc
		}
	}

	svc := codexauth.NewCodexAuthWithProxyURL(e.cfg, proxyURL)
	actual, _ := e.codexAuthCache.LoadOrStore(cacheKey, svc)
	if cached, ok := actual.(*codexauth.CodexAuth); ok {
		return cached
	}
	return svc
}

// ResetCodexAuthCache clears the internal CodexAuth cache. Call this when the
// runtime configuration (proxy settings, SDK headers) is hot-reloaded so the
// next request reconstructs transports against the new configuration. It is
// safe to call concurrently with in-flight requests; the old transports remain
// valid until their referenced goroutines complete.
func (e *CodexExecutor) ResetCodexAuthCache() {
	if e == nil {
		return
	}
	// sync.Map has no Clear() pre-Go 1.23; range+delete preserves zero-value
	// semantics for readers racing against the reset.
	e.codexAuthCache.Range(func(k, _ any) bool {
		e.codexAuthCache.Delete(k)
		return true
	})
}

func (e *CodexExecutor) codexAuthProxyURL(auth *cliproxyauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if e.cfg == nil {
		return ""
	}
	return strings.TrimSpace(e.cfg.ProxyURL)
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if !codexIsAPIKeyAuth(a) {
		if accessToken := metadataString(a.Metadata, "access_token", "accessToken"); accessToken != "" {
			apiKey = accessToken
		}
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func normalizeCodexInstructions(body []byte) []byte {
	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() || instructions.Type == gjson.Null {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	return body
}

var imageGenToolJSON = []byte(`{"type":"image_generation","output_format":"png"}`)
var imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func ensureImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth) []byte {
	if strings.HasSuffix(baseModel, "spark") {
		return body
	}
	if codexIsAPIKeyAuth(auth) {
		return body
	}
	if isCodexFreePlanAuth(auth) {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", imageGenToolArrayJSON)
		return body
	}
	for _, t := range tools.Array() {
		if t.Get("type").String() == "image_generation" {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tools.-1", imageGenToolJSON)
	return body
}

func publishCodexImageToolUsage(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte) {
	detail, ok := helps.ParseCodexImageToolUsage(completedData)
	if !ok {
		return
	}
	reporter.EnsurePublished(ctx)
	reporter.PublishAdditionalModel(ctx, codexImageGenerationToolModel(body), detail)
}

func codexImageGenerationToolModel(body []byte) string {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "image_generation" {
				continue
			}
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				return model
			}
			break
		}
	}
	return codexDefaultImageToolModel
}

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range e.cfg.CodexKey {
		entry := &e.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
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
		for i := range e.cfg.CodexKey {
			entry := &e.cfg.CodexKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}
