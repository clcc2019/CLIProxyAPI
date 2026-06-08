package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
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
	if isSupportedImagesModel("codex/grok-imagine-image") {
		t.Fatal("expected codex/grok-imagine-image to be rejected")
	}
	if !isSupportedImagesModel(" XAI/Grok-Imagine-Image ") {
		t.Fatal("expected mixed-case XAI image model to be supported")
	}
}

func TestImagesGenerationsUsesDefaultImageModelNatively(t *testing.T) {
	executor := &imageNativeCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "test-default-native-images-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{
		ID: "gpt-image-2",
	}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a fox"}`)

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
	if executor.model != "gpt-image-2" {
		t.Fatalf("model = %q, want gpt-image-2", executor.model)
	}
	if gjson.GetBytes(executor.payload, "response_format").String() != "b64_json" {
		t.Fatalf("expected default response_format in native payload, got %s", string(executor.payload))
	}
	if strings.TrimSpace(resp.Body.String()) != `{"created":123,"data":[{"b64_json":"native-image"}]}` {
		t.Fatalf("body = %s", resp.Body.String())
	}
}

func BenchmarkIsXAIImagesModel(b *testing.B) {
	for b.Loop() {
		if !isXAIImagesModel(" XAI/Grok-Imagine-Image ") {
			b.Fatal("expected XAI image model")
		}
	}
}

