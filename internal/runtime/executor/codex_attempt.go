package executor

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

type codexNonStreamFetchMode uint8

const (
	codexNonStreamFetchAggregate codexNonStreamFetchMode = iota
	codexNonStreamFetchBody
)

type codexNonStreamAttempt struct {
	executor            *CodexExecutor
	ctx                 context.Context
	auth                *cliproxyauth.Auth
	from                sdktranslator.Format
	executionSessionID  string
	url                 string
	request             cliproxyexecutor.Request
	apiKey              string
	stream              bool
	needResponseHeaders bool
	fetchMode           codexNonStreamFetchMode

	call       codexPreparedHTTPCall
	body       []byte
	result     codexNonStreamHTTPResult
	usageOwner bool
}

func (attempt *codexNonStreamAttempt) execute(preparedBody []byte) error {
	call, errPrepare := attempt.executor.prepareCodexHTTPCall(
		attempt.ctx,
		attempt.auth,
		attempt.from,
		attempt.executionSessionID,
		attempt.url,
		attempt.request,
		preparedBody,
		attempt.apiKey,
		attempt.stream,
	)
	if errPrepare != nil {
		return errPrepare
	}
	attempt.call = call
	attempt.body = call.prepared.body
	helps.RecordAPIRequest(attempt.ctx, attempt.executor.cfg, call.requestLog)

	var errFetch error
	if attempt.fetchMode == codexNonStreamFetchBody {
		attempt.result, attempt.usageOwner, errFetch = attempt.executor.fetchCodexNonStreamResponse(
			attempt.ctx,
			attempt.auth,
			call.url,
			call.prepared,
			attempt.needResponseHeaders,
		)
	} else {
		attempt.result, attempt.usageOwner, errFetch = attempt.executor.fetchCodexResponsesAggregate(
			attempt.ctx,
			attempt.auth,
			call.url,
			call.prepared,
			attempt.needResponseHeaders,
		)
	}
	return errFetch
}

func (attempt *codexNonStreamAttempt) executeInitial(preparedBody []byte) error {
	if errExecute := attempt.execute(preparedBody); errExecute != nil {
		return errExecute
	}
	if attempt.result.statusCode != http.StatusUnauthorized {
		return nil
	}
	refreshedAuth, retried, errRefresh := attempt.executor.recoverCodexAuthAfterUnauthorized(attempt.ctx, attempt.auth, attempt.result.statusCode, attempt.result.body)
	if errRefresh != nil {
		return errRefresh
	}
	if !retried {
		return nil
	}
	attempt.auth = refreshedAuth
	attempt.apiKey, _ = codexCreds(refreshedAuth)
	return attempt.execute(preparedBody)
}

type codexStreamAttempt struct {
	executor           *CodexExecutor
	logCtx             context.Context
	upstreamCtx        context.Context
	auth               *cliproxyauth.Auth
	from               sdktranslator.Format
	executionSessionID string
	url                string
	request            cliproxyexecutor.Request
	apiKey             string

	call      codexPreparedHTTPCall
	body      []byte
	response  *http.Response
	errorBody []byte
}

func (attempt *codexStreamAttempt) prepare(preparedBody []byte) error {
	call, errPrepare := attempt.executor.prepareCodexHTTPCall(
		attempt.upstreamCtx,
		attempt.auth,
		attempt.from,
		attempt.executionSessionID,
		attempt.url,
		attempt.request,
		preparedBody,
		attempt.apiKey,
		true,
	)
	if errPrepare != nil {
		return errPrepare
	}
	attempt.call = call
	attempt.body = call.prepared.body
	attempt.response = nil
	attempt.errorBody = nil
	helps.RecordAPIRequest(attempt.logCtx, attempt.executor.cfg, call.requestLog)
	return nil
}

func (attempt *codexStreamAttempt) execute(preparedBody []byte) error {
	if errPrepare := attempt.prepare(preparedBody); errPrepare != nil {
		return errPrepare
	}
	call := attempt.call
	httpResp, errDo := attempt.executor.doCodexHTTPRequest(attempt.upstreamCtx, attempt.auth, call.prepared)
	if errDo != nil {
		codexRecordAPIResponseError(attempt.logCtx, attempt.executor.cfg, errDo)
		return errDo
	}
	attempt.response = httpResp
	helps.RecordAPIResponseMetadata(attempt.logCtx, attempt.executor.cfg, httpResp.StatusCode, httpResp.Header)
	attempt.executor.rememberCodexHTTPTurnState(attempt.auth, call.prepared, httpResp.Header)
	if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
		return nil
	}

	data, errRead := helps.ReadErrorResponseBody(httpResp.Body)
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("codex executor: close response body error: %v", errClose)
	}
	if errRead != nil {
		codexRecordAPIResponseError(attempt.logCtx, attempt.executor.cfg, errRead)
		return errRead
	}
	attempt.errorBody = data
	return nil
}

func (attempt *codexStreamAttempt) executeInitial(preparedBody []byte) error {
	if errExecute := attempt.execute(preparedBody); errExecute != nil {
		return errExecute
	}
	if attempt.response == nil || attempt.response.StatusCode != http.StatusUnauthorized {
		return nil
	}
	refreshedAuth, retried, errRefresh := attempt.executor.recoverCodexAuthAfterUnauthorized(attempt.upstreamCtx, attempt.auth, attempt.response.StatusCode, attempt.errorBody)
	if errRefresh != nil {
		codexRecordAPIResponseError(attempt.logCtx, attempt.executor.cfg, errRefresh)
		return errRefresh
	}
	if !retried {
		return nil
	}
	attempt.auth = refreshedAuth
	attempt.apiKey, _ = codexCreds(refreshedAuth)
	return attempt.execute(preparedBody)
}

type codexRecoveryAction uint8

const (
	codexRecoveryNone codexRecoveryAction = iota
	codexRecoveryDropReasoningReplay
	codexRecoveryDropClientEncryptedContent
)

type codexRequestRecovery struct {
	bodyWithoutReplay               []byte
	reasoningReplayApplied          bool
	clientEncryptedContentRetryUsed bool
}

func (recovery *codexRequestRecovery) nextRetry(ctx context.Context, replayScope codexReasoningReplayScope, preparedBody []byte, statusCode int, signatureErrorBody, encryptedContentErrorBody []byte) ([]byte, codexRecoveryAction, bool) {
	if clearCodexReasoningReplayOnInvalidSignature(replayScope, recovery.reasoningReplayApplied, statusCode, signatureErrorBody) {
		recovery.reasoningReplayApplied = false
		return recovery.bodyWithoutReplay, codexRecoveryDropReasoningReplay, true
	}
	if recovery.clientEncryptedContentRetryUsed {
		return nil, codexRecoveryNone, false
	}
	retryBody, ok := codexRetryBodyWithoutClientReasoningEncryptedContent(ctx, preparedBody, encryptedContentErrorBody)
	if !ok {
		return nil, codexRecoveryNone, false
	}
	recovery.clientEncryptedContentRetryUsed = true
	return retryBody, codexRecoveryDropClientEncryptedContent, true
}
