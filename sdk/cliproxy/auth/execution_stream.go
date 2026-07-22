package auth

import (
	"context"
	"net/http"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return mergeHTTPHeaders(e.headers, headersFromError(e.cause))
}

func (e *streamBootstrapError) StatusCode() int {
	if e == nil || e.cause == nil {
		return 0
	}
	return statusCodeFromError(e.cause)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func mergeHTTPHeaders(primary, fallback http.Header) http.Header {
	merged := cloneHTTPHeader(primary)
	if len(fallback) == 0 {
		return merged
	}
	if merged == nil {
		return cloneHTTPHeader(fallback)
	}
	for key, values := range fallback {
		if _, exists := merged[key]; exists {
			continue
		}
		merged[key] = append([]string(nil), values...)
	}
	return merged
}

func invalidStreamResultError(message string) *Error {
	return &Error{Code: "invalid_stream_result", Message: message, Retryable: true, HTTPStatus: http.StatusBadGateway}
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case chunk, ok = <-ch:
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, release func()) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk, cliproxyexecutor.StreamChunkBufferSize)
	go func() {
		defer close(out)
		defer func() {
			if release != nil {
				release()
			}
		}()
		var failed bool
		responseID := ""
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if len(chunk.Payload) > 0 {
				if id := responseIDFromProviderPayload(chunk.Payload); id != "" {
					responseID = id
				}
			}
			if chunk.Err != nil && !failed {
				failed = true
				streamResult := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false}
				applyResultError(&streamResult, chunk.Err)
				m.MarkResult(ctx, streamResult)
			}
			if !forward {
				return false
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if !failed {
			m.MarkResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: true})
			m.bindPreviousResponseID(ctx, responseID, auth.ID)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel string, execModels []string, pooled bool) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	ctx = WithRefreshUpdateCallback(ctx, m.handleExecutionRefreshUpdate)
	ctx = WithAuthUpdateCallback(ctx, m.handleExecutionAuthUpdate)
	ctx = WithRateLimitUpdateCallback(ctx, m.handleExecutionRateLimitUpdate)
	ctx = WithRefreshCoordinator(ctx, m.coordinatedRefreshForRequest)
	var lastErr error
	poolModeRetries := m.apiKeyPoolModeRetries(auth)
	transportRetries := m.requestRetryLimitForAuth(auth)
	for idx, execModel := range execModels {
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		execReq = m.withOAuthModelAliasReasoningEffort(execReq, auth, routeModel, opts)
		for retryAttempt := 0; ; retryAttempt++ {
			releaseAdmission, errAdmission := m.admitAuthExecution(auth, resultModel, retryAttempt > 0)
			if errAdmission != nil {
				return nil, errAdmission
			}
			lease := m.beginAuthInFlight(auth.ID)
			streamResult, errStream := func() (*cliproxyexecutor.StreamResult, error) {
				defer releaseAdmission()
				defer func() {
					if r := recover(); r != nil {
						lease.Close()
						panic(r)
					}
				}()
				return executor.ExecuteStream(ctx, auth, execReq, opts)
			}()
			if errStream != nil {
				lease.Close()
				if errCtx := ctx.Err(); errCtx != nil {
					return nil, errCtx
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false}
				applyResultError(&result, errStream)
				if shouldRetryTransportErrorWithSameAuth(result.Error, retryAttempt, transportRetries) {
					logSameAuthTransportRetry(ctx, auth, provider, resultModel, retryAttempt+1, transportRetries, errStream)
					continue
				}
				result.RetryAfter = retryAfterFromError(errStream)
				m.MarkResult(ctx, result)
				clearSelectedAuthMetadataForCredentialFailover(provider, opts.Metadata, auth.ID, errStream)
				lastErr = errStream
				switch poolModeRetryDecisionForError(errStream, retryAttempt, poolModeRetries) {
				case poolModeRetryInvalidRequest:
					return nil, errStream
				case poolModeRetryRetry:
					continue
				}
				if result.AuthScoped || isAuthWideResultError(result.Error) {
					return nil, errStream
				}
				break
			}
			if streamResult == nil {
				lease.Close()
				invalidErr := invalidStreamResultError("upstream executor returned nil stream result")
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: invalidErr}
				m.MarkResult(ctx, result)
				lastErr = invalidErr
				if poolModeRetryDecisionForError(invalidErr, retryAttempt, poolModeRetries) == poolModeRetryRetry {
					continue
				}
				if idx < len(execModels)-1 {
					break
				}
				return nil, newStreamBootstrapError(invalidErr, nil)
			}
			if streamResult.Chunks == nil {
				lease.Close()
				invalidErr := invalidStreamResultError("upstream executor returned stream result without chunks")
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: invalidErr}
				m.MarkResult(ctx, result)
				lastErr = invalidErr
				if poolModeRetryDecisionForError(invalidErr, retryAttempt, poolModeRetries) == poolModeRetryRetry {
					continue
				}
				if idx < len(execModels)-1 {
					break
				}
				return nil, newStreamBootstrapError(invalidErr, streamResult.Headers)
			}

			buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
			if bootstrapErr != nil {
				lease.Close()
				if errCtx := ctx.Err(); errCtx != nil {
					discardStreamChunks(streamResult.Chunks)
					return nil, errCtx
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false}
				applyResultError(&result, bootstrapErr)
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				discardStreamChunks(streamResult.Chunks)
				if shouldRetryTransportErrorWithSameAuth(result.Error, retryAttempt, transportRetries) {
					logSameAuthTransportRetry(ctx, auth, provider, resultModel, retryAttempt+1, transportRetries, bootstrapErr)
					continue
				}
				m.MarkResult(ctx, result)
				lastErr = bootstrapErr
				switch poolModeRetryDecisionForError(bootstrapErr, retryAttempt, poolModeRetries) {
				case poolModeRetryInvalidRequest:
					return nil, bootstrapErr
				case poolModeRetryRetry:
					continue
				}
				if result.AuthScoped || isAuthWideResultError(result.Error) {
					if statusCodeFromResult(result.Error) == http.StatusBadRequest {
						return nil, bootstrapErr
					}
					return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
				}
				if idx < len(execModels)-1 {
					break
				}
				return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			}

			if closed && len(buffered) == 0 {
				emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true, HTTPStatus: http.StatusBadGateway}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr}
				m.MarkResult(ctx, result)
				lastErr = emptyErr
				if poolModeRetryDecisionForError(emptyErr, retryAttempt, poolModeRetries) == poolModeRetryRetry {
					continue
				}
				if idx < len(execModels)-1 {
					break
				}
				return nil, newStreamBootstrapError(emptyErr, streamResult.Headers)
			}

			remaining := streamResult.Chunks
			if closed {
				closedCh := make(chan cliproxyexecutor.StreamChunk)
				close(closedCh)
				remaining = closedCh
			}
			lease.HandOff()
			return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, streamResult.Headers, buffered, remaining, lease.Finish), nil
		}
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, lastErr
}
