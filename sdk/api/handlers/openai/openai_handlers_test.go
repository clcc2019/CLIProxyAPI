package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type openAIExecutionSessionCaptureExecutor struct {
	mu                  sync.Mutex
	executionSessionIDs []string
}

func (e *openAIExecutionSessionCaptureExecutor) Identifier() string { return "codex" }

func (e *openAIExecutionSessionCaptureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *openAIExecutionSessionCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	if sessionID, ok := opts.Metadata[coreexecutor.ExecutionSessionMetadataKey].(string); ok {
		e.executionSessionIDs = append(e.executionSessionIDs, sessionID)
	}
	e.mu.Unlock()

	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[]}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *openAIExecutionSessionCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *openAIExecutionSessionCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *openAIExecutionSessionCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *openAIExecutionSessionCaptureExecutor) ExecutionSessionIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.executionSessionIDs...)
}

func TestChatCompletionsStreamingUsesExecutionSessionFromResponsesPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const model = "test-openai-chat-session-model"
	executor := &openAIExecutionSessionCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "chat-session-auth", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register(): %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)

	body := `{"model":"` + model + `","stream":true,"prompt_cache_key":"conv-chat-1","input":[{"role":"user","content":"hello"}]}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	h.ChatCompletions(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", recorder.Code, recorder.Body.String())
	}
	if got := executor.ExecutionSessionIDs(); len(got) != 1 || got[0] != "conv-chat-1" {
		t.Fatalf("execution session ids = %#v, want [conv-chat-1]", got)
	}
}
