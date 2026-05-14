package executor

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// codexDedupeIgnoredHeaders lists headers that never contribute to the
// non-stream response dedupe key. Each is either request-scoped noise
// (trace identifiers, per-call request ids) or header-only presentation
// metadata that does not affect the upstream body response.
var codexDedupeIgnoredHeaders = map[string]struct{}{
	"Authorization":                         {},
	"X-Codex-Turn-Metadata":                 {},
	"X-Client-Request-Id":                   {},
	"Traceparent":                           {},
	"Tracestate":                            {},
	"X-Responsesapi-Include-Timing-Metrics": {},
}

// codexDedupeRelevantHeaders lists the headers whose values genuinely
// influence the upstream response and therefore must separate dedupe buckets.
var codexDedupeRelevantHeaders = []string{
	"Chatgpt-Account-Id",
	"OpenAI-Beta",
	"Session_id",
	codexHeaderThreadID,
	"X-Codex-Beta-Features",
	"X-Codex-Installation-Id",
	misc.CodexResidencyHeader,
}

// codexPreparedRequest bundles the outgoing *http.Request with the
// post-translation body bytes and the prompt_cache_key that ultimately drives
// dedupe bucketing. The body is pulled out explicitly because the http.Request
// stream would otherwise be consumed before the dedupe-key fingerprint could
// read it.
type codexPreparedRequest struct {
	httpReq       *http.Request
	body          []byte
	promptCacheID string
}

// codexNonStreamHTTPResult is the structured result surfaced to fetch callers.
// body + completedData can be large (megabytes for streamed aggregates);
// errorBody is small and safe to defensively copy under clone().
type codexNonStreamHTTPResult struct {
	statusCode    int
	headers       http.Header
	body          []byte
	completedData []byte
	errorStatus   int
	errorBody     []byte
}

// prepareCodexRequest constructs the outgoing *http.Request using the URL to
// infer request kind. Callers that already know whether the URL maps to
// /responses or /responses/compact should prefer prepareCodexRequestWithKind
// to avoid the classification cost on the hot path.
func (e *CodexExecutor) prepareCodexRequest(ctx context.Context, from sdktranslator.Format, executionSessionID string, url string, req cliproxyexecutor.Request, rawJSON []byte) (codexPreparedRequest, error) {
	return e.prepareCodexRequestWithKind(ctx, from, executionSessionID, url, codexFinalUpstreamRequestKindForURL(url), req, rawJSON)
}

// prepareCodexRequestWithKind builds the upstream *http.Request while reusing a
// pre-classified request kind supplied by the caller. Avoids re-running the
// URL classification when the caller already knows whether the target is the
// /responses or /responses/compact endpoint.
func (e *CodexExecutor) prepareCodexRequestWithKind(ctx context.Context, from sdktranslator.Format, executionSessionID string, url string, requestKind codexFinalUpstreamRequestKind, req cliproxyexecutor.Request, rawJSON []byte) (codexPreparedRequest, error) {
	resolution := e.resolvePromptCacheResolution(ctx, from, executionSessionID, req)
	cache := resolution.cache
	body := rawJSON
	isCompact := requestKind == codexFinalUpstreamCompact
	if cache.ID != "" {
		body = codexSetPromptCacheKey(body, cache.ID)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return codexPreparedRequest{}, err
	}
	if cache.ID != "" && (!isCompact || resolution.headerEligibleID != "") {
		fallbackHeaderValue := cache.ID
		if resolution.headerEligibleID != "" {
			fallbackHeaderValue = resolution.headerEligibleID
		}
		if sessionHeaderValue := codexPromptCacheSessionHeaderValue(ctx, fallbackHeaderValue); sessionHeaderValue != "" {
			httpReq.Header.Set(codexHeaderSessionID, sessionHeaderValue)
		}
		if threadHeaderValue := codexPromptCacheThreadHeaderValue(ctx, req.Payload, fallbackHeaderValue); threadHeaderValue != "" {
			httpReq.Header.Set(codexHeaderThreadID, threadHeaderValue)
		}
	}
	return codexPreparedRequest{
		httpReq:       httpReq,
		body:          body,
		promptCacheID: cache.ID,
	}, nil
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte) (*http.Request, error) {
	prepared, err := e.prepareCodexRequest(ctx, from, "", url, req, rawJSON)
	if err != nil {
		return nil, err
	}
	return prepared.httpReq, nil
}

func (e *CodexExecutor) fetchCodexNonStreamResponse(ctx context.Context, auth *cliproxyauth.Auth, url string, prepared codexPreparedRequest, needResponseHeaders bool) (codexNonStreamHTTPResult, bool, error) {
	key := e.codexResponseDedupeKey(auth, url, prepared)
	result, executed, shared, err := e.responseDedupe.Do(ctx, key, func() (codexNonStreamHTTPResult, error) {
		httpClient := helps.NewCodexHTTPClient(ctx, e.cfg, auth, 0)
		httpResp, errDo := httpClient.Do(prepared.httpReq)
		if errDo != nil {
			return codexNonStreamHTTPResult{}, errDo
		}
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()

		data, errRead := helps.ReadNonStreamResponseBody(httpResp.Body)
		if errRead != nil {
			return codexNonStreamHTTPResult{}, errRead
		}
		return codexNonStreamHTTPResult{
			statusCode: httpResp.StatusCode,
			headers:    httpResp.Header,
			body:       data,
		}, nil
	})
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return codexNonStreamHTTPResult{}, executed, err
	}
	if shared && !executed {
		codexMetrics.dedupeHit.Add(1)
		helps.LogWithRequestID(ctx).Debugf("codex executor: deduped non-stream request for %s", url)
	} else if executed {
		codexMetrics.dedupeMiss.Add(1)
	}

	if shared {
		result = result.clone(needResponseHeaders)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, result.statusCode, result.headers)
	helps.AppendAPIResponseChunk(ctx, e.cfg, result.body)
	return result, executed, nil
}

