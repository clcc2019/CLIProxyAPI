package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexResponseMetadataUsesOfficialHeaderSemantics(t *testing.T) {
	metadata := codexResponseMetadataFromHeaders(http.Header{
		"x-models-etag":        []string{"catalog-v2"},
		"openai-model":         []string{"gpt-5-safe"},
		"x-reasoning-included": []string{""},
	})
	if metadata.ModelsETag != "catalog-v2" {
		t.Fatalf("models etag = %q, want catalog-v2", metadata.ModelsETag)
	}
	if metadata.ServerModel != "gpt-5-safe" {
		t.Fatalf("server model = %q, want gpt-5-safe", metadata.ServerModel)
	}
	if !metadata.ReasoningIncluded {
		t.Fatal("reasoning included = false, want true when header is present")
	}
}

func TestCodexServerModelFromResponseDataPrefersCompletedResponseModel(t *testing.T) {
	data := []byte(`{"type":"response.completed","model":"gpt-5-requested","response":{"model":"gpt-5-safe"}}`)
	if got := codexServerModelFromResponseData(data); got != "gpt-5-safe" {
		t.Fatalf("server model = %q, want gpt-5-safe", got)
	}
}

func TestCodexServerModelFromResponseDataReadsOfficialStreamHeaderShapes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "ordinary Responses event",
			data: `{"type":"response.created","response":{"headers":{"OpenAI-Model":"gpt-5-safe"}}}`,
			want: "gpt-5-safe",
		},
		{
			name: "WebSocket metadata event",
			data: `{"type":"response.metadata","headers":{"x-openai-model":["gpt-5-safe"]}}`,
			want: "gpt-5-safe",
		},
		{
			name: "headers take precedence over echoed requested model",
			data: `{"type":"response.created","model":"gpt-5-requested","response":{"model":"gpt-5-requested","headers":{"openai-model":"gpt-5-safe"}}}`,
			want: "gpt-5-safe",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexServerModelFromResponseData([]byte(tt.data)); got != tt.want {
				t.Fatalf("server model = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCodexHTTPResponseObserverReceivesResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(codexHeaderModelsETag, "catalog-v2")
		w.Header().Set(codexHeaderOpenAIModel, "gpt-5-safe")
		w.Header().Set(codexHeaderReasoningIncluded, "")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	observed := make(chan CodexResponseMetadata, 1)
	executor := NewCodexExecutorWithResponseObserver(nil, func(_ context.Context, _ *cliproxyauth.Auth, metadata CodexResponseMetadata) {
		observed <- metadata
	})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/responses", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	resp, err := executor.doCodexHTTPRequest(context.Background(), &cliproxyauth.Auth{ID: "auth-http", Provider: "codex"}, codexPreparedRequest{httpReq: req})
	if err != nil {
		t.Fatalf("doCodexHTTPRequest() error = %v", err)
	}
	defer resp.Body.Close()

	select {
	case metadata := <-observed:
		if metadata.ModelsETag != "catalog-v2" || metadata.ServerModel != "gpt-5-safe" || !metadata.ReasoningIncluded {
			t.Fatalf("metadata = %#v", metadata)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HTTP response metadata")
	}
}

func TestCodexWebsocketResponseObserverReceivesHandshakeHeaders(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseHeaders := make(http.Header)
		responseHeaders.Set(codexHeaderModelsETag, "catalog-v3")
		responseHeaders.Set(codexHeaderOpenAIModel, "gpt-5-safe")
		responseHeaders.Set(codexHeaderReasoningIncluded, "true")
		conn, err := upgrader.Upgrade(w, r, responseHeaders)
		if err == nil {
			defer conn.Close()
			_, _, _ = conn.ReadMessage()
		}
	}))
	defer server.Close()

	observed := make(chan CodexResponseMetadata, 1)
	executor := NewCodexWebsocketsExecutorWithResponseObserver(nil, func(_ context.Context, _ *cliproxyauth.Auth, metadata CodexResponseMetadata) {
		observed <- metadata
	})
	prepared := &codexPreparedWebsocketRequest{
		wsURL:     "ws" + strings.TrimPrefix(server.URL, "http"),
		wsHeaders: make(http.Header),
		wsReqLog:  helps.UpstreamRequestLog{URL: server.URL, Method: "WEBSOCKET"},
	}
	attempt := executor.connectPreparedCodexWebsocket(context.Background(), &cliproxyauth.Auth{ID: "auth-ws", Provider: "codex"}, prepared)
	if attempt.err != nil {
		t.Fatalf("connectPreparedCodexWebsocket() error = %v", attempt.err)
	}
	defer attempt.conn.Close()

	select {
	case metadata := <-observed:
		if metadata.ModelsETag != "catalog-v3" || metadata.ServerModel != "gpt-5-safe" || !metadata.ReasoningIncluded {
			t.Fatalf("metadata = %#v", metadata)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for WebSocket response metadata")
	}
}
