package executor

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type nativeImagesRequestCapture struct {
	path          string
	authorization string
	userAgent     string
	contentType   string
	body          []byte
	readErr       error
}

func TestOpenAICompatExecutorNativeImagesGenerations(t *testing.T) {
	captured := make(chan nativeImagesRequestCapture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		captured <- nativeImagesRequestCapture{
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			userAgent:     r.Header.Get("User-Agent"),
			contentType:   r.Header.Get("Content-Type"),
			body:          body,
			readErr:       err,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Test", "native-images")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"img"}],"usage":{"total_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test-key",
	}}
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "upstream-image-model",
		Payload: []byte(`{"model":"client-image-model","prompt":"draw a cat","response_format":"b64_json"}`),
	}, cliproxyexecutor.Options{
		Alt:             "images/generations",
		OriginalRequest: []byte(`{"model":"client-image-model","prompt":"draw a cat","response_format":"b64_json"}`),
		SourceFormat:    sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if string(resp.Payload) != `{"created":123,"data":[{"b64_json":"img"}],"usage":{"total_tokens":1}}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
	if resp.Headers.Get("X-Upstream-Test") != "native-images" {
		t.Fatalf("missing upstream header")
	}

	var got nativeImagesRequestCapture
	select {
	case got = <-captured:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
	if got.readErr != nil {
		t.Fatalf("read request body: %v", got.readErr)
	}
	if got.path != "/v1/images/generations" {
		t.Fatalf("path = %q, want %q", got.path, "/v1/images/generations")
	}
	if got.authorization != "Bearer test-key" {
		t.Fatalf("authorization = %q", got.authorization)
	}
	if got.userAgent != "cli-proxy-openai-compat" {
		t.Fatalf("user agent = %q", got.userAgent)
	}
	if model := gjson.GetBytes(got.body, "model").String(); model != "upstream-image-model" {
		t.Fatalf("model = %q, want %q; body=%s", model, "upstream-image-model", string(got.body))
	}
	if prompt := gjson.GetBytes(got.body, "prompt").String(); prompt != "draw a cat" {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestOpenAICompatExecutorNativeImagesEditsMultipart(t *testing.T) {
	captured := make(chan nativeImagesRequestCapture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		captured <- nativeImagesRequestCapture{
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			userAgent:     r.Header.Get("User-Agent"),
			contentType:   r.Header.Get("Content-Type"),
			body:          body,
			readErr:       err,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"edited"}]}`))
	}))
	defer server.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "client-image-model"); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if err := writer.WriteField("prompt", "edit image"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err := part.Write([]byte("fake-png")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test-key",
	}}
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "upstream-image-model",
		Payload: body.Bytes(),
	}, cliproxyexecutor.Options{
		Alt:             "images/edits",
		OriginalRequest: body.Bytes(),
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestContentTypeMetadataKey: writer.FormDataContentType(),
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if string(resp.Payload) != `{"created":123,"data":[{"b64_json":"edited"}]}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}

	var got nativeImagesRequestCapture
	select {
	case got = <-captured:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
	if got.readErr != nil {
		t.Fatalf("read request body: %v", got.readErr)
	}
	if got.path != "/v1/images/edits" {
		t.Fatalf("path = %q, want /v1/images/edits", got.path)
	}
	if got.authorization != "Bearer test-key" {
		t.Fatalf("authorization = %q", got.authorization)
	}
	if !strings.HasPrefix(got.contentType, "multipart/form-data; boundary=") {
		t.Fatalf("content type = %q, want multipart/form-data boundary", got.contentType)
	}
	bodyText := string(got.body)
	if !strings.Contains(bodyText, `name="model"`) || !strings.Contains(bodyText, "upstream-image-model") {
		t.Fatalf("multipart body did not rewrite model: %s", bodyText)
	}
	if !strings.Contains(bodyText, `name="response_format"`) || !strings.Contains(bodyText, "b64_json") {
		t.Fatalf("multipart body did not add default response_format: %s", bodyText)
	}
	if !strings.Contains(bodyText, `name="image"; filename="input.png"`) {
		t.Fatalf("multipart body did not preserve image part: %s", bodyText)
	}
}

func TestOpenAICompatExecutorNativeImagesVariationsMultipart(t *testing.T) {
	captured := make(chan nativeImagesRequestCapture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		captured <- nativeImagesRequestCapture{
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			userAgent:     r.Header.Get("User-Agent"),
			contentType:   r.Header.Get("Content-Type"),
			body:          body,
			readErr:       err,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"variation"}]}`))
	}))
	defer server.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "client-image-model"); err != nil {
		t.Fatalf("write model: %v", err)
	}
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err := part.Write([]byte("fake-png")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test-key",
	}}
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "upstream-image-model",
		Payload: body.Bytes(),
	}, cliproxyexecutor.Options{
		Alt:             "images/variations",
		OriginalRequest: body.Bytes(),
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestContentTypeMetadataKey: writer.FormDataContentType(),
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if string(resp.Payload) != `{"created":123,"data":[{"b64_json":"variation"}]}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}

	var got nativeImagesRequestCapture
	select {
	case got = <-captured:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
	if got.readErr != nil {
		t.Fatalf("read request body: %v", got.readErr)
	}
	if got.path != "/v1/images/variations" {
		t.Fatalf("path = %q, want /v1/images/variations", got.path)
	}
	if got.authorization != "Bearer test-key" {
		t.Fatalf("authorization = %q", got.authorization)
	}
	if !strings.HasPrefix(got.contentType, "multipart/form-data; boundary=") {
		t.Fatalf("content type = %q, want multipart/form-data boundary", got.contentType)
	}
	bodyText := string(got.body)
	if !strings.Contains(bodyText, `name="model"`) || !strings.Contains(bodyText, "upstream-image-model") {
		t.Fatalf("multipart body did not rewrite model: %s", bodyText)
	}
	if !strings.Contains(bodyText, `name="response_format"`) || !strings.Contains(bodyText, "b64_json") {
		t.Fatalf("multipart body did not add default response_format: %s", bodyText)
	}
	if !strings.Contains(bodyText, `name="image"; filename="input.png"`) {
		t.Fatalf("multipart body did not preserve image part: %s", bodyText)
	}
}

func TestOpenAICompatExecutorNativeImagesGenerationsStream(t *testing.T) {
	captured := make(chan nativeImagesRequestCapture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		captured <- nativeImagesRequestCapture{
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			userAgent:     r.Header.Get("User-Agent"),
			contentType:   r.Header.Get("Content-Type"),
			body:          body,
			readErr:       err,
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: image_generation.completed\n"))
		_, _ = w.Write([]byte("data: {\"b64_json\":\"img\"}\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test-key",
	}}
	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "upstream-image-model",
		Payload: []byte(`{"model":"client-image-model","prompt":"draw","stream":true}`),
	}, cliproxyexecutor.Options{
		Alt:             "images/generations",
		OriginalRequest: []byte(`{"model":"client-image-model","prompt":"draw","stream":true}`),
		SourceFormat:    sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var chunks [][]byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		chunks = append(chunks, chunk.Payload)
	}
	if len(chunks) != 1 || gjson.GetBytes(chunks[0], "type").String() != "image_generation.completed" || gjson.GetBytes(chunks[0], "b64_json").String() != "img" {
		t.Fatalf("chunks = %q", chunks)
	}

	var got nativeImagesRequestCapture
	select {
	case got = <-captured:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
	if got.path != "/v1/images/generations" {
		t.Fatalf("path = %q, want /v1/images/generations", got.path)
	}
	if model := gjson.GetBytes(got.body, "model").String(); model != "upstream-image-model" {
		t.Fatalf("model = %q, want upstream-image-model; body=%s", model, string(got.body))
	}
	if format := gjson.GetBytes(got.body, "response_format").String(); format != "b64_json" {
		t.Fatalf("response_format = %q, want b64_json; body=%s", format, string(got.body))
	}
}
