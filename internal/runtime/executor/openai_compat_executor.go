package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openAICompatImageHandlerType            = "openai-image"
	openAICompatImagesGenerationsPath       = "/images/generations"
	openAICompatImagesEditsPath             = "/images/edits"
	openAICompatDefaultImageEndpoint        = openAICompatImagesGenerationsPath
	openAICompatMultipartMemory       int64 = 32 << 20
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
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

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImages(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	reporter.CaptureModelReasoningEffort(opts.OriginalRequest, req.Payload)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	plan, err := e.buildNonStreamPlan(req, opts, baseModel)
	if err != nil {
		return resp, err
	}

	reporter.SetTranslatedReasoningEffort(plan.translated, plan.to.String())
	url := strings.TrimSuffix(baseURL, "/") + plan.endpoint
	httpResp, err := e.executeUpstreamBody(ctx, auth, apiKey, url, plan.translated, plan.contentType, false)
	if err != nil {
		return resp, err
	}
	defer closeOpenAICompatResponseBody(httpResp)

	body, err := helps.ReadNonStreamResponseBody(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.EnsurePublished(ctx)
	// Translate response back to source format when needed
	out := body
	if !plan.nativeImages {
		var param any
		out = sdktranslator.TranslateNonStream(ctx, plan.to, plan.from, req.Model, opts.OriginalRequest, plan.translated, body, &param)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *OpenAICompatExecutor) executeImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return resp, err
	}

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	payload, contentType, errPrepare := e.buildNativeImagesPayload(baseModel, requestedModel, requestPath, originalPayloadSource, opts)
	if errPrepare != nil {
		err = errPrepare
		return resp, err
	}

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpResp, errDo := e.executeUpstreamBody(ctx, auth, apiKey, url, payload, contentType, false)
	if errDo != nil {
		err = errDo
		return resp, err
	}
	defer closeOpenAICompatResponseBody(httpResp)

	body, errRead := helps.ReadNonStreamResponseBody(httpResp.Body)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		err = errRead
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	reporter.EnsurePublished(ctx)
	resp = cliproxyexecutor.Response{Payload: body, Headers: httpResp.Header.Clone()}
	return resp, nil
}

type openAICompatNonStreamPlan struct {
	from         sdktranslator.Format
	to           sdktranslator.Format
	endpoint     string
	translated   []byte
	contentType  string
	nativeImages bool
}

