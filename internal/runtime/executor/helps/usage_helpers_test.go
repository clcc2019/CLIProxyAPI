package helps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail := ParseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.CacheReadTokens != 4 {
		t.Fatalf("cache read tokens = %d, want %d", detail.CacheReadTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7,"cache_creation_tokens":3},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail := ParseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.CacheReadTokens != 7 {
		t.Fatalf("cache read tokens = %d, want %d", detail.CacheReadTokens, 7)
	}
	if detail.CacheCreationTokens != 3 {
		t.Fatalf("cache creation tokens = %d, want %d", detail.CacheCreationTokens, 3)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestParseOpenAIUsageOfficialCodexTokenUsageShape(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15,"total_tokens":150}}`)
	detail := ParseOpenAIUsage(data)
	if detail.InputTokens != 100 {
		t.Fatalf("input tokens = %d, want 100", detail.InputTokens)
	}
	if detail.OutputTokens != 50 {
		t.Fatalf("output tokens = %d, want 50", detail.OutputTokens)
	}
	if detail.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", detail.TotalTokens)
	}
	if detail.CachedTokens != 40 {
		t.Fatalf("cached tokens = %d, want 40", detail.CachedTokens)
	}
	if detail.CacheReadTokens != 40 {
		t.Fatalf("cache read tokens = %d, want 40", detail.CacheReadTokens)
	}
	if detail.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", detail.ReasoningTokens)
	}
}

func TestParseOpenAIUsageOfficialCodexTokenUsageShapeWithoutTotal(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15}}`)
	detail := ParseOpenAIUsage(data)
	if detail.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", detail.TotalTokens)
	}
	if detail.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", detail.ReasoningTokens)
	}
}

func TestParseCodexUsageOfficialTokenUsageShape(t *testing.T) {
	data := []byte(`{"response":{"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15,"total_tokens":150}}}`)
	detail, ok := ParseCodexUsage(data)
	if !ok {
		t.Fatal("expected Codex usage to be parsed")
	}
	if detail.InputTokens != 100 {
		t.Fatalf("input tokens = %d, want 100", detail.InputTokens)
	}
	if detail.OutputTokens != 50 {
		t.Fatalf("output tokens = %d, want 50", detail.OutputTokens)
	}
	if detail.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", detail.TotalTokens)
	}
	if detail.CachedTokens != 40 {
		t.Fatalf("cached tokens = %d, want 40", detail.CachedTokens)
	}
	if detail.CacheReadTokens != 40 {
		t.Fatalf("cache read tokens = %d, want 40", detail.CacheReadTokens)
	}
	if detail.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", detail.ReasoningTokens)
	}
}

func TestParseCodexUsageOfficialTokenUsageShapeWithoutTotal(t *testing.T) {
	data := []byte(`{"response":{"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15}}}`)
	detail, ok := ParseCodexUsage(data)
	if !ok {
		t.Fatal("expected Codex usage to be parsed")
	}
	if detail.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", detail.TotalTokens)
	}
	if detail.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", detail.ReasoningTokens)
	}
}

func TestParseCodexUsageRetainsResponseTierWithoutTokenUsage(t *testing.T) {
	detail, ok := ParseCodexUsage([]byte(`{"response":{"service_tier":"priority"}}`))
	if !ok {
		t.Fatal("expected response tier to be parsed without token usage")
	}
	if detail.ResponseServiceTier != "priority" {
		t.Fatalf("response service tier = %q, want priority", detail.ResponseServiceTier)
	}
}

func TestParseOpenAIUsageIncludesResponseTier(t *testing.T) {
	detail := ParseOpenAIUsage([]byte(`{"service_tier":"default","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`))
	if detail.ResponseServiceTier != "default" {
		t.Fatalf("response service tier = %q, want default", detail.ResponseServiceTier)
	}
}

