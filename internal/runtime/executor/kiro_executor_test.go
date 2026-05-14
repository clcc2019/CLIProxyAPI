package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestBuildKiroPayloadForOpenAI(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hello"}],"max_tokens":128}`)

	payload, _ := buildKiroPayloadForFormat(body, "claude-sonnet-4.5", "", "AI_EDITOR", false, false, sdktranslator.FormatOpenAI, http.Header{})

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	state, ok := decoded["conversationState"].(map[string]any)
	if !ok {
		t.Fatalf("missing conversationState in payload: %s", string(payload))
	}
	current := state["currentMessage"].(map[string]any)
	userInput := current["userInputMessage"].(map[string]any)
	if got := userInput["modelId"]; got != "claude-sonnet-4.5" {
		t.Fatalf("modelId = %v, want claude-sonnet-4.5", got)
	}
	if got := userInput["origin"]; got != "AI_EDITOR" {
		t.Fatalf("origin = %v, want AI_EDITOR", got)
	}
	if content, _ := userInput["content"].(string); content == "" {
		t.Fatal("expected non-empty current message content")
	}
}

func TestKiroUsageRequestPayloadPrefersActualGeneratePayload(t *testing.T) {
	prepared := &kiroPreparedRequest{
		translated:   []byte(`{"messages":[{"role":"user","content":"intermediate"}]}`),
		firstPayload: []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"first"}}}}`),
	}
	actual := []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"actual"}}}}`)

	if got := string(kiroUsageRequestPayload(prepared, actual)); got != string(actual) {
		t.Fatalf("payload = %s, want actual generate payload", got)
	}
	if got := string(kiroUsageRequestPayload(prepared, nil)); got != string(prepared.firstPayload) {
		t.Fatalf("fallback payload = %s, want firstPayload", got)
	}
}

func TestBuildKiroPayloadForNativeKiroUsesCLIModelID(t *testing.T) {
	body := []byte(`{
		"user": {"local": true},
		"profileArn": "old-profile",
		"conversationState": {
			"currentMessage": {
				"userInputMessage": {
					"content": "hello",
					"modelId": "claude-opus-4.5",
					"origin": "KIRO_CLI"
				}
			},
			"history": [
				{
					"userInputMessage": {
						"content": "previous",
						"modelId": "claude-opus-4.5",
						"origin": "KIRO_CLI"
					}
				}
			]
		}
	}`)

	payload, _ := buildKiroPayloadForFormat(body, "claude-opus-4.5", "profile", "AI_EDITOR", false, false, sdktranslator.FormatKiro, http.Header{})

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if _, ok := decoded["user"]; ok {
		t.Fatalf("expected user field to be removed: %s", string(payload))
	}
	if got := decoded["profileArn"]; got != "profile" {
		t.Fatalf("profileArn = %v, want profile", got)
	}
	state := decoded["conversationState"].(map[string]any)
	current := state["currentMessage"].(map[string]any)
	userInput := current["userInputMessage"].(map[string]any)
	if got := userInput["modelId"]; got != "claude-opus-4.5" {
		t.Fatalf("current modelId = %v, want claude-opus-4.5", got)
	}
	if got := userInput["origin"]; got != "AI_EDITOR" {
		t.Fatalf("current origin = %v, want AI_EDITOR", got)
	}
	history := state["history"].([]any)
	historyInput := history[0].(map[string]any)["userInputMessage"].(map[string]any)
	if got := historyInput["modelId"]; got != "claude-opus-4.5" {
		t.Fatalf("history modelId = %v, want claude-opus-4.5", got)
	}
}

func TestBuildKiroPayloadForNativeKiroDropsProfileArnWhenNotEffective(t *testing.T) {
	body := []byte(`{
		"profileArn": "old-profile",
		"conversationState": {
			"currentMessage": {
				"userInputMessage": {
					"content": "hello"
				}
			}
		}
	}`)

	payload, _ := buildKiroPayloadForFormat(body, "claude-sonnet-4.6", "", "AI_EDITOR", false, false, sdktranslator.FormatKiro, http.Header{})

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if _, ok := decoded["profileArn"]; ok {
		t.Fatalf("expected profileArn to be removed when effective profile arn is empty: %s", string(payload))
	}
	state := decoded["conversationState"].(map[string]any)
	if got := state["chatTriggerType"]; got != "MANUAL" {
		t.Fatalf("chatTriggerType = %v, want MANUAL", got)
	}
	if got := state["agentTaskType"]; got != "vibe" {
		t.Fatalf("agentTaskType = %v, want vibe", got)
	}
	if convID, _ := state["conversationId"].(string); convID == "" {
		t.Fatalf("expected conversationId to be populated: %s", string(payload))
	}
	userInput := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if got := userInput["modelId"]; got != "claude-sonnet-4.6" {
		t.Fatalf("modelId = %v, want claude-sonnet-4.6", got)
	}
	if got := userInput["origin"]; got != "AI_EDITOR" {
		t.Fatalf("origin = %v, want AI_EDITOR", got)
	}
}