func (e *OpenAICompatExecutor) buildNonStreamPlan(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string) (openAICompatNonStreamPlan, error) {
	plan := openAICompatNonStreamPlan{
		from:        opts.SourceFormat,
		to:          sdktranslator.FromString("openai"),
		endpoint:    "/chat/completions",
		contentType: "application/json",
	}
	alt := strings.Trim(strings.TrimSpace(opts.Alt), "/")
	compactResponses := alt == "responses/compact"
	if endpoint, ok := openAICompatNativeImagesEndpoint(alt); ok {
		plan.nativeImages = true
		plan.endpoint = endpoint
	} else if compactResponses {
		plan.to = sdktranslator.FromString("openai-response")
		plan.endpoint = "/responses/compact"
	}

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := openAICompatRequestPath(opts)
	if plan.nativeImages {
		nativePayload, nativeContentType, err := e.buildNativeImagesPayload(baseModel, requestedModel, requestPath, originalPayloadSource, opts)
		if err != nil {
			return plan, err
		}
		plan.translated = nativePayload
		plan.contentType = nativeContentType
		return plan, nil
	}

	originalPayload := originalPayloadSource
	translated, originalTranslated := helps.TranslateRequestWithOriginal(e.cfg, plan.from, plan.to, baseModel, req.Payload, originalPayload, opts.Stream)
	translated = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, plan.to.String(), "", translated, originalTranslated, requestedModel, requestPath)
	if compactResponses {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
	}

	var err error
	plan.translated, err = thinking.ApplyThinking(translated, req.Model, plan.from.String(), plan.to.String(), e.Identifier())
	return plan, err
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImagesStream(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	reporter.CaptureModelReasoningEffort(opts.OriginalRequest, req.Payload)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	alt := strings.Trim(strings.TrimSpace(opts.Alt), "/")
	if endpoint, ok := openAICompatNativeImagesEndpoint(alt); ok {
		originalPayloadSource := req.Payload
		if len(opts.OriginalRequest) > 0 {
			originalPayloadSource = opts.OriginalRequest
		}
		requestedModel := helps.PayloadRequestedModel(opts, req.Model)
		nativePayload, contentType, err := e.buildNativeImagesPayload(baseModel, requestedModel, openAICompatRequestPath(opts), originalPayloadSource, opts)
		if err != nil {
			return nil, err
		}
		url := strings.TrimSuffix(baseURL, "/") + endpoint
		httpResp, err := e.executeUpstreamBody(ctx, auth, apiKey, url, nativePayload, contentType, true)
		if err != nil {
			return nil, err
		}
		out := e.streamOpenAICompatNativeChunks(ctx, reporter, httpResp)
		return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	translated, originalTranslated := helps.TranslateRequestWithOriginal(e.cfg, from, to, baseModel, req.Payload, originalPayload, true)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	translated = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel, openAICompatRequestPath(opts))

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	// Request usage data in the final streaming chunk so that token statistics
	// are captured even when the upstream is an OpenAI-compatible provider.
	translated, _ = sjson.SetBytes(translated, "stream_options.include_usage", true)

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpResp, err := e.executeUpstreamBody(ctx, auth, apiKey, url, translated, "application/json", true)
	if err != nil {
		return nil, err
	}

	out := e.streamOpenAICompatChunks(openAICompatStreamState{
		ctx:        ctx,
		resp:       httpResp,
		reporter:   reporter,
		from:       from,
		to:         to,
		req:        req,
		opts:       opts,
		translated: translated,
	})
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

type openAICompatStreamState struct {
	ctx        context.Context
	resp       *http.Response
	reporter   *helps.UsageReporter
	from       sdktranslator.Format
	to         sdktranslator.Format
	req        cliproxyexecutor.Request
	opts       cliproxyexecutor.Options
	translated []byte
}

func (e *OpenAICompatExecutor) streamOpenAICompatChunks(state openAICompatStreamState) <-chan cliproxyexecutor.StreamChunk {
	out := make(chan cliproxyexecutor.StreamChunk, helps.StreamChunkBufferSize)
	go func() {
		defer close(out)
		defer closeOpenAICompatResponseBody(state.resp)
		var param any
		passthroughOpenAI := state.from == state.to
		errRead := helps.ReadStreamLines(state.resp.Body, func(line []byte) error {
			helps.AppendAPIResponseChunk(state.ctx, e.cfg, line)
			if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
				state.reporter.Publish(state.ctx, detail)
			}
			if len(line) == 0 {
				return nil
			}

			if !bytes.HasPrefix(line, []byte("data:")) {
				return nil
			}
			if passthroughOpenAI {
				payload := bytes.TrimSpace(line[5:])
				if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
					return nil
				}
				out <- cliproxyexecutor.StreamChunk{Payload: payload}
				return nil
			}

			// OpenAI-compatible streams are SSE: lines typically prefixed with "data: ".
			// Pass through translator; it yields one or more chunks for the target schema.
			chunks := sdktranslator.TranslateStream(state.ctx, state.to, state.from, state.req.Model, state.opts.OriginalRequest, state.translated, line, &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
			return nil
		})
		if errRead != nil {
			helps.RecordAPIResponseError(state.ctx, e.cfg, errRead)
			state.reporter.PublishFailureWithError(state.ctx, errRead)
			out <- cliproxyexecutor.StreamChunk{Err: errRead}
		} else if !passthroughOpenAI {
			// In case the upstream close the stream without a terminal [DONE] marker.
			// Feed a synthetic done marker through the translator so pending
			// response.completed events are still emitted exactly once.
			chunks := sdktranslator.TranslateStream(state.ctx, state.to, state.from, state.req.Model, state.opts.OriginalRequest, state.translated, []byte("data: [DONE]"), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
		// Ensure we record the request if no usage chunk was ever seen
		state.reporter.EnsurePublished(state.ctx)
	}()
	return out
}

func (e *OpenAICompatExecutor) streamOpenAICompatNativeChunks(ctx context.Context, reporter *helps.UsageReporter, resp *http.Response) <-chan cliproxyexecutor.StreamChunk {
	out := make(chan cliproxyexecutor.StreamChunk, helps.StreamChunkBufferSize)
	go func() {
		defer close(out)
		defer closeOpenAICompatResponseBody(resp)
		currentEvent := ""
		errRead := helps.ReadStreamLines(resp.Body, func(line []byte) error {
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseOpenAIStreamUsage(line); ok && reporter != nil {
				reporter.Publish(ctx, detail)
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				currentEvent = ""
				return nil
			}
			if bytes.HasPrefix(trimmed, []byte("event:")) {
				currentEvent = strings.TrimSpace(string(bytes.TrimSpace(trimmed[6:])))
				return nil
			}
			if !bytes.HasPrefix(trimmed, []byte("data:")) {
				return nil
			}
			payload := bytes.TrimSpace(trimmed[5:])
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				return nil
			}
			if currentEvent != "" && gjson.ValidBytes(payload) && !gjson.GetBytes(payload, "type").Exists() {
				if updated, err := sjson.SetBytes(payload, "type", currentEvent); err == nil {
					payload = updated
				}
			}
			out <- cliproxyexecutor.StreamChunk{Payload: payload}
			return nil
		})
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			if reporter != nil {
				reporter.PublishFailureWithError(ctx, errRead)
			}
			out <- cliproxyexecutor.StreamChunk{Err: errRead}
		} else if reporter != nil {
			reporter.EnsurePublished(ctx)
		}
	}()
	return out
}