func TestParseOpenAIStreamUsageChatCompletions(t *testing.T) {
	line := []byte(`data: {"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail, ok := ParseOpenAIStreamUsage(line)
	if !ok {
		t.Fatal("expected usage to be parsed")
	}
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.CacheReadTokens != 4 {
		t.Fatalf("cache read tokens = %d, want %d", detail.CacheReadTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIStreamUsageResponses(t *testing.T) {
	line := []byte(`data: {"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7,"cache_creation_tokens":3},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail, ok := ParseOpenAIStreamUsage(line)
	if !ok {
		t.Fatal("expected usage to be parsed")
	}
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.CacheReadTokens != 7 {
		t.Fatalf("cache read tokens = %d, want %d", detail.CacheReadTokens, 7)
	}
	if detail.CacheCreationTokens != 3 {
		t.Fatalf("cache creation tokens = %d, want %d", detail.CacheCreationTokens, 3)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestParseOpenAIUsageCacheReadCreationCompatibilityFields(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}`)
	detail := ParseOpenAIUsage(data)
	if detail.CachedTokens != 20 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 20)
	}
	if detail.CacheReadTokens != 20 {
		t.Fatalf("cache read tokens = %d, want %d", detail.CacheReadTokens, 20)
	}
	if detail.CacheCreationTokens != 30 {
		t.Fatalf("cache creation tokens = %d, want %d", detail.CacheCreationTokens, 30)
	}
}

func TestParseOpenAIStreamUsageOfficialCodexTokenUsageShape(t *testing.T) {
	line := []byte(`data: {"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15,"total_tokens":150}}`)
	detail, ok := ParseOpenAIStreamUsage(line)
	if !ok {
		t.Fatal("expected usage to be parsed")
	}
	if detail.InputTokens != 100 {
		t.Fatalf("input tokens = %d, want 100", detail.InputTokens)
	}
	if detail.OutputTokens != 50 {
		t.Fatalf("output tokens = %d, want 50", detail.OutputTokens)
	}
	if detail.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", detail.TotalTokens)
	}
	if detail.CachedTokens != 40 {
		t.Fatalf("cached tokens = %d, want 40", detail.CachedTokens)
	}
	if detail.CacheReadTokens != 40 {
		t.Fatalf("cache read tokens = %d, want 40", detail.CacheReadTokens)
	}
	if detail.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", detail.ReasoningTokens)
	}
}

func TestParseOpenAIStreamUsageOfficialCodexTokenUsageShapeWithoutTotal(t *testing.T) {
	line := []byte(`data: {"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15}}`)
	detail, ok := ParseOpenAIStreamUsage(line)
	if !ok {
		t.Fatal("expected usage to be parsed")
	}
	if detail.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", detail.TotalTokens)
	}
	if detail.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", detail.ReasoningTokens)
	}
}

func TestParseOpenAIStreamUsageNullUsageIgnored(t *testing.T) {
	line := []byte(`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}`)
	_, ok := ParseOpenAIStreamUsage(line)
	if ok {
		t.Fatal("expected usage:null chunk to be ignored")
	}
}

func TestParseOpenAIStreamUsageEmptyUsageObjectIgnored(t *testing.T) {
	line := []byte(`data: {"choices":[{"delta":{"content":"hi"}}],"usage":{}}`)
	_, ok := ParseOpenAIStreamUsage(line)
	if ok {
		t.Fatal("expected usage:{} chunk to be ignored")
	}
}

func TestParseOpenAIStreamUsageZeroUsageObjectRetained(t *testing.T) {
	line := []byte(`data: {"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	_, ok := ParseOpenAIStreamUsage(line)
	if !ok {
		t.Fatal("expected explicit all-zero usage chunk to be retained")
	}
}

func TestStreamUsageBufferPreservesTierAndKeepsFinalUsage(t *testing.T) {
	var buffer StreamUsageBuffer
	buffer.ObserveOpenAIStream([]byte(`data: {"service_tier":"default"}`))
	buffer.ObserveOpenAIStream([]byte(`data: {"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	buffer.ObserveOpenAIStream([]byte(`data: {"service_tier":"priority","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))

	detail, ok := buffer.Detail()
	if !ok {
		t.Fatal("buffer detail ok = false, want true")
	}
	if detail.InputTokens != 10 || detail.OutputTokens != 5 || detail.TotalTokens != 15 {
		t.Fatalf("detail = %+v, want final usage", detail)
	}
	if detail.ResponseServiceTier != "priority" {
		t.Fatalf("response service tier = %q, want priority", detail.ResponseServiceTier)
	}
}

