package management

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFilesBareRequestDefaultsToBoundedPage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	for index := 0; index < defaultAuthFilesListPageSize+3; index++ {
		name := "runtime-" + strconv.Itoa(index) + ".json"
		auth := &coreauth.Auth{
			ID:       name,
			FileName: name,
			Provider: "codex",
			Attributes: map[string]string{
				"runtime_only": "true",
			},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %d: %v", index, err)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	response := performAuthFilesListRequest(t, h, "/v0/management/auth-files")
	var page struct {
		Files    []map[string]any `json:"files"`
		Total    int              `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
		HasMore  bool             `json:"has_more"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode paged auth list: %v", err)
	}
	if len(page.Files) != defaultAuthFilesListPageSize || page.Total != defaultAuthFilesListPageSize+3 ||
		page.Page != 1 || page.PageSize != defaultAuthFilesListPageSize || !page.HasMore {
		t.Fatalf("bare list pagination = page:%d page_size:%d files:%d total:%d has_more:%v", page.Page, page.PageSize, len(page.Files), page.Total, page.HasMore)
	}

	legacy := performAuthFilesListRequest(t, h, "/v0/management/auth-files?full=true")
	var full struct {
		Files []map[string]any `json:"files"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(legacy.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode full auth list: %v", err)
	}
	if len(full.Files) != defaultAuthFilesListPageSize+3 || full.Total != defaultAuthFilesListPageSize+3 {
		t.Fatalf("full list = files:%d total:%d, want %d", len(full.Files), full.Total, defaultAuthFilesListPageSize+3)
	}
}

func TestWriteAuthFilesJSONCompressesLargeResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := gin.H{"files": make([]gin.H, 0, 200), "total": 200}
	for index := 0; index < 200; index++ {
		payload["files"] = append(payload["files"].([]gin.H), gin.H{
			"name":            "auth-file.json",
			"status_message":  "refresh token invalid - re-login required",
			"recent_requests": make([]gin.H, 20),
		})
	}
	want, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal expected payload: %v", err)
	}

	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ctx.Request.Header.Set("Accept-Encoding", "br, gzip")
	writeAuthFilesJSON(ctx, http.StatusOK, payload)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("content encoding = %q, want gzip", response.Header().Get("Content-Encoding"))
	}
	if response.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("vary = %q, want Accept-Encoding", response.Header().Get("Vary"))
	}
	reader, err := gzip.NewReader(bytes.NewReader(response.Body.Bytes()))
	if err != nil {
		t.Fatalf("create gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip response: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close gzip reader: %v", err)
	}
	if !bytes.Equal(decoded, want) {
		t.Fatalf("decoded response differs from JSON payload")
	}
}

func performAuthFilesListRequest(t *testing.T, h *Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	h.ListAuthFiles(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("list %s status = %d, body=%s", target, response.Code, response.Body.String())
	}
	return response
}
