package helps

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestStatusFromHomeErrorCode(t *testing.T) {
	tests := []struct {
		name string
		code string
		want int
	}{
		{name: "authentication error", code: " authentication_error ", want: http.StatusUnauthorized},
		{name: "unauthorized", code: "\tUnauthorized\r\n", want: http.StatusUnauthorized},
		{name: "model not found", code: "MODEL_NOT_FOUND", want: http.StatusNotFound},
		{name: "unknown", code: "rate_limited", want: http.StatusBadGateway},
		{name: "empty", code: "", want: http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusFromHomeErrorCode(tt.code); got != tt.want {
				t.Fatalf("statusFromHomeErrorCode(%q) = %d, want %d", tt.code, got, tt.want)
			}
		})
	}
}

func BenchmarkStatusFromHomeErrorCode(b *testing.B) {
	for b.Loop() {
		if got := statusFromHomeErrorCode(" Authentication_Error "); got != http.StatusUnauthorized {
			b.Fatalf("statusFromHomeErrorCode() = %d", got)
		}
	}
}

type fakeHomeRefreshClient struct {
	calls     atomic.Int32
	authIndex string
	raw       []byte
}

func (c *fakeHomeRefreshClient) HeartbeatOK() bool {
	return true
}

func (c *fakeHomeRefreshClient) GetRefreshAuth(_ context.Context, authIndex string) ([]byte, error) {
	c.calls.Add(1)
	c.authIndex = authIndex
	return c.raw, nil
}

func TestRefreshAuthViaHomeAcceptsAuthEnvelope(t *testing.T) {
	raw, errMarshal := json.Marshal(struct {
		Auth      cliproxyauth.Auth `json:"auth"`
		AuthIndex string            `json:"auth_index"`
	}{
		Auth: cliproxyauth.Auth{
			ID:       "home-auth-1",
			Provider: "home-test",
			Metadata: map[string]any{
				"access_token": "new-access-token",
			},
		},
		AuthIndex: "home-index-1",
	})
	if errMarshal != nil {
		t.Fatalf("marshal home envelope: %v", errMarshal)
	}

	client := &fakeHomeRefreshClient{raw: raw}
	oldCurrentHomeRefreshClient := currentHomeRefreshClient
	currentHomeRefreshClient = func() homeRefreshClient {
		return client
	}
	t.Cleanup(func() {
		currentHomeRefreshClient = oldCurrentHomeRefreshClient
	})

	cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
	auth := &cliproxyauth.Auth{
		ID:       "home-auth-1",
		Provider: "home-test",
		Index:    "home-index-1",
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
		},
	}

	updated, handled, err := RefreshAuthViaHome(context.Background(), cfg, auth)
	if err != nil {
		t.Fatalf("RefreshAuthViaHome error: %v", err)
	}
	if !handled {
		t.Fatal("RefreshAuthViaHome handled = false, want true")
	}
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("home refresh calls = %d, want 1", got)
	}
	if client.authIndex != "home-index-1" {
		t.Fatalf("home refresh auth_index = %q, want home-index-1", client.authIndex)
	}
	if updated == nil {
		t.Fatal("updated auth = nil")
	}
	if got := updated.Metadata["access_token"]; got != "new-access-token" {
		t.Fatalf("updated access_token = %q, want new-access-token", got)
	}
}
