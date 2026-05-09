package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type fakeKiroUsageClient struct {
	tokenData         *kiroauth.TokenData
	accessTokens      []string
	resolveProfileArn string
	errors            []error
	calls             int
}

func (f *fakeKiroUsageClient) GetUsageLimits(_ context.Context, tokenData *kiroauth.TokenData) (*kiroauth.KiroUsageInfo, error) {
	f.tokenData = tokenData
	if tokenData != nil {
		f.accessTokens = append(f.accessTokens, tokenData.AccessToken)
	} else {
		f.accessTokens = append(f.accessTokens, "")
	}
	call := f.calls
	f.calls++
	if call < len(f.errors) && f.errors[call] != nil {
		return nil, f.errors[call]
	}
	if f.resolveProfileArn != "" && tokenData != nil {
		tokenData.ProfileArn = f.resolveProfileArn
	}
	limit := 100.0
	current := 25.0
	remaining := 75.0
	return &kiroauth.KiroUsageInfo{
		SubscriptionInfo: &kiroauth.KiroSubscriptionInfo{
			SubscriptionTitle: "Kiro Pro",
			Type:              "PRO",
		},
		UsageBreakdownList: []kiroauth.KiroUsageBreakdown{
			{
				ResourceType:              "AGENTIC_REQUEST",
				DisplayName:               "Agentic requests",
				UsageLimitWithPrecision:   &limit,
				CurrentUsageWithPrecision: &current,
				RemainingWithPrecision:    &remaining,
			},
		},
	}, nil
}

func TestGetKiroUsageUsesNamedAuthFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	client := &fakeKiroUsageClient{}
	previousFactory := newKiroUsageClient
	newKiroUsageClient = func(*config.Config) kiroUsageClient { return client }
	t.Cleanup(func() { newKiroUsageClient = previousFactory })

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "kiro-auth.json",
		FileName: "kiro-auth.json",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"profile_arn":   "arn:aws:codewhisperer:us-east-1:123:profile/test",
			"client_id":     "client-id",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/kiro-usage?name=kiro-auth.json", nil)

	h.GetKiroUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if client.tokenData == nil || client.tokenData.AccessToken != "access-token" || client.tokenData.ProfileArn == "" {
		t.Fatalf("unexpected token data: %+v", client.tokenData)
	}

	var payload struct {
		SubscriptionInfo struct {
			SubscriptionTitle string `json:"subscriptionTitle"`
		} `json:"subscriptionInfo"`
		UsageBreakdownList []struct {
			RemainingWithPrecision float64 `json:"remainingWithPrecision"`
		} `json:"usageBreakdownList"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.SubscriptionInfo.SubscriptionTitle != "Kiro Pro" {
		t.Fatalf("subscription title = %q, want Kiro Pro", payload.SubscriptionInfo.SubscriptionTitle)
	}
	if len(payload.UsageBreakdownList) != 1 || payload.UsageBreakdownList[0].RemainingWithPrecision != 75 {
		t.Fatalf("unexpected usage breakdown: %+v", payload.UsageBreakdownList)
	}
}

func TestGetKiroUsagePersistsResolvedProfileArn(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	const resolvedProfileArn = "arn:aws:codewhisperer:us-east-1:123:profile/pro"
	client := &fakeKiroUsageClient{resolveProfileArn: resolvedProfileArn}
	previousFactory := newKiroUsageClient
	newKiroUsageClient = func(*config.Config) kiroUsageClient { return client }
	t.Cleanup(func() { newKiroUsageClient = previousFactory })

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "kiro-empty-profile.json",
		FileName: "kiro-empty-profile.json",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":          "kiro",
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"profile_arn":   "",
			"client_id":     "client-id",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/kiro-usage?name=kiro-empty-profile.json", nil)

	h.GetKiroUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var updated *coreauth.Auth
	for _, item := range manager.List() {
		if item.ID == auth.ID {
			updated = item
			break
		}
	}
	if updated == nil {
		t.Fatal("updated auth not found")
	}
	if got, _ := updated.Metadata["profile_arn"].(string); got != resolvedProfileArn {
		t.Fatalf("metadata profile_arn = %q", got)
	}
	if updated.Attributes["profile_arn"] != resolvedProfileArn {
		t.Fatalf("attribute profile_arn = %q", updated.Attributes["profile_arn"])
	}
}

type fakeKiroRefreshExecutor struct {
	refreshed bool
	calls     int
}

func (e *fakeKiroRefreshExecutor) Identifier() string { return "kiro" }

func (e *fakeKiroRefreshExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *fakeKiroRefreshExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *fakeKiroRefreshExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.refreshed = true
	e.calls++
	updated := auth.Clone()
	updated.Metadata["access_token"] = "new-access-token"
	updated.Metadata["refresh_token"] = "new-refresh-token"
	updated.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	updated.Metadata["refresh_interval_seconds"] = 300
	updated.Metadata["expires_at"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	updated.NextRefreshAfter = time.Now().UTC().Add(5 * time.Minute)
	return updated, nil
}

func (e *fakeKiroRefreshExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *fakeKiroRefreshExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestGetKiroUsageRefreshesDueTokenBeforeRequest(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	client := &fakeKiroUsageClient{}
	previousFactory := newKiroUsageClient
	newKiroUsageClient = func(*config.Config) kiroUsageClient { return client }
	t.Cleanup(func() { newKiroUsageClient = previousFactory })

	manager := coreauth.NewManager(nil, nil, nil)
	refreshExec := &fakeKiroRefreshExecutor{}
	manager.RegisterExecutor(refreshExec)
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:       "kiro-due.json",
		FileName: "kiro-due.json",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":                     "kiro",
			"access_token":             "old-access-token",
			"refresh_token":            "old-refresh-token",
			"profile_arn":              "arn:aws:codewhisperer:us-east-1:123:profile/test",
			"auth_method":              "kiro-cli-social",
			"provider":                 "google",
			"last_refresh":             now.Add(-6 * time.Minute).Format(time.RFC3339),
			"refresh_interval_seconds": 300,
			"expires_at":               now.Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/kiro-usage?name=kiro-due.json", nil)

	h.GetKiroUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !refreshExec.refreshed {
		t.Fatal("expected Kiro auth to be refreshed before usage request")
	}
	if client.tokenData == nil || client.tokenData.AccessToken != "new-access-token" {
		t.Fatalf("usage token = %+v, want refreshed access token", client.tokenData)
	}
	updated, ok := manager.GetByID("kiro-due.json")
	if !ok {
		t.Fatal("updated auth not found")
	}
	if got := updated.Metadata["access_token"]; got != "new-access-token" {
		t.Fatalf("persisted access_token = %#v, want refreshed token", got)
	}
}

func TestGetKiroUsageRefreshesAndRetriesAfterUnauthorized(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	client := &fakeKiroUsageClient{
		errors: []error{
			&kiroauth.StatusError{
				Operation:  "get usage limits",
				StatusCode: http.StatusUnauthorized,
				Body:       `{"message":"The bearer token included in the request is invalid.","reason":null}`,
			},
		},
	}
	previousFactory := newKiroUsageClient
	newKiroUsageClient = func(*config.Config) kiroUsageClient { return client }
	t.Cleanup(func() { newKiroUsageClient = previousFactory })

	manager := coreauth.NewManager(nil, nil, nil)
	refreshExec := &fakeKiroRefreshExecutor{}
	manager.RegisterExecutor(refreshExec)
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:       "kiro-stale-for-quota.json",
		FileName: "kiro-stale-for-quota.json",
		Provider: "kiro",
		Metadata: map[string]any{
			"type":                     "kiro",
			"access_token":             "old-access-token",
			"refresh_token":            "old-refresh-token",
			"profile_arn":              "arn:aws:codewhisperer:us-east-1:123:profile/test",
			"auth_method":              "kiro-cli-social",
			"provider":                 "google",
			"last_refresh":             now.Format(time.RFC3339),
			"refresh_interval_seconds": 300,
			"expires_at":               now.Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/kiro-usage?name=kiro-stale-for-quota.json", nil)

	h.GetKiroUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if refreshExec.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshExec.calls)
	}
	if len(client.accessTokens) != 2 {
		t.Fatalf("usage calls = %d, want 2 (tokens=%v)", len(client.accessTokens), client.accessTokens)
	}
	if client.accessTokens[0] != "old-access-token" || client.accessTokens[1] != "new-access-token" {
		t.Fatalf("usage access tokens = %v, want old then refreshed", client.accessTokens)
	}
	updated, ok := manager.GetByID("kiro-stale-for-quota.json")
	if !ok {
		t.Fatal("updated auth not found")
	}
	if got := updated.Metadata["access_token"]; got != "new-access-token" {
		t.Fatalf("persisted access_token = %#v, want refreshed token", got)
	}
	if got := updated.Metadata["refresh_token"]; got != "new-refresh-token" {
		t.Fatalf("persisted refresh_token = %#v, want refreshed token", got)
	}
}

func TestGetKiroUsageUnauthorizedRefreshWritesAuthFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	client := &fakeKiroUsageClient{
		errors: []error{
			&kiroauth.StatusError{
				Operation:  "get usage limits",
				StatusCode: http.StatusUnauthorized,
				Body:       `{"message":"The bearer token included in the request is invalid."}`,
			},
		},
	}
	previousFactory := newKiroUsageClient
	newKiroUsageClient = func(*config.Config) kiroUsageClient { return client }
	t.Cleanup(func() { newKiroUsageClient = previousFactory })

	dir := t.TempDir()
	fileName := "kiro-file-refresh.json"
	filePath := filepath.Join(dir, fileName)
	now := time.Now().UTC()
	raw := map[string]any{
		"type":                     "kiro",
		"auth_method":              "kiro-cli-social",
		"provider":                 "google",
		"access_token":             "old-access-token",
		"refresh_token":            "old-refresh-token",
		"profile_arn":              "arn:aws:codewhisperer:us-east-1:123:profile/test",
		"last_refresh":             now.Format(time.RFC3339),
		"refresh_interval_seconds": 300,
		"expires_at":               now.Add(30 * time.Minute).Format(time.RFC3339),
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	refreshExec := &fakeKiroRefreshExecutor{}
	manager.RegisterExecutor(refreshExec)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/kiro-usage?name=kiro-file-refresh.json", nil)

	h.GetKiroUsage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if refreshExec.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshExec.calls)
	}
	updatedRaw, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read updated auth file: %v", err)
	}
	var updated map[string]any
	if err := json.Unmarshal(updatedRaw, &updated); err != nil {
		t.Fatalf("unmarshal updated auth file: %v", err)
	}
	if got := updated["access_token"]; got != "new-access-token" {
		t.Fatalf("file access_token = %#v, want refreshed token", got)
	}
	if got := updated["refresh_token"]; got != "new-refresh-token" {
		t.Fatalf("file refresh_token = %#v, want refreshed token", got)
	}
	if len(client.accessTokens) != 2 || client.accessTokens[0] != "old-access-token" || client.accessTokens[1] != "new-access-token" {
		t.Fatalf("usage access tokens = %v, want old then refreshed", client.accessTokens)
	}
}

func TestReadKiroUsageAuthFromDiskNormalizesKiroCLIToken(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{
		"provider": "google",
		"accessToken": "access-token",
		"refreshToken": "refresh-token",
		"profileArn": "arn:aws:codewhisperer:us-east-1:123:profile/test",
		"expiresAt": "2026-05-09T06:54:01Z"
	}`)
	if err := os.WriteFile(filepath.Join(dir, "kiro-raw.json"), raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	auth, status, err := h.readKiroUsageAuthFromDisk(context.Background(), "kiro-raw.json")
	if err != nil {
		t.Fatalf("readKiroUsageAuthFromDisk() error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got := auth.Provider; got != "kiro" {
		t.Fatalf("provider = %q, want kiro", got)
	}
	if got := auth.Metadata["auth_method"]; got != "kiro-cli-social" {
		t.Fatalf("auth_method = %#v, want kiro-cli-social", got)
	}
}

func TestGetKiroUsageRejectsNonKiroAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth.json",
		FileName: "codex-auth.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "access-token",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/kiro-usage?name=codex-auth.json", nil)

	h.GetKiroUsage(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