func TestKiroBuildRequestPayloadKeepsSocialProfileArnWithClientCredentials(t *testing.T) {
	profileArn := "arn:aws:codewhisperer:us-east-1:123:profile/social"
	auth := &cliproxyauth.Auth{
		ID:       "kiro-social",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"auth_method":   "kiro-cli-social",
			"provider":      "google",
			"client_id":     "client-id",
			"client_secret": "client-secret",
			"profile_arn":   profileArn,
		},
	}

	prepared, err := NewKiroExecutor(nil).buildRequestPayload(
		cliproxyexecutor.Request{
			Model:   "claude-sonnet-4.6",
			Payload: []byte(`{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Headers: http.Header{}},
		auth,
		profileArn,
		false,
	)
	if err != nil {
		t.Fatalf("buildRequestPayload() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(prepared.firstPayload, &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if got := decoded["profileArn"]; got != profileArn {
		t.Fatalf("profileArn = %v, want %s", got, profileArn)
	}
	if prepared.profileArn != profileArn {
		t.Fatalf("prepared.profileArn = %q, want %q", prepared.profileArn, profileArn)
	}
}

func TestKiroBuildRequestPayloadOmitsBuilderIDProfileArn(t *testing.T) {
	profileArn := "arn:aws:codewhisperer:us-east-1:123:profile/builder"
	auth := &cliproxyauth.Auth{
		ID:       "kiro-builder",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"auth_method":   "builder-id",
			"provider":      "AWS",
			"client_id":     "client-id",
			"client_secret": "client-secret",
			"profile_arn":   profileArn,
		},
	}

	prepared, err := NewKiroExecutor(nil).buildRequestPayload(
		cliproxyexecutor.Request{
			Model:   "claude-sonnet-4.6",
			Payload: []byte(`{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Headers: http.Header{}},
		auth,
		profileArn,
		false,
	)
	if err != nil {
		t.Fatalf("buildRequestPayload() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(prepared.firstPayload, &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if got, ok := decoded["profileArn"]; ok {
		t.Fatalf("profileArn = %v, want omitted", got)
	}
	if prepared.profileArn != "" {
		t.Fatalf("prepared.profileArn = %q, want empty", prepared.profileArn)
	}
}

func TestShouldRefreshKiroBeforeRequestUsesRefreshInterval(t *testing.T) {
	now := time.Date(2026, 5, 9, 6, 30, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-social",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":                     "kiro",
			"last_refresh":             now.Add(-6 * time.Minute).Format(time.RFC3339),
			"refresh_interval_seconds": 300,
			"expires_at":               now.Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	if !shouldRefreshKiroBeforeRequest(auth, now) {
		t.Fatal("expected refresh to be due after refresh_interval_seconds")
	}

	auth.Metadata["last_refresh"] = now.Add(-4 * time.Minute).Format(time.RFC3339)
	if shouldRefreshKiroBeforeRequest(auth, now) {
		t.Fatal("expected refresh not to be due before refresh_interval_seconds")
	}
}

func TestShouldRefreshKiroBeforeRequestDefaultsOldKiroAuths(t *testing.T) {
	now := time.Date(2026, 5, 9, 6, 30, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-social",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":         "kiro",
			"last_refresh": now.Add(-31 * time.Minute).Format(time.RFC3339),
			"expires_at":   now.Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	if !shouldRefreshKiroBeforeRequest(auth, now) {
		t.Fatal("expected legacy auth without stored interval to refresh after default max interval")
	}

	auth.NextRefreshAfter = now.Add(time.Minute)
	if !shouldRefreshKiroBeforeRequest(auth, now) {
		t.Fatal("expected stale last_refresh to bypass NextRefreshAfter gate")
	}

	auth.Metadata["last_refresh"] = now.Format(time.RFC3339)
	if shouldRefreshKiroBeforeRequest(auth, now) {
		t.Fatal("expected fresh last_refresh plus future NextRefreshAfter to gate request-time refresh")
	}

	auth.Metadata["expires_at"] = now.Add(time.Minute).Format(time.RFC3339)
	if !shouldRefreshKiroBeforeRequest(auth, now) {
		t.Fatal("expected expiring access token to bypass NextRefreshAfter gate")
	}
}

func TestKiroRequestTimeIntervalRefreshFailureUsesCurrentToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary refresh failure", http.StatusBadGateway)
	}))
	defer server.Close()

	oldEndpoint := kiroSocialRefreshEndpoint
	kiroSocialRefreshEndpoint = func(string) string { return server.URL }
	defer func() { kiroSocialRefreshEndpoint = oldEndpoint }()

	now := time.Now().UTC()
	auth := &cliproxyauth.Auth{
		ID:       "kiro-social",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":                     "kiro",
			"auth_method":              "kiro-cli-social",
			"provider":                 "google",
			"access_token":             "old-access",
			"refresh_token":            "refresh-token",
			"last_refresh":             now.Add(-6 * time.Minute).Format(time.RFC3339),
			"refresh_interval_seconds": 300,
			"expires_at":               now.Add(30 * time.Minute).Format(time.RFC3339),
		},
	}

	_, accessToken, _, err := NewKiroExecutor(nil).refreshIfKiroTokenExpiring(context.Background(), auth, "old-access", "")
	if err != nil {
		t.Fatalf("expected soft interval refresh failure to be ignored, got %v", err)
	}
	if accessToken != "old-access" {
		t.Fatalf("accessToken = %q, want old-access", accessToken)
	}
}

func TestKiroRequestTimeRequiredRefreshFailureReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refresh rejected", http.StatusUnauthorized)
	}))
	defer server.Close()

	oldEndpoint := kiroSocialRefreshEndpoint
	kiroSocialRefreshEndpoint = func(string) string { return server.URL }
	defer func() { kiroSocialRefreshEndpoint = oldEndpoint }()

	now := time.Now().UTC()
	auth := &cliproxyauth.Auth{
		ID:       "kiro-social",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"auth_method":   "kiro-cli-social",
			"provider":      "google",
			"access_token":  "old-access",
			"refresh_token": "refresh-token",
			"expires_at":    now.Add(time.Minute).Format(time.RFC3339),
		},
	}

	if _, _, _, err := NewKiroExecutor(nil).refreshIfKiroTokenExpiring(context.Background(), auth, "old-access", ""); err == nil {
		t.Fatal("expected required refresh failure to return error")
	}
}

