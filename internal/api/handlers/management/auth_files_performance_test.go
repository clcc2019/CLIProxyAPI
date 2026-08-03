package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func BenchmarkListAuthFilesFilteredManager(b *testing.B) {
	gin.SetMode(gin.TestMode)
	dir := b.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	largeToken := strings.Repeat("token", 2048)
	for i := 0; i < 512; i++ {
		provider := "codex"
		if i%4 == 0 {
			provider = "claude"
		}
		name := fmt.Sprintf("auth-%04d.json", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(`{"type":"`+provider+`"}`), 0o600); err != nil {
			b.Fatalf("write auth file: %v", err)
		}
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:       name,
			FileName: name,
			Provider: provider,
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{
				"type":         provider,
				"access_token": largeToken,
			},
			Attributes: map[string]string{"path": path},
		}); err != nil {
			b.Fatalf("register auth: %v", err)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	requestTarget := "/v0/management/auth-files?type=claude&page=1&page_size=20&codex_subscription=skip"
	request := func() {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, requestTarget, nil)
		h.ListAuthFiles(ctx)
		if recorder.Code != http.StatusOK {
			b.Fatalf("ListAuthFiles status = %d", recorder.Code)
		}
	}
	request()

	b.ReportAllocs()
	for b.Loop() {
		request()
	}
}

func TestEnsureCodexSubscriptionSnapshotMetadataReusesCanonicalAuth(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"subscription_expires_at":           "2026-08-03T10:00:00Z",
			"chatgpt_subscription_active_until": "2026-08-03T10:00:00Z",
			"chatgpt_subscription_active_start": "2026-07-03T10:00:00Z",
			"subscription_active_start":         "2026-07-03T10:00:00Z",
		},
	}
	updated, changed := ensureCodexSubscriptionSnapshotMetadata(auth, auth.Metadata["subscription_expires_at"])
	if updated != auth || changed {
		t.Fatalf("canonical subscription metadata was cloned or changed: updated=%p auth=%p changed=%v", updated, auth, changed)
	}
}
