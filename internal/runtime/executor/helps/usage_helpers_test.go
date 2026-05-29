package helps

import (
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

func TestParseOpenAIStreamUsageZeroUsageObjectIgnored(t *testing.T) {
	line := []byte(`data: {"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	_, ok := ParseOpenAIStreamUsage(line)
	if ok {
		t.Fatal("expected all-zero usage chunk to be ignored")
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
		provider:    "kiro",
		model:       "kiro-model",
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
		Provider:   "gemini",
		Attributes: map[string]string{"api_key": "upstream-key"},
	}

	apiKey := resolveUsageAPIKey(auth, "client-key")
	if apiKey != "client-key" {
		t.Fatalf("api key = %q, want client-key", apiKey)
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
		Provider:   "gemini",
		Attributes: map[string]string{"api_key": "upstream-key"},
	}

	apiKey := resolveUsageAPIKey(auth, "")
	if apiKey != "upstream-key" {
		t.Fatalf("api key = %q, want upstream-key", apiKey)
	}
}

func TestResolveUsageAPIKeyFallsBackToContextForOAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "gemini-cli",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	apiKey := resolveUsageAPIKey(auth, "client-key")
	if apiKey != "client-key" {
		t.Fatalf("api key = %q, want client-key", apiKey)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