func TestDoKiroRequestWithFallbackTriesNextEndpoint(t *testing.T) {
	executor := NewKiroExecutor(nil)
	rt := &kiroRoundTripRecorder{
		responses: []*http.Response{
			{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(nilReader{}),
			},
			{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(nilReader{}),
			},
		},
	}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))
	prepared := &kiroPreparedRequest{
		translated: []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"hi"}}}}`),
		from:       sdktranslator.FormatKiro,
		modelID:    "claude-sonnet-4.6",
		endpoints: []kiroEndpointConfig{
			{URL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse", Origin: "AI_EDITOR", Name: "AmazonQ"},
			{URL: "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse", Origin: "AI_EDITOR", AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse", Name: "CodeWhisperer"},
		},
		sourceBody: []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"hi"}}}}`),
		headers:    http.Header{},
	}

	resp, _, err := executor.doKiroRequestWithFallback(ctx, &cliproxyauth.Auth{}, prepared, "token")
	if err != nil {
		t.Fatalf("doKiroRequestWithFallback() error = %v", err)
	}
	defer closeKiroResponseBody(resp)
	if len(rt.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(rt.requests))
	}
	if got := rt.requests[1].Header.Get("X-Amz-Target"); got != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
		t.Fatalf("fallback X-Amz-Target = %q", got)
	}
	if got := rt.requests[1].Header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("Authorization = %q, want Bearer token", got)
	}
	if got := rt.requests[1].Header.Get("User-Agent"); got != kiroCLIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, kiroCLIUserAgent)
	}
	if got := rt.requests[1].Header.Get("x-amz-user-agent"); got != kiroCLIAmzAgent {
		t.Fatalf("x-amz-user-agent = %q, want %q", got, kiroCLIAmzAgent)
	}
}

func TestKiroSocialRequestUsesMachineIDUserAgent(t *testing.T) {
	machineID := strings.Repeat("a", 64)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-social",
		Provider: "kiro",
		Metadata: map[string]any{
			"auth_method": "kiro-cli-social",
			"provider":    "google",
			"machine_id":  machineID,
		},
	}
	req, err := http.NewRequest(http.MethodPost, "https://q.us-east-1.amazonaws.com/generateAssistantResponse", nil)
	if err != nil {
		t.Fatal(err)
	}

	applyKiroRuntimeIdentityHeaders(req, auth)

	if got := req.Header.Get("User-Agent"); !strings.Contains(got, "KiroIDE-"+kiroIDEVersion+"-"+machineID) {
		t.Fatalf("User-Agent = %q, want KiroIDE machine id suffix", got)
	}
	if got := req.Header.Get("x-amz-user-agent"); !strings.Contains(got, "KiroIDE "+kiroIDEVersion+" "+machineID) {
		t.Fatalf("x-amz-user-agent = %q, want KiroIDE machine id suffix", got)
	}
}

type kiroRoundTripRecorder struct {
	requests  []*http.Request
	responses []*http.Response
}

func (r *kiroRoundTripRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	r.requests = append(r.requests, cloned)
	if len(r.responses) == 0 {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(nilReader{}), Request: req}, nil
	}
	resp := r.responses[0]
	r.responses = r.responses[1:]
	resp.Request = req
	return resp, nil
}

type nilReader struct{}

func (nilReader) Read(_ []byte) (int, error) { return 0, io.EOF }

func TestKiroModelMappingAgentic(t *testing.T) {
	executor := NewKiroExecutor(nil)
	if got := executor.mapModelToKiro("claude-sonnet-4.5-agentic"); got != "claude-sonnet-4.5" {
		t.Fatalf("mapModelToKiro() = %q, want claude-sonnet-4.5", got)
	}
}

