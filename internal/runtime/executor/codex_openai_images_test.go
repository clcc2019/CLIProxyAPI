package executor

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorNativeImagesGenerationUsesResponsesPath(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotAccount string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("Chatgpt-Account-Id")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll(request body) error = %v", err)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"created_at":123,"output":[{"type":"image_generation_call","result":"img","output_format":"png","revised_prompt":"draw revised","size":"1024x1024"}],"tool_usage":{"image_gen":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}}` + "\n\n"))
	}))
	defer server.Close()

	compression := false
	executor := NewCodexExecutor(&config.Config{EnableRequestCompression: &compression})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "access-token",
			"account_id":   "acct_123",
		},
	}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-image-2",
		Payload: []byte(`{"model":"gpt-image-2","prompt":"draw","response_format":"b64_json","size":"1024x1024","style":"vivid"}`),
	}, cliproxyexecutor.Options{
		Alt:             "images/generations",
		OriginalRequest: []byte(`{"model":"gpt-image-2","prompt":"draw","response_format":"b64_json","size":"1024x1024","style":"vivid"}`),
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", gotPath)
	}
	if gotAuth != "Bearer access-token" {
		t.Fatalf("Authorization = %q, want bearer access token", gotAuth)
	}
	if gotAccount != "acct_123" {
		t.Fatalf("Chatgpt-Account-Id = %q, want acct_123", gotAccount)
	}
	if model := gjson.GetBytes(gotBody, "model").String(); model != codexOpenAIImagesMainModel {
		t.Fatalf("request model = %q, want %q; body=%s", model, codexOpenAIImagesMainModel, string(gotBody))
	}
	if instructions := gjson.GetBytes(gotBody, "instructions"); instructions.Exists() {
		t.Fatalf("instructions should be omitted for native image requests; body=%s", string(gotBody))
	}
	if toolType := gjson.GetBytes(gotBody, "tools.0.type").String(); toolType != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation; body=%s", toolType, string(gotBody))
	}
	if action := gjson.GetBytes(gotBody, "tools.0.action").String(); action != "generate" {
		t.Fatalf("tools.0.action = %q, want generate; body=%s", action, string(gotBody))
	}
	if style := gjson.GetBytes(gotBody, "tools.0.style").String(); style != "vivid" {
		t.Fatalf("tools.0.style = %q, want vivid; body=%s", style, string(gotBody))
	}
	if prompt := gjson.GetBytes(gotBody, "input.0.content.0.text").String(); prompt != "draw" {
		t.Fatalf("prompt = %q, want draw; body=%s", prompt, string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "data.0.b64_json").String(); got != "img" {
		t.Fatalf("response b64_json = %q, want img; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 3 {
		t.Fatalf("usage.total_tokens = %d, want 3; payload=%s", got, string(resp.Payload))
	}
}

func TestCodexExecutorNativeImagesRefreshesAfterUnauthorized(t *testing.T) {
	var (
		mu      sync.Mutex
		headers []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Get("Authorization"))
		attempt := len(headers)
		mu.Unlock()

		if attempt == 1 {
			http.Error(w, "expired token", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"created_at":123,"output":[{"type":"image_generation_call","result":"img","output_format":"png"}]}}` + "\n\n"))
	}))
	defer server.Close()

	ctx := cliproxyauth.WithRefreshCoordinator(context.Background(), func(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
		refreshed := auth.Clone()
		if refreshed.Metadata == nil {
			refreshed.Metadata = map[string]any{}
		}
		refreshed.Metadata["access_token"] = "new-access-token"
		refreshed.Metadata["refresh_token"] = "new-refresh-token"
		return refreshed, nil
	})

	compression := false
	executor := NewCodexExecutor(&config.Config{EnableRequestCompression: &compression})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "refresh-token",
		},
	}

	resp, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-image-2",
		Payload: []byte(`{"model":"gpt-image-2","prompt":"draw"}`),
	}, cliproxyexecutor.Options{
		Alt:             "images/generations",
		OriginalRequest: []byte(`{"model":"gpt-image-2","prompt":"draw"}`),
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "data.0.b64_json").String(); got != "img" {
		t.Fatalf("response b64_json = %q, want img; payload=%s", got, string(resp.Payload))
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer old-access-token", "Bearer new-access-token"}
	if len(headers) != len(want) {
		t.Fatalf("Authorization headers = %#v, want %#v", headers, want)
	}
	for i := range want {
		if headers[i] != want[i] {
			t.Fatalf("Authorization headers = %#v, want %#v", headers, want)
		}
	}
}

func TestCodexExecutorNativeImagesVariationUsesEditToolWithDefaultPrompt(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll(request body) error = %v", err)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"created_at":123,"output":[{"type":"image_generation_call","result":"variation","output_format":"png"}]}}` + "\n\n"))
	}))
	defer server.Close()

	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err = part.Write([]byte("fake-png")); err != nil {
		t.Fatalf("Write(image) error = %v", err)
	}
	if err = writer.WriteField("style", "vivid"); err != nil {
		t.Fatalf("WriteField(style) error = %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	compression := false
	executor := NewCodexExecutor(&config.Config{EnableRequestCompression: &compression})
	resp, err := executor.Execute(context.Background(), &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "sk-test",
		},
	}, cliproxyexecutor.Request{
		Model:   "gpt-image-2",
		Payload: raw.Bytes(),
	}, cliproxyexecutor.Options{
		Alt:             "images/variations",
		OriginalRequest: raw.Bytes(),
		SourceFormat:    sdktranslator.FromString("openai"),
		Headers: http.Header{
			"Content-Type": []string{writer.FormDataContentType()},
		},
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/variations",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if action := gjson.GetBytes(gotBody, "tools.0.action").String(); action != "edit" {
		t.Fatalf("tools.0.action = %q, want edit; body=%s", action, string(gotBody))
	}
	if style := gjson.GetBytes(gotBody, "tools.0.style").String(); style != "vivid" {
		t.Fatalf("tools.0.style = %q, want vivid; body=%s", style, string(gotBody))
	}
	if prompt := gjson.GetBytes(gotBody, "input.0.content.0.text").String(); prompt != codexOpenAIImageVariationPrompt {
		t.Fatalf("prompt = %q, want variation default; body=%s", prompt, string(gotBody))
	}
	if imageType := gjson.GetBytes(gotBody, "input.0.content.1.type").String(); imageType != "input_image" {
		t.Fatalf("input image type = %q, want input_image; body=%s", imageType, string(gotBody))
	}
	if imageURL := gjson.GetBytes(gotBody, "input.0.content.1.image_url").String(); imageURL == "" {
		t.Fatalf("image_url is empty; body=%s", string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "data.0.b64_json").String(); got != "variation" {
		t.Fatalf("response b64_json = %q, want variation; payload=%s", got, string(resp.Payload))
	}
}

func TestCodexExecutorNativeImagesStreamErrorsBeforeCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer server.Close()

	compression := false
	executor := NewCodexExecutor(&config.Config{EnableRequestCompression: &compression})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "sk-test",
		},
	}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-image-2",
		Payload: []byte(`{"model":"gpt-image-2","prompt":"draw"}`),
	}, cliproxyexecutor.Options{
		Alt:             "images/generations",
		OriginalRequest: []byte(`{"model":"gpt-image-2","prompt":"draw"}`),
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var gotErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
		}
	}
	if gotErr == nil {
		t.Fatal("expected stream error before completion")
	}
	if !strings.Contains(gotErr.Error(), "stream disconnected before completion") {
		t.Fatalf("stream error = %v, want disconnected before completion", gotErr)
	}
}