func (e *OpenAICompatExecutor) executeImagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	payload, contentType, errPrepare := prepareOpenAICompatImagesPayload(req.Payload, baseModel, opts.Headers.Get("Content-Type"), true)
	if errPrepare != nil {
		err = errPrepare
		return nil, err
	}
	if contentType == "" {
		contentType = "application/json"
	}

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
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
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, body)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		return nil, statusErr{code: httpResp.StatusCode, msg: string(body)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
			reporter.EnsurePublished(ctx)
		}()
		buffer := make([]byte, 32*1024)
		for {
			n, errRead := httpResp.Body.Read(buffer)
			if n > 0 {
				chunk := bytes.Clone(buffer[:n])
				helps.AppendAPIResponseChunk(ctx, e.cfg, chunk)
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return
				}
			}
			if errRead != nil {
				if errRead != io.EOF {
					helps.RecordAPIResponseError(ctx, e.cfg, errRead)
					reporter.PublishFailure(ctx, errRead)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenAICompatExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelForCounting := baseModel

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := helps.TokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

// Refresh is a no-op for API-key based compatibility providers.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

func openAICompatImageEndpointPath(opts cliproxyexecutor.Options) string {
	if opts.SourceFormat.String() != openAICompatImageHandlerType {
		return ""
	}
	path := helps.PayloadRequestPath(opts)
	if strings.HasSuffix(path, "/images/edits") {
		return openAICompatImagesEditsPath
	}
	if strings.HasSuffix(path, "/images/generations") {
		return openAICompatImagesGenerationsPath
	}
	return openAICompatDefaultImageEndpoint
}

func prepareOpenAICompatImagesPayload(payload []byte, model string, contentType string, stream bool) ([]byte, string, error) {
	model = strings.TrimSpace(model)
	contentType = strings.TrimSpace(contentType)
	if json.Valid(payload) {
		if model != "" {
			payload, _ = sjson.SetBytes(payload, "model", model)
		}
		if stream {
			payload, _ = sjson.SetBytes(payload, "stream", true)
		} else {
			payload, _ = sjson.DeleteBytes(payload, "stream")
		}
		return payload, "application/json", nil
	}

	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return payload, contentType, nil
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return rewriteOpenAICompatImagesMultipartPayload(payload, model, boundary, stream)
}

func cloneOpenAICompatMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func rewriteOpenAICompatImagesMultipartPayload(payload []byte, model string, boundary string, stream bool) ([]byte, string, error) {
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	form, errRead := reader.ReadForm(openAICompatMultipartMemory)
	if errRead != nil {
		return nil, "", fmt.Errorf("read multipart form failed: %w", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("openai compat executor: remove multipart form files error: %v", errRemove)
		}
	}()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if model != "" {
		if errWrite := writer.WriteField("model", model); errWrite != nil {
			return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
		}
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, "", fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
		for _, value := range values {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				return nil, "", fmt.Errorf("write form field %s failed: %w", key, errWrite)
			}
		}
	}
	for key, files := range form.File {
		for _, fileHeader := range files {
			if fileHeader == nil {
				continue
			}
			header := cloneOpenAICompatMIMEHeader(fileHeader.Header)
			header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/octet-stream")
			}
			part, errCreate := writer.CreatePart(header)
			if errCreate != nil {
				return nil, "", fmt.Errorf("create file field %s failed: %w", key, errCreate)
			}
			src, errOpen := fileHeader.Open()
			if errOpen != nil {
				return nil, "", fmt.Errorf("open upload file failed: %w", errOpen)
			}
			_, errCopy := io.Copy(part, src)
			if errClose := src.Close(); errClose != nil {
				log.Errorf("openai compat executor: close upload file error: %v", errClose)
				if errCopy == nil {
					errCopy = errClose
				}
			}
			if errCopy != nil {
				return nil, "", fmt.Errorf("copy upload file failed: %w", errCopy)
			}
		}
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func openAICompatNativeImagesEndpoint(alt string) (string, bool) {
	switch strings.Trim(strings.TrimSpace(alt), "/") {
	case "images/generations":
		return "/images/generations", true
	case "images/edits":
		return "/images/edits", true
	case "images/variations":
		return "/images/variations", true
	default:
		return "", false
	}
}

func (e *OpenAICompatExecutor) buildNativeImagesPayload(baseModel string, requestedModel string, requestPath string, payload []byte, opts cliproxyexecutor.Options) ([]byte, string, error) {
	contentType := openAICompatRequestContentType(opts)
	if isMultipartFormContentType(contentType) {
		rewritten, rewrittenContentType, err := rewriteOpenAICompatMultipartFields(
			payload,
			contentType,
			map[string]string{"model": baseModel},
			map[string]string{"response_format": "b64_json"},
		)
		if err != nil {
			return nil, "", err
		}
		return rewritten, rewrittenContentType, nil
	}

	translated := helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, "openai", "", payload, payload, requestedModel, requestPath)
	translated = setOpenAICompatJSONDefault(translated, "response_format", "b64_json")
	translated = e.overrideModel(translated, baseModel)
	if contentType == "" {
		contentType = "application/json"
	}
	return translated, contentType, nil
}

func openAICompatRequestPath(opts cliproxyexecutor.Options) string {
	return stringMetadataValue(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey)
}

func openAICompatRequestContentType(opts cliproxyexecutor.Options) string {
	if opts.Headers != nil {
		if value := strings.TrimSpace(opts.Headers.Get("Content-Type")); value != "" {
			return value
		}
	}
	return stringMetadataValue(opts.Metadata, cliproxyexecutor.RequestContentTypeMetadataKey)
}

func stringMetadataValue(meta map[string]any, key string) string {
	if len(meta) == 0 || strings.TrimSpace(key) == "" {
		return ""
	}
	switch v := meta[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func isMultipartFormContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	return err == nil && strings.EqualFold(mediaType, "multipart/form-data")
}

func setOpenAICompatJSONDefault(payload []byte, path string, value any) []byte {
	if len(payload) == 0 || strings.TrimSpace(path) == "" || gjson.GetBytes(payload, path).Exists() {
		return payload
	}
	updated, err := sjson.SetBytes(payload, path, value)
	if err != nil {
		return payload
	}
	return updated
}

func rewriteOpenAICompatMultipartFields(body []byte, contentType string, overrides map[string]string, defaults map[string]string) ([]byte, string, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, "", fmt.Errorf("parse multipart content-type: %w", err)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is required")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	seen := make(map[string]struct{}, len(overrides)+len(defaults))

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read multipart body: %w", err)
		}

		formName := strings.TrimSpace(part.FormName())
		partHeader := cloneOpenAICompatMultipartHeader(part.Header)
		target, err := writer.CreatePart(partHeader)
		if err != nil {
			_ = part.Close()
			return nil, "", fmt.Errorf("create multipart part: %w", err)
		}

		if part.FileName() == "" {
			if value, ok := overrides[formName]; ok {
				if _, err := target.Write([]byte(value)); err != nil {
					_ = part.Close()
					return nil, "", fmt.Errorf("rewrite multipart field %s: %w", formName, err)
				}
				seen[formName] = struct{}{}
				_ = part.Close()
				continue
			}
			if _, ok := defaults[formName]; ok {
				seen[formName] = struct{}{}
			}
		}

		if _, err := io.Copy(target, part); err != nil {
			_ = part.Close()
			return nil, "", fmt.Errorf("copy multipart part: %w", err)
		}
		_ = part.Close()
	}

	for field, value := range overrides {
		if _, ok := seen[field]; ok {
			continue
		}
		if err := writer.WriteField(field, value); err != nil {
			return nil, "", fmt.Errorf("append multipart field %s: %w", field, err)
		}
		seen[field] = struct{}{}
	}
	for field, value := range defaults {
		if _, ok := seen[field]; ok {
			continue
		}
		if err := writer.WriteField(field, value); err != nil {
			return nil, "", fmt.Errorf("append multipart field %s: %w", field, err)
		}
		seen[field] = struct{}{}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finalize multipart body: %w", err)
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

func cloneOpenAICompatMultipartHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		copied := make([]string, len(values))
		copy(copied, values)
		dst[key] = copied
	}
	return dst
}