func TestKiroModelMappingDynamicIDs(t *testing.T) {
	executor := NewKiroExecutor(nil)
	tests := map[string]string{
		"claude-opus-4.7":        "claude-opus-4.7",
		"claude-sonnet-4.7":      "claude-sonnet-4.7",
		"deepseek-3.2":           "deepseek-3.2",
		"minimax-m2.5":           "minimax-m2.5",
		"glm-5":                  "glm-5",
		"qwen3-coder-next":       "qwen3-coder-next",
		"kiro-claude-opus-4-7":   "claude-opus-4.7",
		"kiro-claude-sonnet-4-7": "claude-sonnet-4.7",
		"kiro-gpt-3-5-turbo":     "gpt-3.5-turbo",
	}
	for model, want := range tests {
		if got := executor.mapModelToKiro(model); got != want {
			t.Fatalf("mapModelToKiro(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestExtractKiroUsageParsesOfficialTokenUsage(t *testing.T) {
	event := map[string]any{
		"messageMetadataEvent": map[string]any{
			"tokenUsage": map[string]any{
				"uncachedInputTokens":   float64(100),
				"cacheReadInputTokens":  float64(20),
				"cacheWriteInputTokens": float64(5),
				"outputTokens":          float64(30),
				"totalTokens":           float64(155),
			},
		},
	}

	detail := extractKiroUsage(event)
	if detail.InputTokens != 125 {
		t.Fatalf("input tokens = %d, want 125", detail.InputTokens)
	}
	if detail.CachedTokens != 20 {
		t.Fatalf("cached tokens = %d, want 20", detail.CachedTokens)
	}
	// cacheWriteInputTokens must now surface as a distinct counter so the
	// downstream Claude/OpenAI translators can emit cache_creation_input_tokens.
	// Previously this value was rolled into InputTokens and disappeared by the
	// time the event reached a client.
	if detail.CacheCreationTokens != 5 {
		t.Fatalf("cache creation tokens = %d, want 5", detail.CacheCreationTokens)
	}
	if detail.OutputTokens != 30 {
		t.Fatalf("output tokens = %d, want 30", detail.OutputTokens)
	}
	if detail.TotalTokens != 155 {
		t.Fatalf("total tokens = %d, want 155", detail.TotalTokens)
	}
}

func TestExtractKiroUsageParsesOpenAIStyleDetails(t *testing.T) {
	event := map[string]any{
		"usage": map[string]any{
			"prompt_tokens":     float64(70),
			"completion_tokens": float64(11),
			"total_tokens":      float64(81),
			"prompt_tokens_details": map[string]any{
				"cached_tokens": float64(13),
			},
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": float64(7),
			},
		},
	}

	detail := extractKiroUsage(event)
	if detail.InputTokens != 70 {
		t.Fatalf("input tokens = %d, want 70", detail.InputTokens)
	}
	if detail.CachedTokens != 13 {
		t.Fatalf("cached tokens = %d, want 13", detail.CachedTokens)
	}
	if detail.OutputTokens != 11 {
		t.Fatalf("output tokens = %d, want 11", detail.OutputTokens)
	}
	if detail.ReasoningTokens != 7 {
		t.Fatalf("reasoning tokens = %d, want 7", detail.ReasoningTokens)
	}
	if detail.TotalTokens != 81 {
		t.Fatalf("total tokens = %d, want 81", detail.TotalTokens)
	}
}

func TestKiroStreamToolUseEventFragmentsEmitToolUse(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(kiroTestEventStreamFrame("assistantResponseEvent", []byte(`{"assistantResponseEvent":{"content":"I need to inspect the workspace first."}}`)))
	stream.Write(kiroTestEventStreamFrame("toolUseEvent", []byte(`{"toolUseEvent":{"toolUseId":"toolu_123","name":"Bash"}}`)))
	stream.Write(kiroTestEventStreamFrame("toolUseEvent", []byte(`{"toolUseEvent":{"input":"{\"command\":\"pwd\""}}`)))
	stream.Write(kiroTestEventStreamFrame("toolUseEvent", []byte(`{"toolUseEvent":{"input":",\"description\":\"Show current directory\"}","stop":true}}`)))

	out := make(chan cliproxyexecutor.StreamChunk, 20)
	go func() {
		defer close(out)
		NewKiroExecutor(nil).streamToChannel(
			context.Background(),
			bytes.NewReader(stream.Bytes()),
			out,
			sdktranslator.FormatClaude,
			"claude-sonnet-4.5",
			[]byte(`{"stream":true}`),
			[]byte(`{"conversationState":{}}`),
			nil,
		)
	}()

	var combined strings.Builder
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}
	got := combined.String()
	if !strings.Contains(got, `"type":"tool_use"`) {
		t.Fatalf("stream did not emit tool_use block:\n%s", got)
	}
	if !strings.Contains(got, `"name":"Bash"`) {
		t.Fatalf("stream did not emit Bash tool name:\n%s", got)
	}
	if !strings.Contains(got, `"stop_reason":"tool_use"`) {
		t.Fatalf("stream did not stop with tool_use:\n%s", got)
	}
	if !strings.Contains(got, `\u0022command\u0022:\u0022pwd\u0022`) && !strings.Contains(got, `\"command\":\"pwd\"`) {
		t.Fatalf("stream did not include repaired tool input JSON:\n%s", got)
	}
	if strings.Contains(got, "}event:") {
		t.Fatalf("SSE frames are not delimited correctly:\n%s", got)
	}
	if !strings.Contains(got, "\n\n") {
		t.Fatalf("SSE frames are missing blank-line delimiters:\n%s", got)
	}
}

func TestKiroReadEventStreamMessageUnexpectedPreludeEOFIsError(t *testing.T) {
	msg, err := NewKiroExecutor(nil).readEventStreamMessage(bufioNewReader([]byte{0x00, 0x01}))
	if err == nil {
		t.Fatalf("expected short prelude error, got msg=%v", msg)
	}
}

func kiroTestEventStreamFrame(eventType string, payload []byte) []byte {
	headerName := []byte(":event-type")
	headerValue := []byte(eventType)
	headers := make([]byte, 0, 1+len(headerName)+1+2+len(headerValue))
	headers = append(headers, byte(len(headerName)))
	headers = append(headers, headerName...)
	headers = append(headers, 7)
	var valueLen [2]byte
	binary.BigEndian.PutUint16(valueLen[:], uint16(len(headerValue)))
	headers = append(headers, valueLen[:]...)
	headers = append(headers, headerValue...)

	totalLen := 12 + len(headers) + len(payload) + 4
	frame := make([]byte, totalLen)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLen))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	return frame
}

func bufioNewReader(data []byte) *bufio.Reader {
	return bufio.NewReader(bytes.NewReader(data))
}

func TestKiroRefreshUsesSocialEndpointWithoutClientCredentials(t *testing.T) {
	testKiroRefreshUsesSocialEndpoint(t, map[string]any{
		"client_id":      "",
		"client_secret":  "",
		"client_id_hash": "",
		"email":          "",
		"region":         "",
		"start_url":      "",
	})
}

func TestKiroRefreshUsesSocialEndpointForGoogleTokenWithClientCredentials(t *testing.T) {
	testKiroRefreshUsesSocialEndpoint(t, map[string]any{
		"provider":      "google",
		"auth_method":   "kiro-cli-social",
		"client_id":     "client-id",
		"client_secret": "client-secret",
	})
}

