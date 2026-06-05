package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type memoryManagementCacheStore struct {
	mu    sync.Mutex
	items map[string][]byte
}

func newMemoryManagementCacheStore() *memoryManagementCacheStore {
	return &memoryManagementCacheStore{items: make(map[string][]byte)}
}

func (s *memoryManagementCacheStore) LoadCache(_ context.Context, namespace, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.items[namespace+"|"+key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), data...), true, nil
}

func (s *memoryManagementCacheStore) SaveCache(_ context.Context, namespace, key string, data []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[namespace+"|"+key] = append([]byte(nil), data...)
	return nil
}

func (s *memoryManagementCacheStore) DeleteCache(_ context.Context, namespace, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, namespace+"|"+key)
	return nil
}

func TestListAuthFilesSummaryLoadsCodexSubscriptionFromCacheStore(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	clearCodexSubscriptionCacheForTest(t)

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"email":        "cached@example.com",
			"access_token": "summary-token",
		},
		Attributes: map[string]string{"path": "/tmp/codex.json"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	store := newMemoryManagementCacheStore()
	entry := codexSubscriptionCacheEntry{
		found:     true,
		expiresAt: time.Now().Add(time.Hour),
		info: codexAccountSubscriptionInfo{
			PlanType:              "plus",
			Email:                 "cached@example.com",
			SubscriptionExpiresAt: "2026-06-01T00:00:00Z",
		},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal cache entry: %v", err)
	}
	if err := store.SaveCache(context.Background(), codexSubscriptionCacheNamespace, codexSubscriptionAuthCacheKey(auth), data, time.Hour); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetCacheStore(store)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?summary=true&page=1&pageSize=12", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	file := firstAuthFilePayload(t, rec.Body.Bytes())
	if got := file["subscription_expires_at"]; got != "2026-06-01T00:00:00Z" {
		t.Fatalf("subscription_expires_at = %#v", got)
	}
	if got := file["plan_type"]; got != "plus" {
		t.Fatalf("plan_type = %#v, want plus", got)
	}
}
