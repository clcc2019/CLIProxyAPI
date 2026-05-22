package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesRecentRequestsBuckets(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       "runtime-only-auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ginCtx.Request = req

	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok {
		t.Fatalf("expected files array, payload: %#v", payload)
	}
	if len(filesRaw) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(filesRaw))
	}

	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}

	if _, ok := fileEntry["success"].(float64); !ok {
		t.Fatalf("expected success number, got %#v", fileEntry["success"])
	}
	if _, ok := fileEntry["failed"].(float64); !ok {
		t.Fatalf("expected failed number, got %#v", fileEntry["failed"])
	}

	recentRaw, ok := fileEntry["recent_requests"].([]any)
	if !ok {
		t.Fatalf("expected recent_requests array, got %#v", fileEntry["recent_requests"])
	}
	if len(recentRaw) != 20 {
		t.Fatalf("expected 20 recent_requests buckets, got %d", len(recentRaw))
	}
	for idx, item := range recentRaw {
		bucket, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected bucket object at %d, got %#v", idx, item)
		}
		if _, ok := bucket["time"].(string); !ok {
			t.Fatalf("expected bucket time string at %d, got %#v", idx, bucket["time"])
		}
		if _, ok := bucket["success"].(float64); !ok {
			t.Fatalf("expected bucket success number at %d, got %#v", idx, bucket["success"])
		}
		if _, ok := bucket["failed"].(float64); !ok {
			t.Fatalf("expected bucket failed number at %d, got %#v", idx, bucket["failed"])
		}
	}
}

func TestListAuthFilesSummaryOmitsHeavyRuntimeFields(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       "summary-auth-1",
		FileName: "summary-auth-1.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
		LastError: &coreauth.Error{Code: "upstream", Message: "upstream failed"},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5": &coreauth.ModelState{Status: "error", StatusMessage: "blocked"},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "summary-auth-1", Provider: "codex", Model: "gpt-5", Success: true})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "summary-auth-1", Provider: "codex", Model: "gpt-5", Success: false})
	if gotAuth, ok := manager.GetByID("summary-auth-1"); ok {
		var successTotal int64
		var failedTotal int64
		for _, bucket := range gotAuth.RecentRequestsSnapshot(time.Now()) {
			successTotal += bucket.Success
			failedTotal += bucket.Failed
		}
		if successTotal != 1 || failedTotal != 1 {
			t.Fatalf("manager recent_requests totals = success=%d failed=%d, want 1/1", successTotal, failedTotal)
		}
	} else {
		t.Fatal("summary auth missing after MarkResult")
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?summary=1&page=1&page_size=10", nil)

	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok || len(filesRaw) != 1 {
		t.Fatalf("expected one file entry, payload: %#v", payload)
	}
	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}
	for _, key := range []string{"model_states", "last_error"} {
		if _, exists := fileEntry[key]; exists {
			t.Fatalf("summary response included %s: %#v", key, fileEntry[key])
		}
	}
	if _, ok := fileEntry["success"].(float64); !ok {
		t.Fatalf("expected success number in summary, got %#v", fileEntry["success"])
	}
	recentRaw, ok := fileEntry["recent_requests"].([]any)
	if !ok {
		t.Fatalf("expected recent_requests array in summary, got %#v", fileEntry["recent_requests"])
	}
	if len(recentRaw) != 20 {
		t.Fatalf("expected 20 recent_requests buckets in summary, got %d", len(recentRaw))
	}
	var successTotal float64
	var failedTotal float64
	for _, item := range recentRaw {
		bucket, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected recent_requests bucket object, got %#v", item)
		}
		successTotal += bucket["success"].(float64)
		failedTotal += bucket["failed"].(float64)
	}
	if successTotal != 1 || failedTotal != 1 {
		t.Fatalf("summary recent_requests totals = success=%v failed=%v, want 1/1", successTotal, failedTotal)
	}
}

func TestMergeAuthFileEntryGroupKeepsNonZeroRecentRequests(t *testing.T) {
	zeroBuckets := make([]coreauth.RecentRequestBucket, 20)
	activeBuckets := make([]coreauth.RecentRequestBucket, 20)
	activeBuckets[19] = coreauth.RecentRequestBucket{Time: "10:00-10:10", Success: 3, Failed: 1}

	merged := mergeAuthFileEntryGroup([]gin.H{
		{
			"name":            "auth.json",
			"source":          "file",
			"path":            "/tmp/auth.json",
			"recent_requests": zeroBuckets,
		},
		{
			"name":            "auth.json",
			"source":          "memory",
			"recent_requests": activeBuckets,
		},
	})

	recent, ok := merged["recent_requests"].([]coreauth.RecentRequestBucket)
	if !ok {
		t.Fatalf("merged recent_requests = %#v", merged["recent_requests"])
	}
	var successTotal int64
	var failedTotal int64
	for _, bucket := range recent {
		successTotal += bucket.Success
		failedTotal += bucket.Failed
	}
	if successTotal != 3 || failedTotal != 1 {
		t.Fatalf("merged recent_requests totals = success=%d failed=%d, want 3/1", successTotal, failedTotal)
	}
}