func TestKiroRefreshUsesSocialRegionEndpoint(t *testing.T) {
	var gotRegion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}`))
	}))
	defer server.Close()

	oldEndpoint := kiroSocialRefreshEndpoint
	kiroSocialRefreshEndpoint = func(region string) string {
		gotRegion = region
		return server.URL
	}
	defer func() { kiroSocialRefreshEndpoint = oldEndpoint }()

	auth := &cliproxyauth.Auth{
		ID:       "kiro-social-region",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"provider":      "google",
			"access_token":  "old-access",
			"refresh_token": "refresh-token",
			"region":        "eu-west-1",
		},
	}

	if _, err := NewKiroExecutor(nil).Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if gotRegion != "eu-west-1" {
		t.Fatalf("social refresh region = %q, want eu-west-1", gotRegion)
	}
}

func testKiroRefreshUsesSocialEndpoint(t *testing.T, extraMetadata map[string]any) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode refresh payload: %v", err)
		}
		if payload["refreshToken"] != "refresh-token" {
			t.Fatalf("refreshToken = %q, want refresh-token", payload["refreshToken"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","refreshToken":"new-refresh","profileArn":"arn:aws:codewhisperer:us-east-1:123:profile/social","expiresIn":3600}`))
	}))
	defer server.Close()

	oldEndpoint := kiroSocialRefreshEndpoint
	kiroSocialRefreshEndpoint = func(string) string { return server.URL }
	defer func() { kiroSocialRefreshEndpoint = oldEndpoint }()

	auth := &cliproxyauth.Auth{
		ID:       "kiro-social",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"access_token":  "old-access",
			"refresh_token": "refresh-token",
			"expires_at":    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	}
	for key, value := range extraMetadata {
		auth.Metadata[key] = value
	}

	updated, err := NewKiroExecutor(nil).Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := updated.Metadata["access_token"]; got != "new-access" {
		t.Fatalf("access_token = %v, want new-access", got)
	}
	if got := updated.Metadata["refresh_token"]; got != "new-refresh" {
		t.Fatalf("refresh_token = %v, want new-refresh", got)
	}
	if got := updated.Metadata["profile_arn"]; got != "arn:aws:codewhisperer:us-east-1:123:profile/social" {
		t.Fatalf("profile_arn = %v", got)
	}
	if updated.NextRefreshAfter.IsZero() {
		t.Fatal("expected NextRefreshAfter to be set")
	}
	refreshInterval, ok := updated.Metadata["refresh_interval_seconds"].(int)
	if !ok {
		t.Fatalf("refresh_interval_seconds = %#v, want int", updated.Metadata["refresh_interval_seconds"])
	}
	if refreshInterval < kiroauth.DefaultRefreshIntervalMinSeconds || refreshInterval > kiroauth.DefaultRefreshIntervalMaxSeconds {
		t.Fatalf("refresh_interval_seconds = %d, want %d-%d", refreshInterval, kiroauth.DefaultRefreshIntervalMinSeconds, kiroauth.DefaultRefreshIntervalMaxSeconds)
	}
	minNext := time.Now().Add(time.Duration(kiroauth.DefaultRefreshIntervalMinSeconds)*time.Second - time.Second)
	maxNext := time.Now().Add(time.Duration(kiroauth.DefaultRefreshIntervalMaxSeconds)*time.Second + time.Second)
	if updated.NextRefreshAfter.Before(minNext) || updated.NextRefreshAfter.After(maxNext) {
		t.Fatalf("NextRefreshAfter = %s, want within default refresh interval", updated.NextRefreshAfter)
	}
	for key, value := range extraMetadata {
		if raw, ok := value.(string); ok && strings.TrimSpace(raw) == "" {
			if _, exists := updated.Metadata[key]; exists {
				t.Fatalf("empty metadata key %q should be removed after refresh: %#v", key, updated.Metadata[key])
			}
		}
	}
}

func TestKiroRefreshUsesSSOGrantTypeForBuilderID(t *testing.T) {
	var gotPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode refresh payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-sso-access","refreshToken":"new-sso-refresh","expiresIn":3600}`))
	}))
	defer server.Close()

	oldEndpoint := kiroSSOTokenEndpoint
	kiroSSOTokenEndpoint = func(region string) string {
		if region != "us-east-1" {
			t.Fatalf("region = %q, want us-east-1", region)
		}
		return server.URL
	}
	defer func() { kiroSSOTokenEndpoint = oldEndpoint }()

	auth := &cliproxyauth.Auth{
		ID:       "kiro-builder",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"auth_method":   "builder-id",
			"provider":      "AWS",
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"client_id":     "client-id",
			"client_secret": "client-secret",
			"region":        "us-east-1",
		},
	}

	updated, err := NewKiroExecutor(nil).Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if gotPayload["grantType"] != "refresh_token" {
		t.Fatalf("grantType = %q, want refresh_token (payload=%v)", gotPayload["grantType"], gotPayload)
	}
	if gotPayload["clientId"] != "client-id" || gotPayload["clientSecret"] != "client-secret" || gotPayload["refreshToken"] != "old-refresh" {
		t.Fatalf("unexpected SSO payload: %v", gotPayload)
	}
	if got := updated.Metadata["access_token"]; got != "new-sso-access" {
		t.Fatalf("access_token = %v, want new-sso-access", got)
	}
	if got := updated.Metadata["refresh_token"]; got != "new-sso-refresh" {
		t.Fatalf("refresh_token = %v, want new-sso-refresh", got)
	}
}

func TestKiroSSORefreshRejectsMissingRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-sso-access","expiresIn":3600}`))
	}))
	defer server.Close()

	oldEndpoint := kiroSSOTokenEndpoint
	kiroSSOTokenEndpoint = func(string) string { return server.URL }
	defer func() { kiroSSOTokenEndpoint = oldEndpoint }()

	auth := &cliproxyauth.Auth{
		ID:       "kiro-builder-empty-refresh",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"auth_method":   "builder-id",
			"provider":      "AWS",
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"client_id":     "client-id",
			"client_secret": "client-secret",
			"region":        "us-east-1",
		},
	}

	_, err := NewKiroExecutor(nil).Refresh(context.Background(), auth)
	if err == nil {
		t.Fatal("Refresh() expected error when SSO refreshToken is missing, got nil")
	}
	if got, _ := auth.Metadata["refresh_token"].(string); got != "old-refresh" {
		t.Fatalf("original refresh_token = %q, want old-refresh", got)
	}
}

// TestKiroRefreshInvalidGrantIsPermanent verifies that an invalid_grant
// response from the social refresh endpoint surfaces a permanent error that
// both implements PermanentAuthError (for the conductor) and identifies as
// 401 (for the executor's isUnauthorizedStatusErr check).
func TestKiroRefreshInvalidGrantIsPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token has expired"}`))
	}))
	defer server.Close()

	oldEndpoint := kiroSocialRefreshEndpoint
	kiroSocialRefreshEndpoint = func(string) string { return server.URL }
	defer func() { kiroSocialRefreshEndpoint = oldEndpoint }()

	auth := &cliproxyauth.Auth{
		ID:       "kiro-social-invalid",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"access_token":  "old-access",
			"refresh_token": "dead-refresh",
			"provider":      "google",
		},
	}

	_, err := NewKiroExecutor(nil).Refresh(context.Background(), auth)
	if err == nil {
		t.Fatal("Refresh() expected error, got nil")
	}
	if !isKiroRefreshPermanent(err) {
		t.Fatalf("expected permanent refresh error, got %T: %v", err, err)
	}
	if !isUnauthorizedStatusErr(err) {
		t.Fatalf("expected error to report 401 via StatusCode(), got %T: %v", err, err)
	}
	var perm cliproxyauth.PermanentAuthError
	if !errors.As(err, &perm) || !perm.IsPermanentAuthError() {
		t.Fatalf("expected cliproxyauth.PermanentAuthError, got %T: %v", err, err)
	}
}