func TestImagesModelValidationAllowsOpenAICompatImageModels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-openai-compat-image-model-validation"
	modelRegistry.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{
		{ID: "compat-image-model", Object: "model", OwnedBy: "compat", Type: registry.OpenAIImageModelType},
		{ID: "compat-chat-model", Object: "model", OwnedBy: "compat", Type: "openai-compatibility"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	if !isSupportedImagesModel("compat-image-model") {
		t.Fatal("expected configured openai-compatibility image model to be supported")
	}
	if isSupportedImagesModel("compat-chat-model") {
		t.Fatal("expected non-image openai-compatibility model to be rejected")
	}
}

func TestBuildXAIImagesGenerationsRequest(t *testing.T) {
	rawJSON := []byte(`{"model":"xai/grok-imagine-image-quality","prompt":"abstract art","aspect_ratio":"landscape","resolution":"2k","n":2,"response_format":"url"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "xai/grok-imagine-image-quality", "url")

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image-quality" {
		t.Fatalf("model = %q, want grok-imagine-image-quality", got)
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "abstract art" {
		t.Fatalf("prompt = %q, want abstract art", got)
	}
	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "16:9" {
		t.Fatalf("aspect_ratio = %q, want 16:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
	if got := gjson.GetBytes(req, "response_format").String(); got != "url" {
		t.Fatalf("response_format = %q, want url", got)
	}
	if got := gjson.GetBytes(req, "n").Int(); got != 2 {
		t.Fatalf("n = %d, want 2", got)
	}
}

func TestXAIImagesOptionNormalizers(t *testing.T) {
	if got := xaiImagesAspectRatio(" Landscape ", "fallback"); got != "16:9" {
		t.Fatalf("xaiImagesAspectRatio() = %q, want 16:9", got)
	}
	if got := xaiImagesAspectRatio("3:4", "fallback"); got != "3:4" {
		t.Fatalf("xaiImagesAspectRatio(3:4) = %q, want 3:4", got)
	}
	if got := xaiImagesAspectRatio("wide", "fallback"); got != "fallback" {
		t.Fatalf("xaiImagesAspectRatio(wide) = %q, want fallback", got)
	}
	if got := xaiImagesAspectRatioFromSize(" 2048x2048 ", "fallback"); got != "1:1" {
		t.Fatalf("xaiImagesAspectRatioFromSize(2048x2048) = %q, want 1:1", got)
	}
	if got := xaiImagesAspectRatioFromSize("1536x1024", "fallback"); got != "3:2" {
		t.Fatalf("xaiImagesAspectRatioFromSize(1536x1024) = %q, want 3:2", got)
	}
	if got := xaiImagesResolution("\t2K\r\n", "1024x1024", "fallback"); got != "2k" {
		t.Fatalf("xaiImagesResolution(2K) = %q, want 2k", got)
	}
	if got := xaiImagesResolution("", "2048x2048", "fallback"); got != "2k" {
		t.Fatalf("xaiImagesResolution(2048 size) = %q, want 2k", got)
	}
	if got := xaiImagesResolution("4k", "1024x1024", "fallback"); got != "fallback" {
		t.Fatalf("xaiImagesResolution(4k) = %q, want fallback", got)
	}
}

func BenchmarkXAIImagesResolution(b *testing.B) {
	for b.Loop() {
		if got := xaiImagesResolution(" 2K ", "1024x1024", "fallback"); got != "2k" {
			b.Fatalf("xaiImagesResolution() = %q", got)
		}
	}
}

func TestBuildXAIImagesEditRequest(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"data:image/png;base64,AA==", "https://example.com/image.png"}, "b64_json", "3:2", "1k", 0)

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image" {
		t.Fatalf("model = %q, want grok-imagine-image", got)
	}
	if got := gjson.GetBytes(req, "images.0.type").String(); got != "image_url" {
		t.Fatalf("images.0.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "images.0.url").String(); got != "data:image/png;base64,AA==" {
		t.Fatalf("images.0.url = %q", got)
	}
	if got := gjson.GetBytes(req, "images.1.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("images.1.url = %q", got)
	}
	if gjson.GetBytes(req, "image").Exists() {
		t.Fatalf("multiple image edits must use images array: %s", string(req))
	}
}

func TestBuildXAIImagesEditRequestSingleImage(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"https://example.com/image.png"}, "url", "", "", 0)

	if got := gjson.GetBytes(req, "image.type").String(); got != "image_url" {
		t.Fatalf("image.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "image.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("image.url = %q", got)
	}
	if gjson.GetBytes(req, "images").Exists() {
		t.Fatalf("single image edit must use image object: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestPreservesStreamForStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":false}`), "upstream-image", true)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if !gjson.GetBytes(req, "stream").Bool() {
		t.Fatalf("stream flag missing: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestDropsStreamForNonStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":true}`), "upstream-image", false)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if gjson.GetBytes(req, "stream").Exists() {
		t.Fatalf("stream flag should be removed from non-streaming request: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesMultipartRequestPreservesStreamAndFileContentType(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "compat-image"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatalf("write stream field: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "edit"); errWrite != nil {
		t.Fatalf("write prompt field: %v", errWrite)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.png"))
	header.Set("Content-Type", "image/png")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read source form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove source form files: %v", errRemove)
		}
	}()

	out, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, "upstream-image", true)
	if errBuild != nil {
		t.Fatalf("buildOpenAICompatImagesMultipartRequest error: %v", errBuild)
	}
	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil {
		t.Fatalf("parse content type: %v", errParse)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	rewrittenReader := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	rewrittenForm, errRead := rewrittenReader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read rewritten form: %v", errRead)
	}
	defer func() {
		if errRemove := rewrittenForm.RemoveAll(); errRemove != nil {
			t.Fatalf("remove rewritten form files: %v", errRemove)
		}
	}()
	if got := rewrittenForm.Value["model"]; len(got) != 1 || got[0] != "upstream-image" {
		t.Fatalf("model values = %#v, want upstream-image", got)
	}
	if got := rewrittenForm.Value["stream"]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("stream values = %#v, want true", got)
	}
	if got := rewrittenForm.Value["prompt"]; len(got) != 1 || got[0] != "edit" {
		t.Fatalf("prompt values = %#v, want edit", got)
	}
	if got := rewrittenForm.File["image"]; len(got) != 1 || got[0].Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image headers = %#v, want image/png", got)
	}
}

func TestBuildImagesAPIResponseFromXAI(t *testing.T) {
	payload := []byte(`{"created":123,"data":[{"b64_json":"AA==","revised_prompt":"refined","mime_type":"image/png"}],"usage":{"total_tokens":0}}`)

	out, err := buildImagesAPIResponseFromXAI(payload, "b64_json")
	if err != nil {
		t.Fatalf("buildImagesAPIResponseFromXAI() error = %v", err)
	}

	if got := gjson.GetBytes(out, "created").Int(); got != 123 {
		t.Fatalf("created = %d, want 123", got)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "AA==" {
		t.Fatalf("data.0.b64_json = %q, want AA==", got)
	}
	if got := gjson.GetBytes(out, "data.0.revised_prompt").String(); got != "refined" {
		t.Fatalf("data.0.revised_prompt = %q, want refined", got)
	}
	if !gjson.GetBytes(out, "usage").Exists() {
		t.Fatalf("usage missing: %s", string(out))
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

func TestImagesGenerationsStreamUsesDefaultImageModelNatively(t *testing.T) {
	executor := &imageNativeCaptureExecutor{
		streamChunks: [][]byte{
			[]byte(`{"type":"image_generation.completed","b64_json":"stream-image"}`),
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "test-default-stream-images-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: "gpt-image-2"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, manager)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a fox","stream":true}`)

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
	if executor.streamModel != "gpt-image-2" {
		t.Fatalf("stream model = %q, want gpt-image-2", executor.streamModel)
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

func TestImagesEndpointContentTypeRouting(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	handler := NewOpenAIAPIHandler(base)

	resp := performImagesEndpointRequest(t, "/v1/images/edits", " Application/JSON; charset=utf-8 ", strings.NewReader(`{}`), handler.ImagesEdits)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("json status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "prompt is required") {
		t.Fatalf("json body = %s, want JSON edit validation error", resp.Body.String())
	}

	resp = performImagesEndpointRequest(t, "/v1/images/edits", " Text/Plain ", strings.NewReader(`{}`), handler.ImagesEdits)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("unsupported status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `unsupported Content-Type \"text/plain\"`) {
		t.Fatalf("unsupported body = %s, want normalized content type", resp.Body.String())
	}
}

func TestHasContentTypePrefix(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		prefix      string
		want        bool
	}{
		{name: "exact", contentType: "application/json", prefix: "application/json", want: true},
		{name: "mixed case with parameters", contentType: "Application/JSON; charset=utf-8", prefix: "application/json", want: true},
		{name: "multipart mixed case", contentType: "Multipart/Form-Data; boundary=abc", prefix: "multipart/form-data", want: true},
		{name: "short", contentType: "json", prefix: "application/json", want: false},
		{name: "different prefix", contentType: "text/plain", prefix: "application/json", want: false},
	}

	for i := range tests {
		if got := hasContentTypePrefix(tests[i].contentType, tests[i].prefix); got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func TestNormalizeImagesResponseFormat(t *testing.T) {
	tests := []struct {
		name           string
		responseFormat string
		want           string
	}{
		{name: "empty defaults to b64", responseFormat: "", want: "b64_json"},
		{name: "mixed case url", responseFormat: " URL ", want: "url"},
		{name: "b64 stays default", responseFormat: "B64_JSON", want: "b64_json"},
		{name: "unknown defaults to b64", responseFormat: "json", want: "b64_json"},
	}

	for i := range tests {
		if got := normalizeImagesResponseFormat(tests[i].responseFormat); got != tests[i].want {
			t.Fatalf("%s: got %q, want %q", tests[i].name, got, tests[i].want)
		}
	}
}

func TestParseBoolField(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		fallback bool
		want     bool
	}{
		{name: "empty fallback true", raw: "", fallback: true, want: true},
		{name: "mixed true", raw: " TrUe ", fallback: false, want: true},
		{name: "on", raw: "ON", fallback: false, want: true},
		{name: "one", raw: "1", fallback: false, want: true},
		{name: "mixed false", raw: "\tFaLsE\n", fallback: true, want: false},
		{name: "off", raw: "OFF", fallback: true, want: false},
		{name: "zero", raw: "0", fallback: true, want: false},
		{name: "unknown fallback", raw: "maybe", fallback: true, want: true},
	}

	for i := range tests {
		if got := parseBoolField(tests[i].raw, tests[i].fallback); got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func TestImagesEndpointAltMatchesMixedCasePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/Images/Edits", nil)

	if got := imagesEndpointAlt(ginCtx); got != "images/edits" {
		t.Fatalf("imagesEndpointAlt() = %q, want images/edits", got)
	}
}

func TestImagesEndpointPathContainsASCIIFold(t *testing.T) {
	if !asciifold.Contains("/v1/Images/Variations", "/images/variations") {
		t.Fatal("expected mixed-case path to match")
	}
	if asciifold.Contains("/v1/images", "/images/edits") {
		t.Fatal("expected missing path segment not to match")
	}
}

func TestMimeTypeFromOutputFormat(t *testing.T) {
	tests := []struct {
		name         string
		outputFormat string
		want         string
	}{
		{name: "empty defaults to png", outputFormat: "", want: "image/png"},
		{name: "mixed case png", outputFormat: " PNG ", want: "image/png"},
		{name: "jpg", outputFormat: "JPG", want: "image/jpeg"},
		{name: "jpeg", outputFormat: "\tJpeg\r\n", want: "image/jpeg"},
		{name: "webp", outputFormat: "WebP", want: "image/webp"},
		{name: "mime pass through", outputFormat: "image/avif", want: "image/avif"},
		{name: "unknown defaults to png", outputFormat: "bmp", want: "image/png"},
	}

	for i := range tests {
		if got := mimeTypeFromOutputFormat(tests[i].outputFormat); got != tests[i].want {
			t.Fatalf("%s: got %q, want %q", tests[i].name, got, tests[i].want)
		}
	}
}

func BenchmarkMimeTypeFromOutputFormat(b *testing.B) {
	for b.Loop() {
		if got := mimeTypeFromOutputFormat(" WebP "); got != "image/webp" {
			b.Fatalf("mimeTypeFromOutputFormat() = %q", got)
		}
	}
}

func BenchmarkHasContentTypePrefix(b *testing.B) {
	contentType := "Application/JSON; charset=utf-8"
	for b.Loop() {
		if !hasContentTypePrefix(contentType, "application/json") {
			b.Fatal("expected JSON content type")
		}
	}
}

func BenchmarkNormalizeImagesResponseFormat(b *testing.B) {
	for b.Loop() {
		if got := normalizeImagesResponseFormat(" URL "); got != "url" {
			b.Fatalf("normalizeImagesResponseFormat() = %q", got)
		}
	}
}

func BenchmarkParseBoolField(b *testing.B) {
	for b.Loop() {
		if !parseBoolField(" TrUe ", false) {
			b.Fatal("expected true")
		}
	}
}

func BenchmarkContainsASCIIFold(b *testing.B) {
	for b.Loop() {
		if !asciifold.Contains("/v1/Images/Variations", "/images/variations") {
			b.Fatal("expected path match")
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

func TestCollectImagesFromResponsesStreamRejectsNilChannels(t *testing.T) {
	out, errMsg := collectImagesFromResponsesStream(context.Background(), nil, nil, "b64_json")
	if out != nil {
		t.Fatalf("output = %s, want nil", out)
	}
	if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway || !errors.Is(errMsg.Error, errImageStreamNilChannels) {
		t.Fatalf("error = %#v, want nil image stream channel 502", errMsg)
	}
}

func TestCollectImagesFromResponsesStreamRejectsNilDataAfterErrorChannelCloses(t *testing.T) {
	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	out, errMsg := collectImagesFromResponsesStream(context.Background(), nil, errs, "b64_json")
	if out != nil {
		t.Fatalf("output = %s, want nil", out)
	}
	if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway || !errors.Is(errMsg.Error, errImageStreamNilChannels) {
		t.Fatalf("error = %#v, want nil image stream channel 502", errMsg)
	}
}

func TestCollectImagesFromResponsesStreamPrefersPendingErrorWhenDataCloses(t *testing.T) {
	data := make(chan []byte)
	close(data)
	wantErr := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("upstream quota"),
	}
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- wantErr
	close(errs)

	out, errMsg := collectImagesFromResponsesStream(context.Background(), data, errs, "b64_json")
	if out != nil {
		t.Fatalf("output = %s, want nil", out)
	}
	if errMsg != wantErr {
		t.Fatalf("error = %p, want %p", errMsg, wantErr)
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

func TestWaitImagesStreamExecutionWritesKeepAliveWhileStarting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)

	timing := newImageStreamTiming(10*time.Millisecond, 0)
	defer timing.Stop()

	wroteKeepAlive := make(chan struct{})
	var once sync.Once
	result, canceled := waitImagesStreamExecution(ginCtx, timing, func() {
		once.Do(func() { close(wroteKeepAlive) })
		timing.MarkWrite()
	}, func() imagesStreamExecutionResult {
		time.Sleep(50 * time.Millisecond)
		data := make(chan []byte)
		close(data)
		return imagesStreamExecutionResult{Data: data, UpstreamHeaders: http.Header{"X-Test": []string{"ok"}}}
	})

	if canceled {
		t.Fatal("waitImagesStreamExecution canceled unexpectedly")
	}
	select {
	case <-wroteKeepAlive:
	default:
		t.Fatal("expected keepalive while stream execution was starting")
	}
	if result.Data == nil {
		t.Fatal("result data channel is nil")
	}
	if got := result.UpstreamHeaders.Get("X-Test"); got != "ok" {
		t.Fatalf("upstream header = %q, want ok", got)
	}
}

func TestForwardNativeImagesStreamRejectsNilDataAfterErrorChannelCloses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)

	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	var eventName string
	var canceledErr error
	handler := &OpenAIAPIHandler{}
	handler.forwardNativeImagesStream(
		ginCtx,
		func(err error) { canceledErr = err },
		nil,
		errs,
		func(name string, _ []byte) { eventName = name },
		nil,
		nil,
	)

	if !errors.Is(canceledErr, errImageStreamNilChannels) {
		t.Fatalf("cancel err = %v, want nil image stream channels", canceledErr)
	}
	if eventName != "error" {
		t.Fatalf("event = %q, want error", eventName)
	}
}

func TestForwardNativeImagesStreamPrefersPendingErrorWhenDataCloses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)

	data := make(chan []byte)
	close(data)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("upstream quota")}
	close(errs)

	var eventName string
	var canceledErr error
	handler := &OpenAIAPIHandler{}
	handler.forwardNativeImagesStream(
		ginCtx,
		func(err error) { canceledErr = err },
		data,
		errs,
		func(name string, _ []byte) { eventName = name },
		nil,
		nil,
	)

	if canceledErr == nil || !strings.Contains(canceledErr.Error(), "upstream quota") {
		t.Fatalf("cancel err = %v, want upstream quota", canceledErr)
	}
	if eventName != "error" {
		t.Fatalf("event = %q, want error", eventName)
	}
}

