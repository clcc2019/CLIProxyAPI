package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

func performImagesEndpointRequest(t *testing.T, endpointPath string, contentType string, body io.Reader, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(endpointPath, handler)

	req := httptest.NewRequest(http.MethodPost, endpointPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

type imageNativeCaptureExecutor struct {
	calls         int
	streamCalls   int
	alt           string
	streamAlt     string
	sourceFormat  string
	streamFormat  string
	model         string
	streamModel   string
	payload       []byte
	streamPayload []byte
	streamChunks  [][]byte
}

func (e *imageNativeCaptureExecutor) Identifier() string { return "test-native-images-provider" }

func (e *imageNativeCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	e.alt = opts.Alt
	e.sourceFormat = opts.SourceFormat.String()
	e.model = req.Model
	e.payload = append(e.payload[:0], req.Payload...)
	return coreexecutor.Response{
		Payload: []byte(`{"created":123,"data":[{"b64_json":"native-image"}]}`),
		Headers: http.Header{"X-Native-Test": []string{"ok"}},
	}, nil
}

func (e *imageNativeCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if e.streamChunks == nil {
		return nil, errors.New("not implemented")
	}
	e.streamCalls++
	e.streamAlt = opts.Alt
	e.streamFormat = opts.SourceFormat.String()
	e.streamModel = req.Model
	e.streamPayload = append(e.streamPayload[:0], req.Payload...)

	out := make(chan coreexecutor.StreamChunk, len(e.streamChunks))
	go func() {
		defer close(out)
		for _, chunk := range e.streamChunks {
			out <- coreexecutor.StreamChunk{Payload: chunk}
		}
	}()
	return &coreexecutor.StreamResult{
		Headers: http.Header{"X-Native-Stream-Test": []string{"ok"}},
		Chunks:  out,
	}, nil
}

func (e *imageNativeCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *imageNativeCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *imageNativeCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestImagesGenerationsUsesOpenAICompatibleImageModelNatively(t *testing.T) {
	executor := &imageNativeCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "test-native-images-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{
		ID:   "test-native/gpt-image-2",
		Type: "openai-compatibility",
	}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"test-native/gpt-image-2","prompt":"draw a fox"}`)

	resp := performImagesEndpointRequest(t, "/v1/images/generations", "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if executor.alt != "images/generations" {
		t.Fatalf("alt = %q, want images/generations", executor.alt)
	}
	if executor.sourceFormat != "openai" {
		t.Fatalf("source format = %q, want openai", executor.sourceFormat)
	}
	if executor.model != "test-native/gpt-image-2" {
		t.Fatalf("model = %q", executor.model)
	}
	if gjson.GetBytes(executor.payload, "response_format").String() != "b64_json" {
		t.Fatalf("expected default response_format in native payload, got %s", string(executor.payload))
	}
	if strings.TrimSpace(resp.Body.String()) != `{"created":123,"data":[{"b64_json":"native-image"}]}` {
		t.Fatalf("body = %s", resp.Body.String())
	}
}

func TestImagesGenerationsStreamKeepsResponsesPathForOpenAICompatibleImageModel(t *testing.T) {
	executor := &imageNativeCaptureExecutor{
		streamChunks: [][]byte{
			[]byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"stream-image\",\"output_format\":\"png\"}]}}\n\n"),
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "test-stream-images-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: "test-native/gpt-image-2", Type: "openai-compatibility"},
		{ID: defaultImagesMainModel},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"test-native/gpt-image-2","prompt":"draw a fox","stream":true}`)

	resp := performImagesEndpointRequest(t, "/v1/images/generations", "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if executor.calls != 0 {
		t.Fatalf("native non-stream calls = %d, want 0", executor.calls)
	}
	if executor.streamCalls != 1 {
		t.Fatalf("stream calls = %d, want 1", executor.streamCalls)
	}
	if executor.streamModel != defaultImagesMainModel {
		t.Fatalf("stream model = %q, want %q", executor.streamModel, defaultImagesMainModel)
	}
	if executor.streamFormat != "openai-response" {
		t.Fatalf("stream source format = %q, want openai-response", executor.streamFormat)
	}
	if executor.streamAlt != "" {
		t.Fatalf("stream alt = %q, want empty", executor.streamAlt)
	}
	if !strings.Contains(resp.Body.String(), "event: image_generation.completed") {
		t.Fatalf("missing completed image event: %s", resp.Body.String())
	}
}

func TestOpenAICompatibleImageModelUsesRegistryType(t *testing.T) {
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient("test-image-model-openai-compat", "openai-compat-provider", []*registry.ModelInfo{{
		ID:   "registry-native/gpt-image-2",
		Type: "openai-compatibility",
	}})
	registryRef.RegisterClient("test-image-model-bare-openai-compat", "openai-compat-provider", []*registry.ModelInfo{{
		ID:   "gpt-image-2",
		Type: "openai-compatibility",
	}})
	registryRef.RegisterClient("test-image-model-codex", "codex", []*registry.ModelInfo{{
		ID:   "registry-codex/gpt-image-2",
		Type: "codex",
	}})
	t.Cleanup(func() {
		registryRef.UnregisterClient("test-image-model-openai-compat")
		registryRef.UnregisterClient("test-image-model-bare-openai-compat")
		registryRef.UnregisterClient("test-image-model-codex")
	})

	if !openAICompatibleImageModel("registry-native/gpt-image-2") {
		t.Fatal("expected openai-compatible image model")
	}
	if openAICompatibleImageModel("registry-codex/gpt-image-2") {
		t.Fatal("did not expect codex image model to be treated as openai-compatible")
	}
	if openAICompatibleImageModel("gpt-image-2") {
		t.Fatal("did not expect bare gpt-image-2 to be routed through native OpenAI-compatible path")
	}
}
