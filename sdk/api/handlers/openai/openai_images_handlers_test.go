package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
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

func TestImagesGenerationsStreamUsesOpenAICompatibleImageModelNatively(t *testing.T) {
	executor := &imageNativeCaptureExecutor{
		streamChunks: [][]byte{
			[]byte(`{"type":"image_generation.completed","b64_json":"stream-image"}`),
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
	if executor.streamModel != "test-native/gpt-image-2" {
		t.Fatalf("stream model = %q, want test-native/gpt-image-2", executor.streamModel)
	}
	if executor.streamFormat != "openai" {
		t.Fatalf("stream source format = %q, want openai", executor.streamFormat)
	}
	if executor.streamAlt != "images/generations" {
		t.Fatalf("stream alt = %q, want images/generations", executor.streamAlt)
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

func TestImagesEditsMultipartUsesOpenAICompatibleImageModelNatively(t *testing.T) {
	executor := &imageNativeCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "test-native-image-edits-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
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

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "test-native/gpt-image-2"); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if err := writer.WriteField("prompt", "replace background"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatalf("create image part: %v", err)
	}
	if _, err := part.Write([]byte("fake-png")); err != nil {
		t.Fatalf("write image part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler := NewOpenAIAPIHandler(base)
	resp := performImagesEndpointRequest(t, "/v1/images/edits", writer.FormDataContentType(), bytes.NewReader(body.Bytes()), handler.ImagesEdits)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if executor.alt != "images/edits" {
		t.Fatalf("alt = %q, want images/edits", executor.alt)
	}
	if executor.model != "test-native/gpt-image-2" {
		t.Fatalf("model = %q", executor.model)
	}
	if !bytes.Contains(executor.payload, []byte(`name="image"; filename="input.png"`)) {
		t.Fatalf("native payload did not preserve image file part: %s", string(executor.payload))
	}
}

func TestImagesVariationsMultipartUsesOpenAICompatibleImageModelNatively(t *testing.T) {
	executor := &imageNativeCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "test-native-image-variations-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
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

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "test-native/gpt-image-2"); err != nil {
		t.Fatalf("write model: %v", err)
	}
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatalf("create image part: %v", err)
	}
	if _, err := part.Write([]byte("fake-png")); err != nil {
		t.Fatalf("write image part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler := NewOpenAIAPIHandler(base)
	resp := performImagesEndpointRequest(t, "/v1/images/variations", writer.FormDataContentType(), bytes.NewReader(body.Bytes()), handler.ImagesVariations)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if executor.alt != "images/variations" {
		t.Fatalf("alt = %q, want images/variations", executor.alt)
	}
	if executor.model != "test-native/gpt-image-2" {
		t.Fatalf("model = %q", executor.model)
	}
	if !bytes.Contains(executor.payload, []byte(`name="image"; filename="input.png"`)) {
		t.Fatalf("native payload did not preserve image file part: %s", string(executor.payload))
	}
}

func TestImagesEndpointsReturnNotFoundWhenImageGenerationDisabled(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		DisableImageGeneration: internalconfig.DisableImageGenerationAll,
	}, coreauth.NewManager(nil, nil, nil))
	handler := NewOpenAIAPIHandler(base)

	tests := []struct {
		name        string
		path        string
		contentType string
		body        string
		handler     gin.HandlerFunc
	}{
		{
			name:        "generations",
			path:        "/v1/images/generations",
			contentType: "application/json",
			body:        `{"prompt":"draw"}`,
			handler:     handler.ImagesGenerations,
		},
		{
			name:    "edits",
			path:    "/v1/images/edits",
			handler: handler.ImagesEdits,
		},
		{
			name:    "variations",
			path:    "/v1/images/variations",
			handler: handler.ImagesVariations,
		},
	}

	for _, tc := range tests {
		resp := performImagesEndpointRequest(t, tc.path, tc.contentType, strings.NewReader(tc.body), tc.handler)
		if resp.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, body = %s", tc.name, resp.Code, resp.Body.String())
		}
	}
}

func TestCollectImagesFromResponsesStreamUsesOutputItemDoneFallback(t *testing.T) {
	data := make(chan []byte, 2)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"ZmFsbGJhY2s=\",\"output_format\":\"png\"}}\n\n")
	data <- []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[],\"tool_usage\":{\"image_gen\":{\"images\":1}}}}\n\n")
	close(data)
	close(errs)

	out, errMsg := collectImagesFromResponsesStream(context.Background(), data, errs, "b64_json")
	if errMsg != nil {
		t.Fatalf("collectImagesFromResponsesStream error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "ZmFsbGJhY2s=" {
		t.Fatalf("b64_json = %q, body = %s", got, string(out))
	}
}

func TestForwardNativeImagesStreamWritesKeepAlive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	timing := newImageStreamTiming(10*time.Millisecond, 0)
	defer timing.Stop()

	wroteKeepAlive := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	handler := &OpenAIAPIHandler{}

	go handler.forwardNativeImagesStream(
		ginCtx,
		func(err error) { done <- err },
		data,
		errs,
		func(string, []byte) { timing.MarkWrite() },
		timing,
		func() {
			once.Do(func() { close(wroteKeepAlive) })
			timing.MarkWrite()
		},
	)

	select {
	case <-wroteKeepAlive:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for image stream keepalive")
	}

	close(data)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancel err = %v, want nil", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for stream shutdown")
	}
}

func TestForwardNativeImagesStreamEmitsIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	timing := newImageStreamTiming(0, 10*time.Millisecond)
	defer timing.Stop()

	events := make(chan string, 1)
	done := make(chan error, 1)
	handler := &OpenAIAPIHandler{}

	go handler.forwardNativeImagesStream(
		ginCtx,
		func(err error) { done <- err },
		data,
		errs,
		func(eventName string, _ []byte) {
			events <- eventName
			timing.MarkWrite()
		},
		timing,
		nil,
	)

	select {
	case eventName := <-events:
		if eventName != "error" {
			t.Fatalf("event = %q, want error", eventName)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for idle timeout event")
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "upstream image stream idle") {
			t.Fatalf("cancel err = %v, want upstream image stream idle", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for idle timeout shutdown")
	}
}
