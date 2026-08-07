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

func TestAuthFilesListPayloadAcknowledgesPremiumFilter(t *testing.T) {
	payload := authFilesListPayload(nil, 0, authFilesListQuery{PremiumOnly: true}, nil)
	if applied, ok := payload["premium_only_applied"].(bool); !ok || !applied {
		t.Fatalf("premium_only_applied = %#v, want true", payload["premium_only_applied"])
	}
}

func TestAuthFileEntryHasPremiumPlan(t *testing.T) {
	tests := []struct {
		name string
		file gin.H
		want bool
	}{
		{name: "free with expiry", file: gin.H{"plan_type": "free", "subscription_expires_at": "2999-01-01T00:00:00Z"}},
		{name: "missing plan with expiry", file: gin.H{"subscription_expires_at": "2999-01-01T00:00:00Z"}},
		{name: "unsupported plan", file: gin.H{"plan_type": "team"}},
		{name: "plus", file: gin.H{"plan_type": "plus"}, want: true},
		{name: "k12", file: gin.H{"plan_type": "K12"}, want: true},
		{name: "pro", file: gin.H{"plan_type": "pro"}, want: true},
		{name: "pro lite alias", file: gin.H{"plan_type": "pro_lite"}, want: true},
		{name: "active premium", file: gin.H{"plan_type": "plus", "subscription_expires_at": "2999-01-01T00:00:00Z"}, want: true},
		{name: "expired premium", file: gin.H{"plan_type": "plus", "subscription_expires_at": "2000-01-01T00:00:00Z"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authFileEntryHasPremiumPlan(tt.file); got != tt.want {
				t.Fatalf("authFileEntryHasPremiumPlan() = %v, want %v", got, tt.want)
			}
		})
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
