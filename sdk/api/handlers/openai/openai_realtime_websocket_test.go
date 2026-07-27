package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestRealtimeWebsocketRelaysFramesThroughOpenAICompatibleCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamDone := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime" {
			upstreamDone <- &realtimeTestError{message: "upstream path = " + r.URL.Path}
			return
		}
		if model := r.URL.Query().Get("model"); model != "gpt-realtime-upstream" {
			upstreamDone <- &realtimeTestError{message: "upstream model = " + model}
			return
		}
		if authorization := r.Header.Get("Authorization"); authorization != "Bearer upstream-key" {
			upstreamDone <- &realtimeTestError{message: "upstream authorization = " + authorization}
			return
		}
		if safetyID := r.Header.Get("OpenAI-Safety-Identifier"); safetyID != "safe-user" {
			upstreamDone <- &realtimeTestError{message: "upstream safety identifier = " + safetyID}
			return
		}
		if feature := r.Header.Get("X-Provider-Feature"); feature != "enabled" {
			upstreamDone <- &realtimeTestError{message: "custom upstream header = " + feature}
			return
		}

		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			upstreamDone <- err
			return
		}
		defer func() { _ = conn.Close() }()

		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			upstreamDone <- err
			return
		}
		if messageType != websocket.TextMessage || string(payload) != `{"type":"session.update"}` {
			upstreamDone <- &realtimeTestError{message: "unexpected text frame"}
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.updated"}`)); err != nil {
			upstreamDone <- err
			return
		}

		messageType, payload, err = conn.ReadMessage()
		if err != nil {
			upstreamDone <- err
			return
		}
		if messageType != websocket.BinaryMessage || string(payload) != "audio-frame" {
			upstreamDone <- &realtimeTestError{message: "unexpected binary frame"}
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte("audio-ack")); err != nil {
			upstreamDone <- err
			return
		}
		upstreamDone <- nil
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "realtime-auth",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url":                  upstream.URL + "/v1",
			"api_key":                   "upstream-key",
			"provider_key":              "openai-compatibility",
			"header:X-Provider-Feature": "enabled",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.GET("/v1/realtime", handler.RealtimeWebsocket)
	proxy := httptest.NewServer(router)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/realtime?model=gpt-realtime-upstream"
	requestHeaders := http.Header{"OpenAI-Safety-Identifier": []string{"safe-user"}}
	downstreamDialer := websocket.Dialer{Subprotocols: []string{"realtime"}}
	conn, response, err := downstreamDialer.Dial(wsURL, requestHeaders)
	if err != nil {
		if response != nil {
			t.Fatalf("dial realtime websocket: %v (status %s)", err, response.Status)
		}
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if protocol := conn.Subprotocol(); protocol != "realtime" {
		t.Fatalf("downstream subprotocol = %q, want realtime", protocol)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update"}`)); err != nil {
		t.Fatalf("write text frame: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read text frame: %v", err)
	}
	if messageType != websocket.TextMessage || string(payload) != `{"type":"session.updated"}` {
		t.Fatalf("text frame = type %d payload %s", messageType, payload)
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("audio-frame")); err != nil {
		t.Fatalf("write binary frame: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	messageType, payload, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read binary frame: %v", err)
	}
	if messageType != websocket.BinaryMessage || string(payload) != "audio-ack" {
		t.Fatalf("binary frame = type %d payload %q", messageType, payload)
	}

	select {
	case err := <-upstreamDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream relay")
	}
}

func TestOpenAIRealtimeWebsocketURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		model   string
		want    string
	}{
		{
			name:    "official base without v1",
			baseURL: "https://api.openai.com",
			model:   "gpt-realtime-2.1",
			want:    "wss://api.openai.com/v1/realtime?model=gpt-realtime-2.1",
		},
		{
			name:    "custom v1 base preserves configured query",
			baseURL: "https://realtime.example.test/v1?api-version=2026-01-01",
			model:   "mapped-model",
			want:    "wss://realtime.example.test/v1/realtime?api-version=2026-01-01&model=mapped-model",
		},
		{
			name:    "already realtime websocket base",
			baseURL: "wss://realtime.example.test/v1/realtime",
			model:   "mapped-model",
			want:    "wss://realtime.example.test/v1/realtime?model=mapped-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := openAIRealtimeWebsocketURL(tt.baseURL, tt.model)
			if err != nil {
				t.Fatalf("openAIRealtimeWebsocketURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("openAIRealtimeWebsocketURL() = %q, want %q", got, tt.want)
			}
		})
	}

	if _, err := openAIRealtimeWebsocketURL("https://api.openai.com/v1#fragment", "gpt-realtime-2.1"); err == nil {
		t.Fatal("fragment base URL should be rejected")
	}
}

func TestRealtimeWebsocketRequiresModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	router := gin.New()
	router.GET("/v1/realtime", handler.RealtimeWebsocket)

	request := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

type realtimeTestError struct {
	message string
}

func (e *realtimeTestError) Error() string { return e.message }
