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
	codexHeaderOpenAIInternalCodexResponsesLite = "X-OpenAI-Internal-Codex-Responses-Lite"
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
	codexWireHeaderOpenAIInternalCodexResponsesLite = "X-Openai-Internal-Codex-Responses-Lite"
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

func codexSetPairedSingleHeaderValues(headers http.Header, firstKey string, firstValue string, secondKey string, secondValue string) {
	if len(headers[firstKey]) == 0 && len(headers[secondKey]) == 0 {
		values := []string{firstValue, secondValue}
		headers[firstKey] = values[0:1:1]
		headers[secondKey] = values[1:2:2]
		return
	}
	codexSetSingleHeaderValue(headers, firstKey, firstValue)
	codexSetSingleHeaderValue(headers, secondKey, secondValue)
}

func codexSetTripleSingleHeaderValues(headers http.Header, firstKey string, firstValue string, secondKey string, secondValue string, thirdKey string, thirdValue string) {
	if len(headers[firstKey]) == 0 && len(headers[secondKey]) == 0 && len(headers[thirdKey]) == 0 {
		values := []string{firstValue, secondValue, thirdValue}
		headers[firstKey] = values[0:1:1]
		headers[secondKey] = values[1:2:2]
		headers[thirdKey] = values[2:3:3]
		return
	}
	codexSetSingleHeaderValue(headers, firstKey, firstValue)
	codexSetSingleHeaderValue(headers, secondKey, secondValue)
	codexSetSingleHeaderValue(headers, thirdKey, thirdValue)
}

func codexHeaderGet(headers http.Header, key string) string {
	if headers == nil {
		return ""
	}
	if values := headers[key]; len(values) > 0 {
		return values[0]
	}
	if canonical := codexKnownCanonicalHeaderKey(key); canonical != "" {
		if canonical == key {
			return ""
		}
		if values := headers[canonical]; len(values) > 0 {
			return values[0]
		}
		return ""
	}
	return headers.Get(key)
}

func codexKnownCanonicalHeaderKey(key string) string {
	switch key {
	case "Accept",
		"Authorization",
		"Content-Type",
		"Conversation_id",
		"Originator",
		"Traceparent",
		"Tracestate",
		"User-Agent",
		"Version",
		codexHeaderCompactionImpl,
		codexHeaderCompactionPhase,
		codexHeaderCompactionReason,
		codexHeaderCompactionStrategy,
		codexHeaderCompactionTrigger,
		codexHeaderInstallationID,
		codexHeaderOfficialSessionID,
		codexHeaderOfficialThreadID,
		codexHeaderParentThreadID,
		codexHeaderSessionID,
		codexHeaderThreadID,
		codexHeaderTurnMetadata,
		codexHeaderTurnState,
		codexHeaderWindowID,
		codexWireHeaderMemgenRequest,
		codexWireHeaderOAIAttestation,
		codexWireHeaderOpenAIBeta,
		codexWireHeaderOpenAIFedramp,
		codexWireHeaderOpenAISubagent,
		codexWireHeaderResponsesAPIIncludeTimingMetrics:
		return key
	case "ChatGPT-Account-ID":
		return "Chatgpt-Account-Id"
	case "OpenAI-Beta":
		return codexWireHeaderOpenAIBeta
	case "X-Client-Request-Id":
		return "X-Client-Request-Id"
	case "X-Codex-Beta-Features":
		return "X-Codex-Beta-Features"
	case "X-OAI-Attestation":
		return codexWireHeaderOAIAttestation
	case "X-OpenAI-Fedramp":
		return codexWireHeaderOpenAIFedramp
	case "X-OpenAI-Memgen-Request":
		return codexWireHeaderMemgenRequest
	case "X-OpenAI-Subagent":
		return codexWireHeaderOpenAISubagent
	case "X-Session-ID":
		return "X-Session-Id"
	case "X-Thread-ID":
		return "X-Thread-Id"
	case codexHeaderResponsesAPIIncludeTimingMetrics:
		return codexWireHeaderResponsesAPIIncludeTimingMetrics
	default:
		return ""
	}
}