func (e *CodexExecutor) fetchCodexResponsesAggregate(ctx context.Context, auth *cliproxyauth.Auth, url string, prepared codexPreparedRequest, needResponseHeaders bool) (codexNonStreamHTTPResult, bool, error) {
	key := e.codexResponseDedupeKey(auth, url, prepared)
	captureBody := e.cfg != nil && e.cfg.RequestLog
	result, executed, shared, err := e.responseDedupe.Do(ctx, key, func() (codexNonStreamHTTPResult, error) {
		httpClient := helps.NewCodexHTTPClient(ctx, e.cfg, auth, 0)
		httpResp, errDo := httpClient.Do(prepared.httpReq)
		if errDo != nil {
			return codexNonStreamHTTPResult{}, errDo
		}
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()

		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			data, errRead := helps.ReadErrorResponseBody(httpResp.Body)
			if errRead != nil {
				return codexNonStreamHTTPResult{}, errRead
			}
			return codexNonStreamHTTPResult{
				statusCode: httpResp.StatusCode,
				headers:    httpResp.Header,
				body:       data,
			}, nil
		}

		aggregate, errRead := collectCodexResponseAggregateWithIdleTimeout(httpResp.Body, captureBody, codexResponsesAggregateIdleTimeout)
		if errRead != nil {
			return codexNonStreamHTTPResult{}, errRead
		}
		aggregate.statusCode = httpResp.StatusCode
		aggregate.headers = httpResp.Header
		return aggregate, nil
	})
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return codexNonStreamHTTPResult{}, executed, err
	}
	if shared && !executed {
		codexMetrics.dedupeHit.Add(1)
		helps.LogWithRequestID(ctx).Debugf("codex executor: deduped non-stream request for %s", url)
	} else if executed {
		codexMetrics.dedupeMiss.Add(1)
	}

	if shared {
		result = result.clone(needResponseHeaders)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, result.statusCode, result.headers)
	if len(result.body) > 0 {
		helps.AppendAPIResponseChunk(ctx, e.cfg, result.body)
	}
	return result, executed, nil
}

// codexResponseDedupeKey composes the deterministic key used to bucket
// concurrent in-flight non-stream requests for single-flight aggregation. It
// returns the empty string when the request lacks the fields needed to make
// dedupe safe (no prompt_cache_key, or empty body).
func (e *CodexExecutor) codexResponseDedupeKey(auth *cliproxyauth.Auth, url string, prepared codexPreparedRequest) string {
	if prepared.promptCacheID == "" || len(prepared.body) == 0 {
		return ""
	}
	return strings.Join([]string{
		"codex",
		e.codexResponseDedupeScope(auth),
		http.MethodPost,
		url,
		prepared.promptCacheID,
		shortHashBytes(prepared.body),
		hashCodexDedupeHeaders(prepared.httpReq.Header),
	}, "|")
}

func (e *CodexExecutor) codexResponseDedupeScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return "default"
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return "id:" + id
	}

	parts := make([]string, 0, 3)
	if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
		parts = append(parts, "proxy="+proxyURL)
	}
	if auth.Attributes != nil {
		if baseURL := strings.TrimSpace(auth.Attributes["base_url"]); baseURL != "" {
			parts = append(parts, "base="+baseURL)
		}
		if apiKey := strings.TrimSpace(auth.Attributes["api_key"]); apiKey != "" {
			parts = append(parts, "api="+shortHashString(apiKey))
		}
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, ",")
}

// clone produces a defensive copy suitable for the request-specific caller.
// The large payload fields (body, completedData) are treated as read-only and
// shared across shared callers: downstream translators invoke sjson/gjson which
// never mutate the input buffer in place, so cloning MBs per shared caller is
// pure overhead. errorBody and headers are still defensive-copied because
// errorBody is small and response headers are mutable by design.
func (result codexNonStreamHTTPResult) clone(needResponseHeaders bool) codexNonStreamHTTPResult {
	cloned := codexNonStreamHTTPResult{
		statusCode:  result.statusCode,
		errorStatus: result.errorStatus,
	}
	if needResponseHeaders {
		cloned.headers = result.headers.Clone()
	} else {
		cloned.headers = result.headers
	}
	// body and completedData can be large (up to MBs for streamed aggregates).
	// Callers must not write through these slices; doing so would corrupt the
	// shared memo entry. Go's typical json/gjson/sjson code paths already
	// allocate new buffers on modification, so this assumption is safe here.
	cloned.body = result.body
	cloned.completedData = result.completedData
	if len(result.errorBody) > 0 {
		cloned.errorBody = bytes.Clone(result.errorBody)
	}
	return cloned
}
