package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type fakeCodexDeviceLoginService struct {
	authorization *sdkAuth.CodexDeviceAuthorization
	startErr      error
	record        *coreauth.Auth
	completeErr   error
	completed     chan *sdkAuth.LoginOptions
}

func (f *fakeCodexDeviceLoginService) StartDeviceFlow(context.Context, *config.Config) (*sdkAuth.CodexDeviceAuthorization, error) {
	return f.authorization, f.startErr
}

func (f *fakeCodexDeviceLoginService) CompleteDeviceFlow(_ context.Context, _ *config.Config, _ *sdkAuth.CodexDeviceAuthorization, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
	if f.completed != nil {
		f.completed <- opts
	}
	return f.record, f.completeErr
}

func TestRequestCodexTokenDefaultsToDeviceFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	replaceOAuthSessionStoreForTest(t, newOAuthSessionStore(time.Minute))

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	t.Cleanup(h.Close)
	fileStore := sdkAuth.NewFileTokenStore()
	fileStore.SetBaseDir(authDir)
	h.tokenStore = fileStore

	completed := make(chan *sdkAuth.LoginOptions, 1)
	fake := &fakeCodexDeviceLoginService{
		authorization: &sdkAuth.CodexDeviceAuthorization{
			DeviceAuthID:    "private-device-id",
			UserCode:        "ABCD-1234",
			VerificationURL: "https://auth.openai.com/codex/device",
			PollInterval:    5 * time.Second,
			ExpiresAt:       time.Now().Add(time.Minute),
		},
		record: &coreauth.Auth{
			ID:       "codex-device@example.com.json",
			Provider: "codex",
			FileName: "codex-device@example.com.json",
			Metadata: map[string]any{
				"type":          "codex",
				"email":         "codex-device@example.com",
				"access_token":  "access-token",
				"refresh_token": "refresh-token",
			},
		},
		completed: completed,
	}
	replaceCodexDeviceLoginServiceForTest(t, func() codexDeviceLoginService { return fake })

	router := gin.New()
	router.GET("/codex-auth-url", h.RequestCodexToken)
	req := httptest.NewRequest(http.MethodGet, "/codex-auth-url?is_webui=true", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("RequestCodexToken status = %d, body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Mode            string `json:"mode"`
		URL             string `json:"url"`
		VerificationURL string `json:"verification_url"`
		UserCode        string `json:"user_code"`
		State           string `json:"state"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode device response: %v", err)
	}
	if response.Mode != sdkAuth.CodexLoginModeDevice || response.UserCode != "ABCD-1234" {
		t.Fatalf("device response = %#v", response)
	}
	if response.URL != fake.authorization.VerificationURL || response.VerificationURL != fake.authorization.VerificationURL {
		t.Fatalf("verification URL response = %#v", response)
	}
	if response.State == "" || response.ExpiresIn < 1 || response.Interval != 5 {
		t.Fatalf("device session response = %#v", response)
	}
	if strings.Contains(w.Body.String(), "private-device-id") {
		t.Fatal("response exposed private device_auth_id")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	select {
	case opts := <-completed:
		if opts == nil || !opts.NoBrowser || opts.Metadata[sdkAuth.CodexLoginModeMetadataKey] != sdkAuth.CodexLoginModeDevice {
			t.Fatalf("complete login options = %#v", opts)
		}
	case <-time.After(time.Second):
		t.Fatal("device completion did not start")
	}

	deadline := time.Now().Add(time.Second)
	for {
		_, _, done, ok := GetOAuthSessionDetails(response.State)
		if ok && done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device session did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := manager.GetByID(fake.record.ID); !ok {
		t.Fatal("saved device credential was not published to auth manager")
	}
	wantPath := filepath.Join(authDir, fake.record.FileName)
	if got, _ := manager.GetByID(fake.record.ID); got == nil || got.Attributes["path"] != wantPath {
		t.Fatalf("published auth path = %#v, want %q", got, wantPath)
	}
}

func TestRequestCodexTokenReportsDeviceStartFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	replaceCodexDeviceLoginServiceForTest(t, func() codexDeviceLoginService {
		return &fakeCodexDeviceLoginService{startErr: errors.New("device login disabled")}
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	t.Cleanup(h.Close)
	router := gin.New()
	router.GET("/codex-auth-url", h.RequestCodexToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/codex-auth-url", nil))

	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "device login disabled") {
		t.Fatalf("start failure response = %d %s", w.Code, w.Body.String())
	}
}

func TestCodexLoginModeFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		query   string
		want    string
		wantErr bool
	}{
		{want: sdkAuth.CodexLoginModeDevice},
		{query: "?mode=device", want: sdkAuth.CodexLoginModeDevice},
		{query: "?mode=browser", want: sdkAuth.CodexLoginModeBrowser},
		{query: "?login_mode=browser", want: sdkAuth.CodexLoginModeBrowser},
		{query: "?mode=unknown", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodGet, "/codex-auth-url"+tt.query, nil)
			got, err := codexLoginModeFromRequest(ctx)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("codexLoginModeFromRequest() = %q, %v; want %q, err=%t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestOAuthSessionStoreSupportsDeviceFlowTTL(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	before := time.Now()
	store.RegisterWithTTL("device-state", "codex", 15*time.Minute)
	session, ok := store.Get("device-state")
	if !ok {
		t.Fatal("device OAuth session was not registered")
	}
	if got := session.ExpiresAt.Sub(before); got < 14*time.Minute || got > 16*time.Minute {
		t.Fatalf("device OAuth session TTL = %s, want about 15m", got)
	}
}

func replaceCodexDeviceLoginServiceForTest(t *testing.T, factory func() codexDeviceLoginService) {
	t.Helper()
	original := newCodexDeviceLoginService
	newCodexDeviceLoginService = factory
	t.Cleanup(func() { newCodexDeviceLoginService = original })
}
