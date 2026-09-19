package redisqueue

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func init() {
	coreusage.RegisterPlugin(&usageQueuePlugin{})
}

type usageQueuePlugin struct{}

func (p *usageQueuePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	if !Enabled() || !UsageStatisticsEnabled() {
		return
	}

	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	modelName := strings.TrimSpace(record.Model)
	if modelName == "" {
		modelName = "unknown"
	}
	aliasName := strings.TrimSpace(record.Alias)
	if aliasName == "" {
		aliasName = modelName
	}
	provider := strings.TrimSpace(record.Provider)
	if provider == "" {
		provider = "unknown"
	}
	executorType := strings.TrimSpace(record.ExecutorType)
	if executorType == "" {
		executorType = "unknown"
	}
	authType := strings.TrimSpace(record.AuthType)
	if authType == "" {
		authType = "unknown"
	}
	apiKey := strings.TrimSpace(record.APIKey)
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	reasoningEffort := strings.TrimSpace(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = coreusage.ReasoningEffortFromContext(ctx)
	}
	requestServiceTier := strings.TrimSpace(record.RequestServiceTier)
	if requestServiceTier == "" {
		requestServiceTier = strings.TrimSpace(record.ServiceTier)
	}
	if requestServiceTier == "" {
		requestServiceTier = coreusage.ServiceTierFromContext(ctx)
	}
	responseServiceTier := strings.TrimSpace(record.ResponseServiceTier)
	if responseServiceTier == "" {
		responseServiceTier = strings.TrimSpace(record.Detail.ResponseServiceTier)
	}
	requestedReasoningEffort := strings.TrimSpace(record.RequestedReasoningEffort)
	if requestedReasoningEffort == "" {
		requestedReasoningEffort = strings.TrimSpace(record.ModelReasoningEffort)
	}
	upstreamReasoningEffort := strings.TrimSpace(record.UpstreamReasoningEffort)
	if upstreamReasoningEffort == "" {
		upstreamReasoningEffort = strings.TrimSpace(record.ReasoningEffort)
	}
	requestedServiceTier := strings.TrimSpace(record.RequestedServiceTier)
	if requestedServiceTier == "" {
		requestedServiceTier = requestServiceTier
	}
	upstreamServiceTier := strings.TrimSpace(record.UpstreamServiceTier)

	tokens := tokenStats{
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		ReasoningTokens:     record.Detail.ReasoningTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
	}
	tokens = normalizeQueuedTokenStats(provider, tokens)

	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	fail := resolveFail(ctx, record, failed)

	detail := requestDetail{
		Timestamp:       timestamp,
		LatencyMs:       record.Latency.Milliseconds(),
		TTFTMs:          record.TTFT.Milliseconds(),
		RequestedModel:  firstNonEmptyQueuedModel(record.RequestedModel, record.Alias),
		UpstreamModel:   strings.TrimSpace(record.Model),
		ResponseModel:   strings.TrimSpace(record.ResponseModel),
		ModelDowngraded: modelWasDowngraded(record),
		Source:          record.Source,
		AuthIndex:       record.AuthIndex,
		Tokens:          tokens,
		Failed:          failed,
		Fail:            fail,
		ErrorMessage:    normalizeUsageQueueErrorMessage(record.ErrorMessage, failed),
		ResponseHeaders: record.ResponseHeaders,
		Stream:          record.Stream,
	}

	payload, err := json.Marshal(queuedUsageDetail{
		requestDetail:            detail,
		Provider:                 provider,
		ExecutorType:             executorType,
		Model:                    modelName,
		Alias:                    aliasName,
		Endpoint:                 resolveEndpoint(ctx),
		AuthType:                 authType,
		APIKey:                   apiKey,
		RequestID:                requestID,
		ReasoningEffort:          reasoningEffort,
		RequestedReasoningEffort: requestedReasoningEffort,
		UpstreamReasoningEffort:  upstreamReasoningEffort,
		ServiceTier:              requestServiceTier,
		RequestServiceTier:       requestServiceTier,
		RequestedServiceTier:     requestedServiceTier,
		UpstreamServiceTier:      upstreamServiceTier,
		ResponseServiceTier:      responseServiceTier,
		ModelMappingChain:        queuedModelMappingChain(record),
		ResponseModelMismatch:    queuedResponseModelMismatch(record),
		ResponseModelConflict:    record.ResponseModelConflict,
	})
	if err != nil {
		return
	}
	Enqueue(payload)
}

type queuedUsageDetail struct {
	requestDetail
	Provider                 string `json:"provider"`
	ExecutorType             string `json:"executor_type"`
	Model                    string `json:"model"`
	Alias                    string `json:"alias"`
	Endpoint                 string `json:"endpoint"`
	AuthType                 string `json:"auth_type"`
	APIKey                   string `json:"api_key"`
	RequestID                string `json:"request_id"`
	ReasoningEffort          string `json:"reasoning_effort"`
	RequestedReasoningEffort string `json:"requested_reasoning_effort,omitempty"`
	UpstreamReasoningEffort  string `json:"upstream_reasoning_effort,omitempty"`
	ServiceTier              string `json:"service_tier"`
	RequestServiceTier       string `json:"request_service_tier"`
	RequestedServiceTier     string `json:"requested_service_tier,omitempty"`
	UpstreamServiceTier      string `json:"upstream_service_tier,omitempty"`
	ResponseServiceTier      string `json:"response_service_tier"`
	ModelMappingChain        string `json:"model_mapping_chain,omitempty"`
	ResponseModelMismatch    *bool  `json:"response_model_mismatch,omitempty"`
	ResponseModelConflict    bool   `json:"response_model_conflict,omitempty"`
}

