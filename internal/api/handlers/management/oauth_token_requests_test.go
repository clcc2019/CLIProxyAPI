package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSaveTokenRecordPublishesAuthToManagementListImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	fileStore := sdkAuth.NewFileTokenStore()
	fileStore.SetBaseDir(authDir)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = fileStore

	fileName := "oauth@example.com.json"
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		FileName: fileName,
		Metadata: map[string]any{
			"type":         "codex",
			"email":        "oauth@example.com",
			"access_token": "access-token",
		},
	}

	savedPath, errSave := h.saveTokenRecord(context.Background(), record)
	if errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}
	if savedPath != filepath.Join(authDir, fileName) {
		t.Fatalf("saved path = %q, want %q", savedPath, filepath.Join(authDir, fileName))
	}
	if _, errStat := os.Stat(savedPath); errStat != nil {
		t.Fatalf("saved auth file is missing: %v", errStat)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("ListAuthFiles status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
		Total int              `json:"total"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode auth file list: %v", errDecode)
	}
	if payload.Total != 1 || len(payload.Files) != 1 {
		t.Fatalf("auth file list = total:%d files:%d, want one entry; body=%s", payload.Total, len(payload.Files), rec.Body.String())
	}
	if got := payload.Files[0]["name"]; got != fileName {
		t.Fatalf("listed auth file name = %v, want %q", got, fileName)
	}
}

func TestSaveTokenRecordPublishesCompletePersistedTokenMetadata(t *testing.T) {
	authDir := t.TempDir()
	fileStore := sdkAuth.NewFileTokenStore()
	fileStore.SetBaseDir(authDir)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = fileStore

	fileName := "claude@example.com.json"
	_, errSave := h.saveTokenRecord(context.Background(), &coreauth.Auth{
		ID:       fileName,
		Provider: "claude",
		FileName: fileName,
		Storage: &claude.ClaudeTokenStorage{
			AccessToken:  "claude-access-token",
			RefreshToken: "claude-refresh-token",
			Email:        "claude@example.com",
		},
		Metadata: map[string]any{
			"email": "claude@example.com",
		},
	})
	if errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	published, ok := manager.GetByID(fileName)
	if !ok || published == nil {
		t.Fatalf("saved auth was not published to the manager")
	}
	if got := published.Metadata["access_token"]; got != "claude-access-token" {
		t.Fatalf("published access token = %v, want complete persisted token metadata", got)
	}
	if got := published.Metadata["refresh_token"]; got != "claude-refresh-token" {
		t.Fatalf("published refresh token = %v, want complete persisted token metadata", got)
	}
}