func TestForwardImagesStreamRejectsNilDataAndErrorChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)

	var eventName string
	var canceledErr error
	handler := &OpenAIAPIHandler{}
	handler.forwardImagesStream(context.Background(), ginCtx, imageStreamForwardOptions{
		cancel:     func(err error) { canceledErr = err },
		writeEvent: func(name string, _ []byte) { eventName = name },
	})

	if !errors.Is(canceledErr, errImageStreamNilChannels) {
		t.Fatalf("cancel err = %v, want nil image stream channels", canceledErr)
	}
	if eventName != "error" {
		t.Fatalf("event = %q, want error", eventName)
	}
}

func TestForwardImagesStreamPrefersPendingErrorWhenDataCloses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)

	data := make(chan []byte)
	close(data)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("upstream quota")}
	close(errs)

	var eventName string
	var canceledErr error
	handler := &OpenAIAPIHandler{}
	handler.forwardImagesStream(context.Background(), ginCtx, imageStreamForwardOptions{
		cancel:     func(err error) { canceledErr = err },
		data:       data,
		errs:       errs,
		writeEvent: func(name string, _ []byte) { eventName = name },
	})

	if canceledErr == nil || !strings.Contains(canceledErr.Error(), "upstream quota") {
		t.Fatalf("cancel err = %v, want upstream quota", canceledErr)
	}
	if eventName != "error" {
		t.Fatalf("event = %q, want error", eventName)
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