func TestStreamUsageBufferIgnoresIrrelevantAndInvalidChunks(t *testing.T) {
	var buffer StreamUsageBuffer
	buffer.ObserveOpenAIStream([]byte(`data: {"content":"the word \"usage\" appears here"}`))
	buffer.ObserveOpenAIStream([]byte(`data: {"usage":`))
	buffer.ObserveOpenAIStream([]byte(`data: {"usage":null}`))
	if detail, ok := buffer.Detail(); ok {
		t.Fatalf("detail = %+v ok=true, want empty buffer", detail)
	}
}

func TestParseClaudeUsageSeparatesCacheReadAndCreation(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}`)
	detail := ParseClaudeUsage(data)
	if detail.InputTokens != 150 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 150)
	}
	if detail.OutputTokens != 5 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 5)
	}
	if detail.CachedTokens != 20 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 20)
	}
	if detail.CacheReadTokens != 20 {
		t.Fatalf("cache read tokens = %d, want %d", detail.CacheReadTokens, 20)
	}
	if detail.CacheCreationTokens != 30 {
		t.Fatalf("cache creation tokens = %d, want %d", detail.CacheCreationTokens, 30)
	}
	if detail.TotalTokens != 155 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 155)
	}
}

func TestParseClaudeStreamUsageIgnoresZeroUsage(t *testing.T) {
	line := []byte(`data: {"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`)
	_, ok := ParseClaudeStreamUsage(line)
	if ok {
		t.Fatal("expected all-zero Claude usage chunk to be ignored")
	}
}

func TestUsageReporterBuildRecordIncludesLatency(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "openai",
		model:       "gpt-5.4",
		requestedAt: time.Now().Add(-1500 * time.Millisecond),
	}

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false, nil)
	if record.Latency < time.Second {
		t.Fatalf("latency = %v, want >= 1s", record.Latency)
	}
	if record.Latency > 3*time.Second {
		t.Fatalf("latency = %v, want <= 3s", record.Latency)
	}
}

func TestUsageReporterBuildRecordIncludesRequestAndResponseServiceTiers(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "openai",
		model:       "gpt-5.4",
		serviceTier: "priority",
		requestedAt: time.Now(),
	}

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3, ResponseServiceTier: "default"}, false)
	if record.ServiceTier != "priority" || record.RequestServiceTier != "priority" {
		t.Fatalf("request service tiers = (%q, %q), want priority", record.ServiceTier, record.RequestServiceTier)
	}
	if record.ResponseServiceTier != "default" {
		t.Fatalf("response service tier = %q, want default", record.ResponseServiceTier)
	}
}

func TestUsageReporterBuildRecordDoesNotDoubleCountOpenAIReasoning(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "openai",
		model:       "gpt-5.4",
		requestedAt: time.Now(),
	}

	detail := ParseOpenAIUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":50,"reasoning_output_tokens":15}}`))
	record := reporter.buildRecord(detail, false)
	if record.Detail.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", record.Detail.TotalTokens)
	}
	if record.Detail.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", record.Detail.ReasoningTokens)
	}
}

func TestUsageReporterBuildRecordDoesNotDoubleCountCodexReasoningWithoutTotal(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "codex",
		model:       "gpt-5.4",
		requestedAt: time.Now(),
	}

	record := reporter.buildRecord(usage.Detail{
		InputTokens:     100,
		OutputTokens:    50,
		ReasoningTokens: 15,
	}, false)
	if record.Detail.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", record.Detail.TotalTokens)
	}
	if record.Detail.ReasoningTokens != 15 {
		t.Fatalf("reasoning tokens = %d, want 15", record.Detail.ReasoningTokens)
	}
}