func (e *OpenAICompatExecutor) executeUpstreamBody(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, url string, payload []byte, contentType string, stream bool) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	e.prepareUpstreamRequest(httpReq, auth, apiKey, contentType, stream)
	authID, authLabel, authType, authValue := openAICompatAuthInfo(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header,
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
	helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
	status := httpResp.StatusCode
	closeOpenAICompatResponseBody(httpResp)
	return nil, statusErr{code: status, msg: string(b)}
}

func closeOpenAICompatResponseBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	if errClose := resp.Body.Close(); errClose != nil {
		log.Errorf("openai compat executor: close response body error: %v", errClose)
	}
}

func (e *OpenAICompatExecutor) prepareUpstreamRequest(req *http.Request, auth *cliproxyauth.Auth, apiKey string, contentType string, stream bool) {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
	}
}

func openAICompatAuthInfo(auth *cliproxyauth.Auth) (id, label, accountType, accountValue string) {
	if auth == nil {
		return "", "", "", ""
	}
	accountType, accountValue = auth.AccountInfo()
	return auth.ID, auth.Label, accountType, accountValue
}

func (e *OpenAICompatExecutor) overrideModel(payload []byte, model string) []byte {
	if len(payload) == 0 || model == "" {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "model", model)
	if err != nil {
		return payload
	}
	return updated
}

type statusErr struct {
	code       int
	msg        string
	retryAfter *time.Duration
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }
