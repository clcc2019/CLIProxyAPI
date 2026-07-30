package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type codexRemoteModelsTestExecutor struct{}

func (codexRemoteModelsTestExecutor) Identifier() string { return "codex" }

func (codexRemoteModelsTestExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (codexRemoteModelsTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return &coreexecutor.StreamResult{}, nil
}

func (codexRemoteModelsTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (codexRemoteModelsTestExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (codexRemoteModelsTestExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	request := req.WithContext(ctx)
	if auth != nil && auth.Metadata != nil {
		if token, ok := auth.Metadata["access_token"].(string); ok && token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return http.DefaultClient.Do(request)
}

func TestRefreshCodexRemoteCatalogUsesAccountScopedModels(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if got := req.URL.Path; got != "/models" {
			t.Errorf("path = %q, want /models", got)
		}
		if got := req.URL.Query().Get("client_version"); got != misc.CodexCLIVersion {
			t.Errorf("client_version = %q, want %q", got, misc.CodexCLIVersion)
		}
		if got := req.Header.Get("ChatGPT-Account-ID"); got != "acct-test" {
			t.Errorf("ChatGPT-Account-ID = %q, want acct-test", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer access-test" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","display_name":"Account Sol","supports_parallel_tool_calls":false,"use_responses_lite":false}]}`))
	}))
	defer server.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRemoteModelsTestExecutor{})
	service := &Service{coreManager: manager}
	auth := &coreauth.Auth{
		ID:       "auth-remote-catalog",
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind":  "oauth",
			"base_url":   server.URL,
			"account_id": "acct-test",
		},
		Metadata: map[string]any{"access_token": "access-test"},
	}
	if err := service.refreshCodexRemoteCatalog(context.Background(), auth); err != nil {
		t.Fatalf("refreshCodexRemoteCatalog: %v", err)
	}
	if err := service.refreshCodexRemoteCatalog(context.Background(), auth); err != nil {
		t.Fatalf("cached refreshCodexRemoteCatalog: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 within cache TTL", got)
	}

	models := service.codexModelsFromRemoteCatalog(auth, registry.GetCodexProModels())
	var sol *ModelInfo
	for _, model := range models {
		if model != nil && model.ID == "gpt-5.6-sol" {
			sol = model
			break
		}
	}
	if sol == nil || sol.CodexCapabilities == nil {
		t.Fatalf("account model capabilities missing: %#v", sol)
	}
	if sol.CodexCapabilities.UseResponsesLite {
		t.Fatal("account catalog should disable responses_lite")
	}
}

func TestRefreshCodexRemoteCatalogUsesStandardOpenAIListForAPIKeyAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := req.URL.Path; got != "/models" {
			t.Errorf("path = %q, want /models", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.6-sol"},{"id":"custom-codex-model"}]}`))
	}))
	defer server.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRemoteModelsTestExecutor{})
	service := &Service{coreManager: manager}
	auth := &coreauth.Auth{
		ID:       "auth-api-key-catalog",
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind": "api_key",
			"api_key":   "sk-test",
			"base_url":  server.URL,
		},
	}
	if err := service.refreshCodexRemoteCatalog(context.Background(), auth); err != nil {
		t.Fatalf("refreshCodexRemoteCatalog: %v", err)
	}

	models := service.codexModelsFromRemoteCatalog(auth, registry.GetCodexProModels())
	ids := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = true
		}
	}
	if !ids["gpt-5.6-sol"] || !ids["custom-codex-model"] {
		t.Fatalf("remote API-key models should retain both discovered IDs: %#v", models)
	}
}

func TestCodexRemoteCatalogDoesNotCrossAuthSourceChanges(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		accountID := req.Header.Get("ChatGPT-Account-ID")
		if accountID == "acct-new" {
			http.Error(w, "temporary failure", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"old-account-only-model","use_responses_lite":true}]}`))
	}))
	defer server.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRemoteModelsTestExecutor{})
	service := &Service{coreManager: manager}
	auth := &coreauth.Auth{
		ID:       "auth-source-change",
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind":  "oauth",
			"base_url":   server.URL,
			"account_id": "acct-old",
		},
		Metadata: map[string]any{"access_token": "old-token"},
	}
	if err := service.refreshCodexRemoteCatalog(context.Background(), auth); err != nil {
		t.Fatalf("initial refreshCodexRemoteCatalog: %v", err)
	}

	changed := auth.Clone()
	changed.Attributes["account_id"] = "acct-new"
	changed.Metadata["access_token"] = "new-token"
	fallback := []*ModelInfo{{ID: "fallback-model"}}
	if got := service.codexModelsFromRemoteCatalog(changed, fallback); len(got) != 1 || got[0].ID != "fallback-model" {
		t.Fatalf("changed auth source reused old catalog: %#v", got)
	}
	if err := service.refreshCodexRemoteCatalog(context.Background(), changed); err == nil {
		t.Fatal("changed auth source refresh unexpectedly succeeded")
	}
	if got := service.codexModelsFromRemoteCatalog(changed, fallback); len(got) != 1 || got[0].ID != "fallback-model" {
		t.Fatalf("failed changed-source refresh reused old catalog: %#v", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after auth source change", got)
	}
}

func TestRefreshCodexRemoteCatalogRefetchesExpiredEntry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"models":[{"slug":"ttl-model","use_responses_lite":true}]}`))
	}))
	defer server.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRemoteModelsTestExecutor{})
	service := &Service{coreManager: manager}
	auth := &coreauth.Auth{
		ID:         "auth-expired-catalog",
		Provider:   "codex",
		Attributes: map[string]string{"auth_kind": "oauth", "base_url": server.URL, "account_id": "acct-ttl"},
		Metadata:   map[string]any{"access_token": "ttl-token"},
	}
	if err := service.refreshCodexRemoteCatalog(context.Background(), auth); err != nil {
		t.Fatalf("initial refreshCodexRemoteCatalog: %v", err)
	}
	cached, ok := service.codexRemoteCatalogs.Load(auth.ID)
	if !ok {
		t.Fatal("remote catalog was not cached")
	}
	entry := cached.(codexRemoteCatalogCacheEntry)
	entry.fetchedAt = time.Now().Add(-codexRemoteCatalogTTL)
	service.codexRemoteCatalogs.Store(auth.ID, entry)
	if err := service.refreshCodexRemoteCatalog(context.Background(), auth); err != nil {
		t.Fatalf("expired refreshCodexRemoteCatalog: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after cache expiry", got)
	}
}

func TestCodexModelsFromRemoteCatalogRejectsExpiredEntry(t *testing.T) {
	service := &Service{}
	auth := &coreauth.Auth{
		ID:       "auth-expired-catalog-read",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url":   "https://catalog.example.test",
			"account_id": "acct-expired-read",
		},
	}
	sourceKey := codexRemoteCatalogSourceKey(auth, codexServiceBaseURL(auth))
	service.codexRemoteCatalogs.Store(auth.ID, codexRemoteCatalogCacheEntry{
		payload:   []byte(`{"models":[{"slug":"stale-remote-model"}]}`),
		fetchedAt: time.Now().Add(-codexRemoteCatalogTTL),
		sourceKey: sourceKey,
	})
	fallback := []*ModelInfo{{ID: "fallback-model"}}

	models := service.codexModelsFromRemoteCatalog(auth, fallback)
	if len(models) != 1 || models[0] != fallback[0] {
		t.Fatalf("expired catalog models = %#v, want fallback %#v", models, fallback)
	}
}

