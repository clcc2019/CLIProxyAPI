package executor

import "net/http"

const (
	codexRequestHeaderInitialCapacity = 24

	codexHeaderSessionID                        = "Session_id"
	codexHeaderThreadID                         = "Thread_id"
	codexHeaderOfficialSessionID                = "Session-Id"
	codexHeaderOfficialThreadID                 = "Thread-Id"
	codexHeaderChatGPTAccountID                 = "ChatGPT-Account-ID"
	codexHeaderOpenAIBeta                       = "OpenAI-Beta"
	codexHeaderOpenAISubagent                   = "X-OpenAI-Subagent"
	codexHeaderOpenAIFedramp                    = "X-OpenAI-Fedramp"
	codexHeaderResponsesAPIIncludeTimingMetrics = "x-responsesapi-include-timing-metrics"
	codexHeaderCompactionTrigger                = "X-Codex-Compaction-Trigger"
	codexHeaderCompactionReason                 = "X-Codex-Compaction-Reason"
	codexHeaderCompactionImpl                   = "X-Codex-Compaction-Implementation"
	codexHeaderCompactionPhase                  = "X-Codex-Compaction-Phase"
	codexHeaderCompactionStrategy               = "X-Codex-Compaction-Strategy"
	codexHeaderTurnState                        = "X-Codex-Turn-State"
	codexHeaderTurnMetadata                     = "X-Codex-Turn-Metadata"
	codexHeaderOAIAttestation                   = "X-OAI-Attestation"

	codexWireHeaderOpenAIBeta                       = "Openai-Beta"
	codexWireHeaderOpenAISubagent                   = "X-Openai-Subagent"
	codexWireHeaderOpenAIFedramp                    = "X-Openai-Fedramp"
	codexWireHeaderResponsesAPIIncludeTimingMetrics = "X-Responsesapi-Include-Timing-Metrics"
	codexWireHeaderOAIAttestation                   = "X-Oai-Attestation"
)

func codexSetSingleHeaderValue(headers http.Header, key string, value string) {
	if values := headers[key]; len(values) > 0 {
		values[0] = value
		headers[key] = values[:1]
		return
	}
	headers[key] = []string{value}
}