func TestKiroInvalidBearerMessageIsUnauthorized(t *testing.T) {
	invalidBearer := `{"message":"The bearer token included in the request is invalid.","reason":null}`
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"401 status", statusErr{code: http.StatusUnauthorized, msg: "unauthorized"}, true},
		{"403 status", statusErr{code: http.StatusForbidden, msg: "forbidden"}, true},
		{"400 invalid bearer body", statusErr{code: http.StatusBadRequest, msg: invalidBearer}, true},
		{"plain invalid bearer body", errors.New(invalidBearer), true},
		{"400 unrelated", statusErr{code: http.StatusBadRequest, msg: "invalid model"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnauthorizedStatusErr(tc.err); got != tc.want {
				t.Fatalf("isUnauthorizedStatusErr() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKiroRefreshClearsCredentialErrorState(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 15, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{
		ID:             "kiro-credential-state",
		Provider:       "kiro",
		Status:         cliproxyauth.StatusError,
		StatusMessage:  `{"message":"Bad credentials"}`,
		Unavailable:    true,
		LastError:      &cliproxyauth.Error{HTTPStatus: http.StatusUnauthorized, Message: `{"message":"Bad credentials"}`},
		NextRetryAfter: now.Add(30 * time.Minute),
		ModelStates: map[string]*cliproxyauth.ModelState{
			"claude-opus-4-7": {
				Status:         cliproxyauth.StatusError,
				StatusMessage:  `{"message":"Bad credentials"}`,
				Unavailable:    true,
				LastError:      &cliproxyauth.Error{HTTPStatus: http.StatusUnauthorized, Message: `{"message":"Bad credentials"}`},
				NextRetryAfter: now.Add(30 * time.Minute),
			},
			"claude-haiku-4-5": {
				Status:      cliproxyauth.StatusActive,
				Unavailable: false,
			},
		},
	}

	clearKiroCredentialErrorState(auth, now)

	if auth.Status != cliproxyauth.StatusActive || auth.Unavailable || auth.LastError != nil || !auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth credential state not cleared: status=%s unavailable=%v last=%v next=%s", auth.Status, auth.Unavailable, auth.LastError, auth.NextRetryAfter)
	}
	state := auth.ModelStates["claude-opus-4-7"]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Status != cliproxyauth.StatusActive || state.Unavailable || state.LastError != nil || !state.NextRetryAfter.IsZero() {
		t.Fatalf("model credential state not cleared: %#v", state)
	}
	if len(auth.ModelStates) == 0 {
		t.Fatal("model states should remain non-empty so manager update does not restore stale credential errors")
	}
}

// TestKiroSSORefreshInvalidGrantIsPermanent exercises the same classification
// for AWS SSO-OIDC responses; the refresh-token rotation semantics there make
// invalid_grant the single most common 401 cause in production.
func TestKiroSSORefreshInvalidGrantIsPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token has expired"}`))
	}))
	defer server.Close()

	oldEndpoint := kiroSSOTokenEndpoint
	kiroSSOTokenEndpoint = func(region string) string {
		if region != "us-east-1" {
			t.Fatalf("region = %q, want us-east-1", region)
		}
		return server.URL + "/token"
	}
	defer func() { kiroSSOTokenEndpoint = oldEndpoint }()

	auth := &cliproxyauth.Auth{
		ID:       "kiro-builder-invalid",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"auth_method":   "builder-id",
			"provider":      "AWS",
			"access_token":  "old-access",
			"refresh_token": "dead-refresh",
			"client_id":     "client-id",
			"client_secret": "client-secret",
			"region":        "us-east-1",
		},
	}

	_, err := NewKiroExecutor(nil).Refresh(context.Background(), auth)
	if err == nil {
		t.Fatal("Refresh() expected error, got nil")
	}
	if !isKiroRefreshPermanent(err) {
		t.Fatalf("expected permanent refresh error, got %T: %v", err, err)
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected error to report 401 via StatusCode(), got %T: %v", err, err)
	}
}