func TestRefreshCodexRemoteCatalogCollapsesConcurrentRequests(t *testing.T) {
	const workers = 12
	var calls atomic.Int32
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		startedOnce.Do(func() { close(requestStarted) })
		<-releaseResponse
		_, _ = w.Write([]byte(`{"models":[{"slug":"singleflight-model","use_responses_lite":true}]}`))
	}))
	defer server.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRemoteModelsTestExecutor{})
	service := &Service{coreManager: manager}
	auth := &coreauth.Auth{
		ID:         "auth-concurrent-catalog",
		Provider:   "codex",
		Attributes: map[string]string{"auth_kind": "oauth", "base_url": server.URL, "account_id": "acct-concurrent"},
		Metadata:   map[string]any{"access_token": "concurrent-token"},
	}

	start := make(chan struct{})
	ready := make(chan struct{}, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			ready <- struct{}{}
			<-start
			errs <- service.refreshCodexRemoteCatalog(context.Background(), auth)
		}()
	}
	for i := 0; i < workers; i++ {
		<-ready
	}
	close(start)
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for catalog request")
	}
	// Keep the leader in flight briefly so all workers reach the coalescing path.
	time.Sleep(50 * time.Millisecond)
	close(releaseResponse)
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent refreshCodexRemoteCatalog: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 coalesced request", got)
	}
}

func TestCodexResponseModelsETagRefreshesCatalogOnlyWhenChanged(t *testing.T) {
	var calls atomic.Int32
	var catalogETag atomic.Value
	catalogETag.Store("catalog-v1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", req.URL.Path)
		}
		calls.Add(1)
		w.Header().Set(codexRemoteCatalogETagHeader, catalogETag.Load().(string))
		_, _ = w.Write([]byte(`{"models":[{"slug":"etag-catalog-model","use_responses_lite":true}]}`))
	}))
	defer server.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRemoteModelsTestExecutor{})
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-response-etag",
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind": "api_key",
			"api_key":   "test-key",
			"base_url":  server.URL,
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	service := &Service{coreManager: manager}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })

	service.observeCodexResponseMetadata(context.Background(), auth, runtimeexecutor.CodexResponseMetadata{ModelsETag: "catalog-v1"})
	waitForCodexRemoteCatalogCalls(t, &calls, 1)
	entry := loadCodexRemoteCatalogEntry(t, service, auth.ID)
	if entry.etag != "catalog-v1" {
		t.Fatalf("cached ETag = %q, want catalog-v1", entry.etag)
	}
	if ids := registry.GetGlobalRegistry().GetModelIDsForClient(auth.ID); !containsModelID(ids, "etag-catalog-model") {
		t.Fatalf("refreshed registry models = %#v, want etag-catalog-model", ids)
	}

	service.observeCodexResponseMetadata(context.Background(), auth, runtimeexecutor.CodexResponseMetadata{ModelsETag: "catalog-v1"})
	if got := calls.Load(); got != 1 {
		t.Fatalf("matching ETag upstream calls = %d, want 1", got)
	}

	catalogETag.Store("catalog-v2")
	service.observeCodexResponseMetadata(context.Background(), auth, runtimeexecutor.CodexResponseMetadata{ModelsETag: "catalog-v2"})
	waitForCodexRemoteCatalogCalls(t, &calls, 2)
	if entry := loadCodexRemoteCatalogEntry(t, service, auth.ID); entry.etag != "catalog-v2" {
		t.Fatalf("refreshed cached ETag = %q, want catalog-v2", entry.etag)
	}
}

func waitForCodexRemoteCatalogCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := calls.Load(); got >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("catalog calls = %d, want at least %d", calls.Load(), want)
		case <-ticker.C:
		}
	}
}

func loadCodexRemoteCatalogEntry(t *testing.T, service *Service, authID string) codexRemoteCatalogCacheEntry {
	t.Helper()
	cached, ok := service.codexRemoteCatalogs.Load(authID)
	if !ok {
		t.Fatalf("catalog entry for %q was not cached", authID)
	}
	entry, ok := cached.(codexRemoteCatalogCacheEntry)
	if !ok {
		t.Fatalf("catalog entry type = %T, want codexRemoteCatalogCacheEntry", cached)
	}
	return entry
}

func containsModelID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