type requestDetail struct {
	Timestamp                time.Time   `json:"timestamp"`
	LatencyMs                int64       `json:"latency_ms"`
	TTFTMs                   int64       `json:"ttft_ms"`
	RequestedModel           string      `json:"requested_model,omitempty"`
	UpstreamModel            string      `json:"upstream_model,omitempty"`
	ResponseModel            string      `json:"response_model,omitempty"`
	ModelDowngraded          bool        `json:"model_downgraded,omitempty"`
	RequestedReasoningEffort string      `json:"requested_reasoning_effort,omitempty"`
	UpstreamReasoningEffort  string      `json:"upstream_reasoning_effort,omitempty"`
	RequestedServiceTier     string      `json:"requested_service_tier,omitempty"`
	UpstreamServiceTier      string      `json:"upstream_service_tier,omitempty"`
	ResponseServiceTier      string      `json:"response_service_tier,omitempty"`
	ModelMappingChain        string      `json:"model_mapping_chain,omitempty"`
	ResponseModelMismatch    *bool       `json:"response_model_mismatch,omitempty"`
	ResponseModelConflict    bool        `json:"response_model_conflict,omitempty"`
	Source                   string      `json:"source"`
	AuthIndex                string      `json:"auth_index"`
	Tokens                   tokenStats  `json:"tokens"`
	Failed                   bool        `json:"failed"`
	Fail                     failDetail  `json:"fail"`
	ErrorMessage             string      `json:"error_message,omitempty"`
	ResponseHeaders          http.Header `json:"response_headers,omitempty"`
	Stream                   bool        `json:"stream"`
}

func firstNonEmptyQueuedModel(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// modelWasDowngraded compares the model that was actually sent upstream
// (after alias resolution) with the model reported by the upstream response.
func modelWasDowngraded(record coreusage.Record) bool {
	requestedModel := strings.TrimSpace(record.Model)
	responseModel := strings.TrimSpace(record.ResponseModel)
	return requestedModel != "" && responseModel != "" && !strings.EqualFold(requestedModel, responseModel)
}

func queuedResponseModelMismatch(record coreusage.Record) *bool {
	if explicit := record.ResponseModelMismatch; explicit != nil {
		value := *explicit
		return &value
	}
	upstreamModel := strings.TrimSpace(record.Model)
	responseModel := strings.TrimSpace(record.ResponseModel)
	if upstreamModel == "" || responseModel == "" {
		return nil
	}
	value := !strings.EqualFold(upstreamModel, responseModel)
	return &value
}

func queuedModelMappingChain(record coreusage.Record) string {
	if chain := strings.TrimSpace(record.ModelMappingChain); chain != "" {
		return chain
	}
	models := []string{firstNonEmptyQueuedModel(record.RequestedModel, record.Alias), strings.TrimSpace(record.Model), strings.TrimSpace(record.ResponseModel)}
	chain := make([]string, 0, len(models))
	for _, model := range models {
		if model == "" || (len(chain) > 0 && strings.EqualFold(chain[len(chain)-1], model)) {
			continue
		}
		chain = append(chain, model)
	}
	if len(chain) < 2 {
		return ""
	}
	return strings.Join(chain, "→")
}

type tokenStats struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

func normalizeQueuedTokenStats(provider string, tokens tokenStats) tokenStats {
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens
		if !queuedProviderReportsReasoningAsOutputDetail(provider) {
			tokens.TotalTokens += tokens.ReasoningTokens
		}
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	return tokens
}

func queuedProviderReportsReasoningAsOutputDetail(provider string) bool {
	provider = strings.TrimSpace(provider)
	switch {
	case strings.EqualFold(provider, "codex"):
		return true
	case strings.EqualFold(provider, "openai"):
		return true
	default:
		return false
	}
}

type failDetail struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func resolveFail(ctx context.Context, record coreusage.Record, failed bool) failDetail {
	fail := failDetail{
		StatusCode: record.Fail.StatusCode,
		Body:       strings.TrimSpace(record.Fail.Body),
	}
	if !failed {
		return failDetail{StatusCode: 200}
	}
	if fail.StatusCode <= 0 {
		fail.StatusCode = internallogging.GetResponseStatus(ctx)
	}
	if fail.StatusCode <= 0 {
		fail.StatusCode = 500
	}
	return fail
}

func resolveSuccess(ctx context.Context) bool {
	status := internallogging.GetResponseStatus(ctx)
	if status == 0 {
		return true
	}
	return status < httpStatusBadRequest
}

func resolveEndpoint(ctx context.Context) string {
	return strings.TrimSpace(internallogging.GetEndpoint(ctx))
}

const httpStatusBadRequest = 400

func normalizeUsageQueueErrorMessage(message string, failed bool) string {
	if !failed {
		return ""
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	message = strings.Join(strings.Fields(message), " ")
	const maxLen = 2000
	if len(message) > maxLen {
		message = truncateUsageQueueErrorMessage(message, maxLen)
	}
	return message
}

func truncateUsageQueueErrorMessage(message string, maxLen int) string {
	runes := []rune(message)
	if len(runes) <= maxLen {
		return message
	}
	return strings.TrimSpace(string(runes[:maxLen])) + "..."
}
