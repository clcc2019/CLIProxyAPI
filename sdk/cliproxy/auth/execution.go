package auth

import (
	"context"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type executionRetryPlan struct {
	providers           []string
	opts                cliproxyexecutor.Options
	maxRetryCredentials int
	maxWait             time.Duration
}

func (m *Manager) prepareExecutionRetryPlan(providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (executionRetryPlan, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return executionRetryPlan{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	opts = ensureRequestedModelMetadata(opts, req.Model)
	ensureOptionsMetadata(&opts)

	_, maxRetryCredentials, maxWait := m.retrySettings()
	return executionRetryPlan{
		providers:           normalized,
		opts:                opts,
		maxRetryCredentials: maxRetryCredentialsFromMetadata(opts.Metadata, maxRetryCredentials),
		maxWait:             maxWait,
	}, nil
}

func executeWithCooldown[T any](ctx context.Context, m *Manager, plan executionRetryPlan, model string, executeOnce func([]string, cliproxyexecutor.Options, int) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; ; attempt++ {
		result, errExecute := executeOnce(plan.providers, plan.opts, plan.maxRetryCredentials)
		if errExecute == nil {
			return result, nil
		}
		lastErr = errExecute
		wait, shouldRetry := m.shouldRetryAfterError(errExecute, attempt, plan.providers, model, plan.maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return zero, errWait
		}
	}
	if lastErr != nil {
		return zero, lastErr
	}
	return zero, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	plan, errPlan := m.prepareExecutionRetryPlan(providers, req, opts)
	if errPlan != nil {
		return cliproxyexecutor.Response{}, errPlan
	}
	return executeWithCooldown(ctx, m, plan, req.Model, func(providers []string, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
		return m.executeResponseMixedOnce(ctx, providers, req, opts, maxRetryCredentials, responseExecutionModeExecute)
	})
}

// ExecuteCount counts tokens using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	plan, errPlan := m.prepareExecutionRetryPlan(providers, req, opts)
	if errPlan != nil {
		return cliproxyexecutor.Response{}, errPlan
	}
	return executeWithCooldown(ctx, m, plan, req.Model, func(providers []string, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
		return m.executeResponseMixedOnce(ctx, providers, req, opts, maxRetryCredentials, responseExecutionModeCount)
	})
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	plan, errPlan := m.prepareExecutionRetryPlan(providers, req, opts)
	if errPlan != nil {
		return nil, errPlan
	}
	result, errStream := executeWithCooldown(ctx, m, plan, req.Model, func(providers []string, opts cliproxyexecutor.Options, maxRetryCredentials int) (*cliproxyexecutor.StreamResult, error) {
		return m.executeStreamMixedOnce(ctx, providers, req, opts, maxRetryCredentials)
	})
	if errStream != nil {
		var bootstrapErr *streamBootstrapError
		if errors.As(errStream, &bootstrapErr) && bootstrapErr != nil {
			return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
		}
		return nil, errStream
	}
	return result, nil
}

type mixedExecutionPolicy struct {
	retryLabel                       string
	usePreviousResponseAffinity      bool
	decoratePreparedExecutionContext func(context.Context) context.Context
}

type mixedExecutionState struct {
	ctx                 context.Context
	providers           []string
	req                 cliproxyexecutor.Request
	opts                cliproxyexecutor.Options
	maxRetryCredentials int
	routeModel          string
	tried               map[string]struct{}
	attempted           map[string]struct{}
	lastErr             error
	homeMode            bool
	homeAuthCount       int
}

type preparedMixedCredential struct {
	auth     *Auth
	executor ProviderExecutor
	provider string
	ctx      context.Context
	models   []string
	pooled   bool
}

func newMixedExecutionState(ctx context.Context, m *Manager, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (*mixedExecutionState, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	ensureOptionsMetadata(&opts)
	return &mixedExecutionState{
		ctx:                 ctx,
		providers:           providers,
		req:                 req,
		opts:                opts,
		maxRetryCredentials: maxRetryCredentials,
		routeModel:          routeModel,
		tried:               borrowAuthIDSet(),
		attempted:           borrowAuthIDSet(),
		homeMode:            m.HomeEnabled(),
		homeAuthCount:       1,
	}, nil
}

func (state *mixedExecutionState) close() {
	if state == nil {
		return
	}
	releaseAuthIDSet(state.tried)
	releaseAuthIDSet(state.attempted)
	state.tried = nil
	state.attempted = nil
}

func (state *mixedExecutionState) nextCredential(m *Manager, policy mixedExecutionPolicy) (*preparedMixedCredential, error) {
	for {
		if shouldStopCredentialFailover(state.homeMode, state.maxRetryCredentials, state.attempted, state.lastErr, state.opts) {
			return nil, m.credentialRetryLimitReachedError(state.ctx, policy.retryLabel, state.providers, state.routeModel, state.opts, state.maxRetryCredentials, state.attempted, state.lastErr)
		}
		pickOpts := state.opts
		if state.homeMode {
			pickOpts = withHomeAuthCount(state.opts, state.homeAuthCount)
		}
		previousResponseID := ""
		previousResponseAuthID := ""
		if policy.usePreviousResponseAffinity {
			previousResponseID, previousResponseAuthID = m.previousResponsePinnedAuthID(state.ctx, state.req, pickOpts)
			if previousResponseAuthID != "" {
				pickOpts = withPinnedAuthMetadata(pickOpts, previousResponseAuthID)
			}
		}
		auth, executor, provider, errPick := m.pickNextMixed(state.ctx, state.providers, state.routeModel, pickOpts, state.tried)
		if errPick != nil && previousResponseAuthID != "" && isRecoverableAffinityPickError(errPick) {
			m.invalidatePreviousResponseID(state.ctx, previousResponseID)
			fallbackPickOpts := state.opts
			if state.homeMode {
				fallbackPickOpts = withHomeAuthCount(state.opts, state.homeAuthCount)
			}
			auth, executor, provider, errPick = m.pickNextMixed(state.ctx, state.providers, state.routeModel, fallbackPickOpts, state.tried)
		}
		if errPick != nil {
			if shouldReturnLastErrorOnPickFailure(state.homeMode, state.lastErr, errPick) {
				return nil, state.lastErr
			}
			return nil, errPick
		}

		if log.IsLevelEnabled(log.DebugLevel) {
			debugLogAuthSelection(logEntryWithRequestID(state.ctx), auth, provider, state.req.Model)
		}
		publishSelectedAuthMetadata(state.opts.Metadata, auth.ID)
		state.tried[auth.ID] = struct{}{}

		execCtx := state.ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		if policy.decoratePreparedExecutionContext != nil {
			execCtx = policy.decoratePreparedExecutionContext(execCtx)
		}
		models, pooled := m.preparedExecutionModels(auth, state.routeModel)
		if len(models) == 0 {
			continue
		}
		state.attempted[auth.ID] = struct{}{}
		prepareStartedAt := time.Now()
		preparedAuth, errPrepare := m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{AuthID: auth.ID, Provider: provider, Model: state.routeModel, Success: false, Error: &Error{Message: errPrepare.Error()}}
			if status := statusCodeFromError(errPrepare); status > 0 {
				result.Error.HTTPStatus = status
			}
			m.MarkResult(execCtx, result)
			// A credential that fails to prepare (expired token, failed
			// refresh) never reaches the executor, so it would otherwise
			// vanish from the history despite consuming a failover slot.
			state.recordAttempt(&preparedMixedCredential{auth: auth, provider: provider}, prepareStartedAt, errPrepare)
			forceNewUpstreamSessionForNextCredential(&state.opts)
			state.lastErr = errPrepare
			continue
		}
		return &preparedMixedCredential{
			auth:     preparedAuth,
			executor: executor,
			provider: provider,
			ctx:      execCtx,
			models:   models,
			pooled:   pooled,
		}, nil
	}
}

// failCredential is the single funnel every credential failure passes through
// before failover moves on, which makes it the one place that sees the whole
// retry chain. Recording here keeps the per-attempt history complete without
// scattering bookkeeping across each protocol's error branches.
func (state *mixedExecutionState) failCredential(credential *preparedMixedCredential, startedAt time.Time, err error) error {
	state.recordAttempt(credential, startedAt, err)
	if isRequestInvalidError(err) {
		return err
	}
	forceNewUpstreamSessionForNextCredential(&state.opts)
	state.lastErr = err
	if state.homeMode {
		state.homeAuthCount++
	}
	return nil
}

func (state *mixedExecutionState) recordAttempt(credential *preparedMixedCredential, startedAt time.Time, err error) {
	if state == nil || err == nil {
		return
	}
	attempt := logging.UpstreamAttempt{
		Model:     state.routeModel,
		Status:    statusCodeFromError(err),
		Kind:      classifyAttemptFailure(err),
		Message:   err.Error(),
		ElapsedMs: logging.AttemptElapsed(startedAt),
	}
	if credential != nil {
		attempt.Provider = credential.provider
		if credential.auth != nil {
			attempt.AuthID = credential.auth.ID
			attempt.AuthLabel = credential.auth.Label
		}
	}
	logging.RecordUpstreamAttempt(state.ctx, attempt)
}

type responseExecutionMode uint8

const (
	responseExecutionModeExecute responseExecutionMode = iota
	responseExecutionModeCount
)

func (mode responseExecutionMode) retryLabel() string {
	if mode == responseExecutionModeCount {
		return "execute_count"
	}
	return "execute"
}

func (mode responseExecutionMode) usesPreviousResponseAffinity() bool {
	return mode == responseExecutionModeExecute
}

func (mode responseExecutionMode) withExecutionCallbacks(ctx context.Context, m *Manager) context.Context {
	if mode != responseExecutionModeExecute {
		return ctx
	}
	ctx = WithRefreshUpdateCallback(ctx, m.handleExecutionRefreshUpdate)
	ctx = WithAuthUpdateCallback(ctx, m.handleExecutionAuthUpdate)
	ctx = WithRateLimitUpdateCallback(ctx, m.handleExecutionRateLimitUpdate)
	return WithRefreshCoordinator(ctx, m.coordinatedRefreshForRequest)
}

func (mode responseExecutionMode) execute(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if mode == responseExecutionModeCount {
		return executor.CountTokens(ctx, auth, req, opts)
	}
	return executor.Execute(ctx, auth, req, opts)
}

func (mode responseExecutionMode) recordSuccess(m *Manager, ctx context.Context, authID string, resp cliproxyexecutor.Response) {
	if mode == responseExecutionModeExecute {
		m.bindPreviousResponseFromPayload(ctx, authID, resp.Payload)
	}
}

func (m *Manager) executeResponseMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, mode responseExecutionMode) (cliproxyexecutor.Response, error) {
	state, errState := newMixedExecutionState(ctx, m, providers, req, opts, maxRetryCredentials)
	if errState != nil {
		return cliproxyexecutor.Response{}, errState
	}
	defer state.close()

	policy := mixedExecutionPolicy{
		retryLabel:                  mode.retryLabel(),
		usePreviousResponseAffinity: mode.usesPreviousResponseAffinity(),
		decoratePreparedExecutionContext: func(execCtx context.Context) context.Context {
			execCtx = contextWithRequestedModelAlias(execCtx, state.opts, state.routeModel)
			return mode.withExecutionCallbacks(execCtx, m)
		},
	}
	for {
		credential, errCredential := state.nextCredential(m, policy)
		if errCredential != nil {
			return cliproxyexecutor.Response{}, errCredential
		}

		var authErr error
		stopModelLoop := false
		credentialStartedAt := time.Now()
		poolModeRetries := m.apiKeyPoolModeRetries(credential.auth)
		transportRetries := m.requestRetryLimitForAuth(credential.auth)
		for _, upstreamModel := range credential.models {
			resultModel := m.stateModelForExecution(credential.auth, state.routeModel, upstreamModel, credential.pooled)
			execReq := state.req
			execReq.Model = upstreamModel
			execReq = m.withOAuthModelAliasReasoningEffort(execReq, credential.auth, state.routeModel, state.opts)
			for retryAttempt := 0; ; retryAttempt++ {
				releaseAdmission, errAdmission := m.admitAuthExecution(credential.auth, resultModel, retryAttempt > 0)
				if errAdmission != nil {
					authErr = errAdmission
					stopModelLoop = true
					break
				}
				lease := m.beginAuthInFlight(credential.auth.ID)
				resp, errExec := func() (cliproxyexecutor.Response, error) {
					defer releaseAdmission()
					defer func() {
						if r := recover(); r != nil {
							lease.Close()
							panic(r)
						}
					}()
					return mode.execute(credential.ctx, credential.executor, credential.auth, execReq, state.opts)
				}()
				lease.Close()
				result := Result{AuthID: credential.auth.ID, Provider: credential.provider, Model: resultModel, Success: errExec == nil}
				if errExec != nil {
					if errCtx := credential.ctx.Err(); errCtx != nil {
						return cliproxyexecutor.Response{}, errCtx
					}
					applyResultError(&result, errExec)
					if shouldRetryTransportErrorWithSameAuth(result.Error, retryAttempt, transportRetries) {
						logSameAuthTransportRetry(credential.ctx, credential.auth, credential.provider, resultModel, retryAttempt+1, transportRetries, errExec)
						continue
					}
					if shouldRetryClaudeOverloadWithSameAuth(credential.provider, errExec, retryAttempt, transportRetries) {
						logSameAuthClaudeOverloadRetry(credential.ctx, credential.auth, credential.provider, resultModel, retryAttempt+1, transportRetries, errExec)
						continue
					}
					if ra := retryAfterFromError(errExec); ra != nil {
						result.RetryAfter = ra
					}
					m.MarkResult(credential.ctx, result)
					if isClaudeOverloadedFailure(credential.provider, errExec) {
						return cliproxyexecutor.Response{}, errExec
					}
					clearSelectedAuthMetadataForCredentialFailover(credential.provider, state.opts.Metadata, credential.auth.ID, errExec)
					authErr = errExec
					switch poolModeRetryDecisionForError(errExec, retryAttempt, poolModeRetries) {
					case poolModeRetryInvalidRequest:
						return cliproxyexecutor.Response{}, errExec
					case poolModeRetryRetry:
						continue
					}
					if result.AuthScoped || isAuthWideResultError(result.Error) {
						stopModelLoop = true
					}
					break
				}
				m.MarkResult(credential.ctx, result)
				mode.recordSuccess(m, credential.ctx, credential.auth.ID, resp)
				return resp, nil
			}
			if stopModelLoop {
				break
			}
		}
		if authErr != nil {
			if errFailover := state.failCredential(credential, credentialStartedAt, authErr); errFailover != nil {
				return cliproxyexecutor.Response{}, errFailover
			}
		}
	}
}
func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (*cliproxyexecutor.StreamResult, error) {
	state, errState := newMixedExecutionState(ctx, m, providers, req, opts, maxRetryCredentials)
	if errState != nil {
		return nil, errState
	}
	defer state.close()

	policy := mixedExecutionPolicy{
		retryLabel:                  "execute_stream",
		usePreviousResponseAffinity: true,
	}
	for {
		credential, errCredential := state.nextCredential(m, policy)
		if errCredential != nil {
			return nil, errCredential
		}
		credentialStartedAt := time.Now()
		execReq := sanitizeDownstreamWebsocketFallbackRequest(credential.ctx, credential.auth, state.req)
		streamResult, errStream := m.executeStreamWithModelPool(
			credential.ctx,
			credential.executor,
			credential.auth,
			credential.provider,
			execReq,
			state.opts,
			state.routeModel,
			credential.models,
			credential.pooled,
		)
		if errStream != nil {
			if errCtx := credential.ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			if isClaudeOverloadedFailure(credential.provider, errStream) {
				return nil, errStream
			}
			if errFailover := state.failCredential(credential, credentialStartedAt, errStream); errFailover != nil {
				return nil, errFailover
			}
			continue
		}
		return streamResult, nil
	}
}
