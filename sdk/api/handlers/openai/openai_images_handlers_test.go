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