// TestKiroWrapAuthScoped429 verifies that a 429 from the CodeWhisperer
// request path is wrapped as an auth-scoped failure so the conductor
// suspends the whole auth (not just the model). Non-429 errors are left
// intact.
func TestKiroWrapAuthScoped429(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantScoped bool
		wantStatus int
	}{
		{"429 quota wrapped", statusErr{code: 429, msg: "quota exceeded"}, true, 429},
		{"429 usage limit wrapped", statusErr{code: 429, msg: `{"message":"AGENTIC_REQUEST usage limit reached"}`}, true, 429},
		{"429 credits wrapped", statusErr{code: 429, msg: `{"message":"Kiro credits exhausted"}`}, true, 429},
		{"429 insufficient model capacity not wrapped", statusErr{code: 429, msg: `{"message":"I am experiencing high traffic, please try again shortly.","reason":"INSUFFICIENT_MODEL_CAPACITY"}`}, false, 429},
		{"429 throttling not wrapped", statusErr{code: 429, msg: `{"__type":"ThrottlingException","message":"Rate exceeded"}`}, false, 429},
		{"429 too many requests not wrapped", statusErr{code: 429, msg: `{"message":"Too many requests, try again later"}`}, false, 429},
		{"401 not wrapped", statusErr{code: 401, msg: "unauthorized"}, false, 401},
		{"500 not wrapped", statusErr{code: 500, msg: "internal error"}, false, 500},
		{"non-status error", errors.New("network"), false, 0},
		{"nil returns nil", nil, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := wrapKiroAuthScoped(tc.err)
			if tc.err == nil {
				if out != nil {
					t.Fatalf("expected nil, got %v", out)
				}
				return
			}
			var scoped cliproxyauth.AuthScopedFailure
			isScoped := errors.As(out, &scoped) && scoped.IsAuthScopedFailure()
			if isScoped != tc.wantScoped {
				t.Fatalf("auth-scoped = %v, want %v (err=%v)", isScoped, tc.wantScoped, out)
			}
			if tc.wantStatus > 0 {
				var s interface{ StatusCode() int }
				if !errors.As(out, &s) || s.StatusCode() != tc.wantStatus {
					t.Fatalf("status code = %v, want %d", s, tc.wantStatus)
				}
			}
		})
	}
}
func TestClassifyKiroOAuthErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"invalid_grant", 400, `{"error":"invalid_grant","error_description":"expired"}`, true},
		{"invalid_client", 400, `{"error":"invalid_client"}`, true},
		{"unauthorized_client", 400, `{"error":"unauthorized_client"}`, true},
		{"access_denied", 403, `{"error":"access_denied"}`, true},
		{"camelCase errorCode", 400, `{"errorCode":"InvalidGrantException","message":"x"}`, true},
		{"transient slow_down", 400, `{"error":"slow_down"}`, false},
		{"authorization_pending", 400, `{"error":"authorization_pending"}`, false},
		{"5xx not classified", 500, `{"error":"invalid_grant"}`, false},
		{"empty body", 400, ``, false},
		{"non-json", 400, `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyKiroOAuthError(tc.status, []byte(tc.body))
			got := err != nil
			if got != tc.permanent {
				t.Fatalf("classifyKiroOAuthError(%d, %q) permanent=%v, want %v (err=%v)", tc.status, tc.body, got, tc.permanent, err)
			}
			if got && !isKiroRefreshPermanent(err) {
				t.Fatalf("classified error does not pass isKiroRefreshPermanent: %v", err)
			}
		})
	}
}

// TestKiroSocialRefreshPreservesRefreshTokenWhenOmitted ensures the Kiro
// social refresh endpoint can update access_token even when it does not
// return a rotated refreshToken.
func TestKiroSocialRefreshPreservesRefreshTokenWhenOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","refreshToken":"","expiresIn":3600}`))
	}))
	defer server.Close()

	oldEndpoint := kiroSocialRefreshEndpoint
	kiroSocialRefreshEndpoint = func(string) string { return server.URL }
	defer func() { kiroSocialRefreshEndpoint = oldEndpoint }()

	auth := &cliproxyauth.Auth{
		ID:       "kiro-social-empty-refresh",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"access_token":  "old-access",
			"refresh_token": "existing-refresh",
			"provider":      "google",
		},
	}

	updated, err := NewKiroExecutor(nil).Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := updated.Metadata["access_token"]; got != "new-access" {
		t.Fatalf("access_token = %v, want new-access", got)
	}
	if got := updated.Metadata["refresh_token"]; got != "existing-refresh" {
		t.Fatalf("refresh_token metadata = %v, want existing-refresh", got)
	}
	if got, _ := auth.Metadata["refresh_token"].(string); got != "existing-refresh" {
		t.Fatalf("original auth refresh_token = %q, want existing-refresh", got)
	}
}

