package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexDeviceFlowIsDefault(t *testing.T) {
	tests := []struct {
		name string
		opts *LoginOptions
		want bool
	}{
		{name: "nil options", want: true},
		{name: "empty metadata", opts: &LoginOptions{Metadata: map[string]string{}}, want: true},
		{name: "device", opts: &LoginOptions{Metadata: map[string]string{CodexLoginModeMetadataKey: CodexLoginModeDevice}}, want: true},
		{name: "unknown defaults to device", opts: &LoginOptions{Metadata: map[string]string{CodexLoginModeMetadataKey: "future-mode"}}, want: true},
		{name: "browser override", opts: &LoginOptions{Metadata: map[string]string{CodexLoginModeMetadataKey: " BrOwSeR "}}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldUseCodexDeviceFlow(tt.opts); got != tt.want {
				t.Fatalf("shouldUseCodexDeviceFlow() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestCodexDeviceHTTPClientUsesSelectedOAuthProxy(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	client := codexDeviceHTTPClient(cfg, "http://pool.example.com:8081")
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected http.Transport, got %T", client.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://auth.openai.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://pool.example.com:8081" {
		t.Fatalf("proxy URL = %v, want selected OAuth proxy", proxyURL)
	}
}

func TestRequestCodexDeviceUserCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			http.Error(w, "unexpected content type", http.StatusBadRequest)
			return
		}
		var request codexDeviceUserCodeRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ClientID != codex.ClientID {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_auth_id":"device-1","user_code":"ABCD-1234","interval":"2","expires_in":600}`))
	}))
	defer server.Close()

	response, err := requestCodexDeviceUserCode(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("requestCodexDeviceUserCode() error = %v", err)
	}
	if response.DeviceAuthID != "device-1" || response.UserCode != "ABCD-1234" {
		t.Fatalf("device response = %#v", response)
	}
	if got := parseCodexDevicePollInterval(response.Interval); got != 2*time.Second {
		t.Fatalf("poll interval = %s, want 2s", got)
	}
	if got := parseCodexDeviceExpiresIn(response.ExpiresIn); got != 10*time.Minute {
		t.Fatalf("device expiry = %s, want 10m", got)
	}
}

func TestPollCodexDeviceTokenPendingThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request codexDeviceTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.DeviceAuthID != "device-1" || request.UserCode != "ABCD-1234" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authorization_code":"auth-code","code_verifier":"verifier"}`))
	}))
	defer server.Close()

	response, err := pollCodexDeviceToken(
		context.Background(),
		server.Client(),
		server.URL,
		"device-1",
		"ABCD-1234",
		time.Millisecond,
		time.Now().Add(time.Second),
	)
	if err != nil {
		t.Fatalf("pollCodexDeviceToken() error = %v", err)
	}
	if response.AuthorizationCode != "auth-code" || response.CodeVerifier != "verifier" {
		t.Fatalf("token response = %#v", response)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("poll calls = %d, want 2", got)
	}
}

func TestPollCodexDeviceTokenStopsOnAccessDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer server.Close()

	_, err := pollCodexDeviceToken(
		context.Background(),
		server.Client(),
		server.URL,
		"device-1",
		"ABCD-1234",
		time.Millisecond,
		time.Now().Add(time.Second),
	)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("pollCodexDeviceToken() error = %v, want access_denied", err)
	}
}

func TestPollCodexDeviceTokenHonorsContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pollCodexDeviceToken(
		ctx,
		server.Client(),
		server.URL,
		"device-1",
		"ABCD-1234",
		time.Second,
		time.Now().Add(time.Minute),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pollCodexDeviceToken() error = %v, want context.Canceled", err)
	}
}