func TestUsageReporterBuildRecordKeepsSeparateReasoningProviderTotals(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "oauth",
		model:       "oauth-model",
		requestedAt: time.Now(),
	}

	record := reporter.buildRecord(usage.Detail{
		InputTokens:     100,
		OutputTokens:    50,
		ReasoningTokens: 15,
	}, false)
	if record.Detail.TotalTokens != 165 {
		t.Fatalf("total tokens = %d, want 165", record.Detail.TotalTokens)
	}
}

func TestUsageReporterTrackHTTPClientStartsTTFTBeforeRoundTrip(t *testing.T) {
	delay := 40 * time.Millisecond
	reporter := NewUsageReporter(context.Background(), "openai", "gpt-5.4", nil)
	client := reporter.TrackHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			time.Sleep(delay)
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
	})

	req, errNewRequest := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.invalid/v1/chat/completions", strings.NewReader("{}"))
	if errNewRequest != nil {
		t.Fatalf("NewRequestWithContext() error = %v", errNewRequest)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatalf("Do() error = %v", errDo)
	}
	if _, errRead := io.ReadAll(resp.Body); errRead != nil {
		t.Fatalf("ReadAll() error = %v", errRead)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close error = %v", errClose)
	}
	if got := reporter.ttftDuration(); got < delay {
		t.Fatalf("ttft = %v, want >= %v", got, delay)
	}
}

func TestUsageReporterBuildRecordIncludesRequestedModelAlias(t *testing.T) {
	ctx := usage.WithRequestedModelAlias(context.Background(), "client-gpt")
	reporter := NewUsageReporter(ctx, "openai", "gpt-5.4", nil)

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Model != "gpt-5.4" {
		t.Fatalf("model = %q, want %q", record.Model, "gpt-5.4")
	}
	if record.Alias != "client-gpt" {
		t.Fatalf("alias = %q, want %q", record.Alias, "client-gpt")
	}
}

func TestUsageReporterBuildRecordIncludesErrorMessage(t *testing.T) {
	reporter := &UsageReporter{provider: "openai", model: "gpt-5.4", requestedAt: time.Now()}
	record := reporter.buildRecord(
		usage.Detail{},
		true,
		errors.New(`{"error":{"message":"upstream quota exhausted"}}`),
	)

	if record.ErrorMessage != "upstream quota exhausted" {
		t.Fatalf("error message = %q, want upstream quota exhausted", record.ErrorMessage)
	}
}

func TestResolveUsageAPIKeyPrefersClientAPIKeyWhenPresent(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "upstream-key"},
	}

	apiKey := resolveUsageAPIKey(auth, "client-key")
	if apiKey != "client-key" {
		t.Fatalf("api key = %q, want client-key", apiKey)
	}
}

func TestNewExecutorUsageReporterIncludesExecutorType(t *testing.T) {
	reporter := NewExecutorUsageReporter(context.Background(), &TestUsageExecutor{}, "gpt-5.4", nil)

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Provider != "test-provider" {
		t.Fatalf("provider = %q, want %q", record.Provider, "test-provider")
	}
	if record.ExecutorType != "TestUsageExecutor" {
		t.Fatalf("executor type = %q, want %q", record.ExecutorType, "TestUsageExecutor")
	}
}

func TestUsageProviderReportsReasoningAsOutputDetail(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     bool
	}{
		{name: "openai", provider: " OpenAI ", want: true},
		{name: "codex", provider: "\tCodex\r\n", want: true},
		{name: "oauth", provider: "oauth", want: false},
		{name: "empty", provider: " ", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usageProviderReportsReasoningAsOutputDetail(tt.provider); got != tt.want {
				t.Fatalf("usageProviderReportsReasoningAsOutputDetail(%q) = %t, want %t", tt.provider, got, tt.want)
			}
		})
	}
}

func TestNormalizeReasoningEffortValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: " Medium ", want: "medium"},
		{input: "XHIGH", want: "xhigh"},
		{input: " Adaptive ", want: "adaptive"},
		{input: "DISABLED", want: "disabled"},
		{input: "Custom-Level", want: "custom-level"},
		{input: " ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := normalizeReasoningEffortValue(tt.input); got != tt.want {
				t.Fatalf("normalizeReasoningEffortValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeUsageDetailTotalForProviderMixedCaseProvider(t *testing.T) {
	detail := normalizeUsageDetailTotalForProvider(" OpenAI ", usage.Detail{
		InputTokens:     100,
		OutputTokens:    50,
		ReasoningTokens: 15,
	})
	if detail.TotalTokens != 150 {
		t.Fatalf("OpenAI total tokens = %d, want 150", detail.TotalTokens)
	}

	detail = normalizeUsageDetailTotalForProvider(" OAuth ", usage.Detail{
		InputTokens:     100,
		OutputTokens:    50,
		ReasoningTokens: 15,
	})
	if detail.TotalTokens != 165 {
		t.Fatalf("OAuth total tokens = %d, want 165", detail.TotalTokens)
	}
}

func BenchmarkUsageProviderReportsReasoningAsOutputDetail(b *testing.B) {
	for b.Loop() {
		if !usageProviderReportsReasoningAsOutputDetail(" OpenAI ") {
			b.Fatal("expected OpenAI provider to report reasoning as output")
		}
	}
}

func BenchmarkNormalizeReasoningEffortValue(b *testing.B) {
	for b.Loop() {
		if got := normalizeReasoningEffortValue(" Medium "); got != "medium" {
			b.Fatalf("normalizeReasoningEffortValue() = %q", got)
		}
	}
}

func TestFilterSSEUsageMetadataPreservesUnchangedAndTerminalPayloads(t *testing.T) {
	unchanged := []byte("event: message\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n")
	if got := FilterSSEUsageMetadata(unchanged); !bytes.Equal(got, unchanged) {
		t.Fatalf("unchanged payload changed:\n got %q\nwant %q", got, unchanged)
	}

	terminal := []byte("data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1}}\n\n")
	if got := FilterSSEUsageMetadata(terminal); !bytes.Equal(got, terminal) {
		t.Fatalf("terminal payload changed:\n got %q\nwant %q", got, terminal)
	}
}

func TestFilterSSEUsageMetadataRebuildsOnlyModifiedDataLines(t *testing.T) {
	payload := []byte("event: message\r\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1}}\r\n: keep\n\ndata: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2}}\n\n")

	got := FilterSSEUsageMetadata(payload)

	if bytes.Contains(got, []byte(`"usageMetadata":{"promptTokenCount":1}`)) {
		t.Fatalf("non-terminal usage metadata was not removed: %s", got)
	}
	if !bytes.Contains(got, []byte(`"cpaUsageMetadata":{"promptTokenCount":1}`)) {
		t.Fatalf("renamed usage metadata missing: %s", got)
	}
	if !bytes.Contains(got, []byte(`"usageMetadata":{"promptTokenCount":2}`)) {
		t.Fatalf("terminal usage metadata should remain: %s", got)
	}
	if !bytes.Contains(got, []byte("event: message\r\n")) || !bytes.Contains(got, []byte(": keep\n")) {
		t.Fatalf("non-data lines changed: %q", got)
	}
}

func TestStripUsageMetadataFromJSONRenamesResponseWrappedMetadata(t *testing.T) {
	payload := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":1}}}`)

	got, changed := StripUsageMetadataFromJSON(payload)

	if !changed {
		t.Fatal("expected response usage metadata to be renamed")
	}
	if bytes.Contains(got, []byte(`"usageMetadata"`)) {
		t.Fatalf("old usage metadata key remains: %s", got)
	}
	if !bytes.Contains(got, []byte(`"cpaUsageMetadata":{"promptTokenCount":1}`)) {
		t.Fatalf("renamed response usage metadata missing: %s", got)
	}
}

func BenchmarkFilterSSEUsageMetadataPassthrough(b *testing.B) {
	payload := []byte("event: message\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n")
	b.ReportAllocs()
	for b.Loop() {
		if got := FilterSSEUsageMetadata(payload); len(got) != len(payload) {
			b.Fatalf("payload length = %d, want %d", len(got), len(payload))
		}
	}
}

func BenchmarkFilterSSEUsageMetadataModified(b *testing.B) {
	payload := []byte("event: message\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1}}\n\n")
	b.ReportAllocs()
	for b.Loop() {
		if got := FilterSSEUsageMetadata(payload); bytes.Contains(got, []byte(`"usageMetadata"`)) {
			b.Fatal("usage metadata was not filtered")
		}
	}
}

func TestUsageReporterBuildRecordIncludesReasoningEffort(t *testing.T) {
	ctx := usage.WithReasoningEffort(context.Background(), "medium")
	reporter := NewUsageReporter(ctx, "openai", "gpt-5.4", nil)

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.ReasoningEffort != "medium" {
		t.Fatalf("reasoning effort = %q, want %q", record.ReasoningEffort, "medium")
	}
}

func TestUsageReporterBuildAdditionalModelRecordSkipsZeroTokens(t *testing.T) {
	reporter := &UsageReporter{
		provider:    "codex",
		model:       "gpt-5.4",
		requestedAt: time.Now(),
	}

	if _, ok := reporter.buildAdditionalModelRecord("gpt-image-2", usage.Detail{}); ok {
		t.Fatalf("expected all-zero token usage to be skipped")
	}
	if _, ok := reporter.buildAdditionalModelRecord("gpt-image-2", usage.Detail{InputTokens: 2}); !ok {
		t.Fatalf("expected non-zero input token usage to be recorded")
	}
	if _, ok := reporter.buildAdditionalModelRecord("gpt-image-2", usage.Detail{CachedTokens: 2}); !ok {
		t.Fatalf("expected non-zero cached token usage to be recorded")
	}
}

func TestResolveUsageAPIKeyFallsBackToUpstreamAPIKeyAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "upstream-key"},
	}

	apiKey := resolveUsageAPIKey(auth, "")
	if apiKey != "upstream-key" {
		t.Fatalf("api key = %q, want upstream-key", apiKey)
	}
}