func TestKiroAutoRefreshWritesBackSocialAccessTokenWithStableRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode refresh payload: %v", err)
		}
		if payload["refreshToken"] != "stable-refresh-token" {
			t.Fatalf("refreshToken = %q, want stable-refresh-token", payload["refreshToken"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"auto-new-access","expiresIn":3600}`))
	}))
	defer server.Close()

	oldEndpoint := kiroSocialRefreshEndpoint
	kiroSocialRefreshEndpoint = func(string) string { return server.URL }
	defer func() { kiroSocialRefreshEndpoint = oldEndpoint }()

	dir := t.TempDir()
	fileName := "kiro-auto-social.json"
	filePath := filepath.Join(dir, fileName)
	now := time.Now().UTC()
	raw := map[string]any{
		"type":                     "kiro",
		"auth_method":              "kiro-cli-social",
		"provider":                 "google",
		"client_id":                "",
		"client_secret":            "",
		"client_id_hash":           "",
		"email":                    "",
		"region":                   "",
		"start_url":                "",
		"profile_arn":              "",
		"access_token":             "old-access",
		"refresh_token":            "stable-refresh-token",
		"expires_at":               now.Add(time.Hour).Format(time.RFC3339),
		"last_refresh":             now.Add(-2 * time.Minute).Format(time.RFC3339),
		"refresh_interval_seconds": 1,
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := cliproxyauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(NewKiroExecutor(nil))
	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !manager.StartAutoRefresh(context.Background(), 10*time.Millisecond) {
		t.Fatal("expected Kiro provider-level auto-refresh to start")
	}
	defer manager.StopAutoRefresh()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		updatedRaw, errRead := os.ReadFile(filePath)
		if errRead != nil {
			t.Fatalf("read auth file: %v", errRead)
		}
		var updated map[string]any
		if err := json.Unmarshal(updatedRaw, &updated); err != nil {
			t.Fatalf("unmarshal updated auth file: %v", err)
		}
		if updated["access_token"] == "auto-new-access" {
			if got := updated["refresh_token"]; got != "stable-refresh-token" {
				t.Fatalf("refresh_token = %v, want stable-refresh-token", got)
			}
			if got, _ := updated["last_refresh"].(string); got == "" {
				t.Fatalf("last_refresh was not persisted: %#v", updated["last_refresh"])
			}
			for _, key := range []string{"client_id", "client_secret", "client_id_hash", "email", "region", "start_url", "profile_arn"} {
				if _, exists := updated[key]; exists {
					t.Fatalf("empty metadata key %q should not be persisted: %#v", key, updated[key])
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	updatedRaw, _ := os.ReadFile(filePath)
	t.Fatalf("auth file was not refreshed before deadline: %s", string(updatedRaw))
}

// TestShouldRefreshKiroWithSSO_Classification guards the routing table for
// the refresh dispatcher. Adding a new SSO or social method here requires
// touching isKiroSSOAuth / isKiroSocialAuth in kiro_executor.go; this test
// pins the expected decision for every combination we care about, including
// the ambiguous fallback.
func TestShouldRefreshKiroWithSSO_Classification(t *testing.T) {
	cases := []struct {
		name         string
		authMethod   string
		provider     string
		clientID     string
		clientSecret string
		wantSSO      bool
	}{
		{"no credentials -> social", "", "", "", "", false},
		{"only clientID -> social", "", "", "cid", "", false},
		{"builder-id with credentials -> sso", "builder-id", "aws", "cid", "secret", true},
		{"idc method -> sso", "idc", "", "cid", "secret", true},
		{"aws_sso_oidc -> sso", "aws_sso_oidc", "", "cid", "secret", true},
		{"kiro-cli-social -> social", "kiro-cli-social", "google", "cid", "secret", false},
		{"explicit social provider -> social", "", "google", "cid", "secret", false},
		{"kiro-ide-import with AWS provider -> sso", "kiro-ide-import", "aws", "cid", "secret", true},
		{"unknown method, known social provider -> social", "mystery", "github", "cid", "secret", false},
		{"unknown method, unknown provider with credentials -> sso", "mystery", "mystery", "cid", "secret", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRefreshKiroWithSSO(tc.authMethod, tc.provider, tc.clientID, tc.clientSecret)
			if got != tc.wantSSO {
				t.Fatalf("shouldRefreshKiroWithSSO(%q, %q) = %v, want %v", tc.authMethod, tc.provider, got, tc.wantSSO)
			}
		})
	}
}

// kiroCredentials used to treat Attributes as a whole-record fallback,
// dropping Attributes.profile_arn whenever Metadata already supplied the
// access_token. That silently sent empty-ARN requests to IdC / Identity
// Center endpoints and surfaced as transient 400s.
func TestKiroCredentials_IndependentMetadataAndAttributes(t *testing.T) {
	cases := []struct {
		name       string
		metadata   map[string]any
		attributes map[string]string
		wantToken  string
		wantArn    string
	}{
		{
			name: "metadata has token, attributes has profile_arn",
			metadata: map[string]any{
				"access_token": "meta-token",
			},
			attributes: map[string]string{
				"profile_arn": "arn:aws:codewhisperer:us-east-1:999:profile/attrs",
			},
			wantToken: "meta-token",
			wantArn:   "arn:aws:codewhisperer:us-east-1:999:profile/attrs",
		},
		{
			name:     "metadata empty, attributes has both",
			metadata: map[string]any{},
			attributes: map[string]string{
				"access_token": "attr-token",
				"profile_arn":  "arn:attr",
			},
			wantToken: "attr-token",
			wantArn:   "arn:attr",
		},
		{
			name: "metadata has both, attributes does not override",
			metadata: map[string]any{
				"access_token": "meta-token",
				"profile_arn":  "arn:meta",
			},
			attributes: map[string]string{
				"access_token": "should-not-win",
				"profile_arn":  "should-not-win",
			},
			wantToken: "meta-token",
			wantArn:   "arn:meta",
		},
		{
			name: "attributes profile_arn is trimmed",
			metadata: map[string]any{
				"access_token": "meta-token",
			},
			attributes: map[string]string{
				"profile_arn": "  arn:trimmed  ",
			},
			wantToken: "meta-token",
			wantArn:   "arn:trimmed",
		},
		{
			name:       "both nil",
			metadata:   nil,
			attributes: nil,
			wantToken:  "",
			wantArn:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				Metadata:   tc.metadata,
				Attributes: tc.attributes,
			}
			gotToken, gotArn := kiroCredentials(auth)
			if gotToken != tc.wantToken {
				t.Fatalf("access_token = %q, want %q", gotToken, tc.wantToken)
			}
			if gotArn != tc.wantArn {
				t.Fatalf("profile_arn = %q, want %q", gotArn, tc.wantArn)
			}
		})
	}
}

func TestParseKiroEventPayload_ClassifiesValidationError(t *testing.T) {
	parsed := parseKiroEventPayload([]byte(`{"_type":"ValidationException","message":"Improperly formed request: missing content"}`))
	if parsed.err == nil {
		t.Fatal("expected event error")
	}
	var status interface{ StatusCode() int }
	if !errors.As(parsed.err, &status) {
		t.Fatalf("expected status error, got %T", parsed.err)
	}
	if got := status.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
	if !strings.Contains(parsed.err.Error(), "Improperly formed request") {
		t.Fatalf("error = %q, want upstream message", parsed.err.Error())
	}
}

func TestValidateKiroGeneratePayloadRejectsMalformedPayload(t *testing.T) {
	err := validateKiroGeneratePayload([]byte(`{"conversationState":{"conversationId":"c","currentMessage":{"userInputMessage":{"modelId":"auto","origin":"AI_EDITOR"}}}}`))
	if err == nil {
		t.Fatal("expected malformed payload error")
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) {
		t.Fatalf("expected status error, got %T", err)
	}
	if got := status.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
	if !strings.Contains(err.Error(), "content is required") {
		t.Fatalf("error = %q, want content validation reason", err.Error())
	}
}

func TestDoKiroRequest_AttachesRetryAfterFromHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"quota exceeded"}`))
	}))
	t.Cleanup(server.Close)

	payload := []byte(`{"conversationState":{"conversationId":"c","currentMessage":{"userInputMessage":{"content":"hi","modelId":"auto","origin":"AI_EDITOR"}}}}`)
	_, err := NewKiroExecutor(nil).doKiroRequest(context.Background(), &cliproxyauth.Auth{ID: "auth-1"}, kiroEndpointConfig{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}, payload, "token")
	if err == nil {
		t.Fatal("expected upstream error")
	}
	var se statusErr
	if !errors.As(err, &se) {
		t.Fatalf("expected statusErr, got %T", err)
	}
	if se.retryAfter == nil || *se.retryAfter != 7*time.Second {
		t.Fatalf("retryAfter = %v, want 7s", se.retryAfter)
	}
}