func TestResolveUsageAPIKeyFallsBackToContextForOAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	apiKey := resolveUsageAPIKey(auth, "client-key")
	if apiKey != "client-key" {
		t.Fatalf("api key = %q, want client-key", apiKey)
	}
}

func TestExtractReasoningEffortFromPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "top level", payload: `{"reasoning_effort":"high"}`, want: "high"},
		{name: "responses", payload: `{"reasoning":{"effort":"xhigh"}}`, want: "xhigh"},
		{name: "output config", payload: `{"output_config":{"effort":"low"}}`, want: "low"},
		{name: "generation config level", payload: `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"medium"}}}`, want: "medium"},
		{name: "nested generation config level", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"adaptive"}}}}`, want: "adaptive"},
		{name: "thinking budget", payload: `{"thinking":{"budget_tokens":123}}`, want: "budget:123"},
		{name: "thinking type", payload: `{"thinking":{"type":"enabled"}}`, want: "enabled"},
		{name: "escaped key", payload: `{"reasoning\u005feffort":"high"}`, want: "high"},
		{name: "ordinary payload", payload: `{"model":"gpt-5.4","input":"hello"}`, want: ""},
		{name: "escaped ordinary payload", payload: `{"model":"gpt-5.4","input":"hello\\nworld"}`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractReasoningEffortFromPayload([]byte(tt.payload)); got != tt.want {
				t.Fatalf("extractReasoningEffortFromPayload() = %q, want %q", got, tt.want)
			}
		})
	}
}

var reasoningEffortBenchmarkSink string

func BenchmarkExtractReasoningEffortFromPayloadNoMarkers(b *testing.B) {
	payload := []byte(`{"model":"gpt-5.4","input":"` + strings.Repeat("ordinary request payload ", 256) + `"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		reasoningEffortBenchmarkSink = extractReasoningEffortFromPayload(payload)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type TestUsageExecutor struct{}

func (TestUsageExecutor) Identifier() string {
	return "test-provider"
}
