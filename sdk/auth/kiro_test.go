package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestKiroCreateAuthRecordBuilderIDMetadata(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	authenticator := NewKiroAuthenticator()
	record := authenticator.createAuthRecord(&kiroauth.TokenData{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    expiresAt,
		AuthMethod:   "builder-id",
		Provider:     "AWS",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		Email:        "user@example.com",
		StartURL:     kiroauth.BuilderIDStartURL,
		Region:       kiroauth.DefaultRegion,
	}, "aws-builder-id")

	if record.Provider != "kiro" || record.Status != coreauth.StatusActive {
		t.Fatalf("unexpected auth record: %+v", record)
	}
	if record.ID != "kiro-aws-user@example.com.json" || record.FileName != record.ID {
		t.Fatalf("unexpected file name: id=%q file=%q", record.ID, record.FileName)
	}
	if record.Label != "kiro-aws" {
		t.Fatalf("label = %q, want kiro-aws", record.Label)
	}

	assertMetadataString(t, record.Metadata, "type", "kiro")
	assertMetadataString(t, record.Metadata, "access_token", "access-token")
	assertMetadataString(t, record.Metadata, "refresh_token", "refresh-token")
	assertMetadataString(t, record.Metadata, "auth_method", "builder-id")
	assertMetadataString(t, record.Metadata, "provider", "AWS")
	assertMetadataString(t, record.Metadata, "client_id", "client-id")
	assertMetadataString(t, record.Metadata, "client_secret", "client-secret")
	assertMetadataString(t, record.Metadata, "email", "user@example.com")
	assertMetadataString(t, record.Metadata, "region", kiroauth.DefaultRegion)
	assertMetadataString(t, record.Metadata, "start_url", kiroauth.BuilderIDStartURL)
	if got := record.Metadata["last_refresh"]; got == "" {
		t.Fatalf("last_refresh metadata is empty")
	}
	refreshInterval, ok := record.Metadata["refresh_interval_seconds"].(int)
	if !ok {
		t.Fatalf("refresh_interval_seconds = %#v, want int", record.Metadata["refresh_interval_seconds"])
	}
	if refreshInterval < kiroauth.DefaultRefreshIntervalMinSeconds || refreshInterval > kiroauth.DefaultRefreshIntervalMaxSeconds {
		t.Fatalf("refresh_interval_seconds = %d, want %d-%d", refreshInterval, kiroauth.DefaultRefreshIntervalMinSeconds, kiroauth.DefaultRefreshIntervalMaxSeconds)
	}
	minNext := time.Now().Add(time.Duration(kiroauth.DefaultRefreshIntervalMinSeconds)*time.Second - time.Second)
	maxNext := time.Now().Add(time.Duration(kiroauth.DefaultRefreshIntervalMaxSeconds)*time.Second + time.Second)
	if record.NextRefreshAfter.Before(minNext) || record.NextRefreshAfter.After(maxNext) {
		t.Fatalf("NextRefreshAfter = %s, want within default refresh interval", record.NextRefreshAfter)
	}
	if machineID := kiroauth.NormalizeKiroMachineID(record.Metadata["machine_id"].(string)); machineID == "" {
		t.Fatalf("machine_id metadata is invalid: %#v", record.Metadata["machine_id"])
	}
	if record.Attributes["source"] != "aws-builder-id" || record.Attributes["auth_method"] != "builder-id" {
		t.Fatalf("unexpected attributes: %+v", record.Attributes)
	}
}

func TestKiroCreateAuthRecordSocialMetadataOmitsEmptyOptionalFields(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	authenticator := NewKiroAuthenticator()
	record := authenticator.createAuthRecord(&kiroauth.TokenData{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    expiresAt,
		AuthMethod:   "kiro-cli-social",
		Provider:     "google",
	}, "kiro-oauth")

	assertMetadataString(t, record.Metadata, "type", "kiro")
	assertMetadataString(t, record.Metadata, "access_token", "access-token")
	assertMetadataString(t, record.Metadata, "refresh_token", "refresh-token")
	assertMetadataString(t, record.Metadata, "auth_method", "kiro-cli-social")
	assertMetadataString(t, record.Metadata, "provider", "google")
	for _, key := range []string{"client_id", "client_secret", "client_id_hash", "email", "region", "start_url", "profile_arn"} {
		assertMetadataAbsent(t, record.Metadata, key)
	}
	if record.Attributes["source"] != "kiro-oauth" || record.Attributes["auth_method"] != "kiro-cli-social" {
		t.Fatalf("unexpected attributes: %+v", record.Attributes)
	}
	if _, ok := record.Attributes["email"]; ok {
		t.Fatalf("email attribute should be omitted for empty email: %+v", record.Attributes)
	}
	if record.LastRefreshedAt.IsZero() {
		t.Fatal("LastRefreshedAt should be set for social auth")
	}
}

func TestKiroRegistersDefaultAutoRefresh(t *testing.T) {
	if !coreauth.ProviderDefaultAutoRefresh("kiro") {
		t.Fatal("expected Kiro to enable provider-level auto refresh")
	}
	got := coreauth.ProviderDefaultRefreshInterval("kiro")
	min := time.Duration(kiroauth.DefaultRefreshIntervalMinSeconds) * time.Second
	max := time.Duration(kiroauth.DefaultRefreshIntervalMaxSeconds) * time.Second
	if got < min || got > max {
		t.Fatalf("Kiro default refresh interval = %s, want %s-%s", got, min, max)
	}
}

func TestKiroAutoRefreshWritesBackAuthFile(t *testing.T) {
	dir := t.TempDir()
	fileName := "kiro-auto.json"
	filePath := filepath.Join(dir, fileName)
	now := time.Now().UTC()
	raw := map[string]any{
		"type":                     "kiro",
		"auth_method":              "kiro-cli-social",
		"provider":                 "google",
		"client_id":                "",
		"client_secret":            "",
		"client_id_hash":           "",
		"email":                    "",
		"region":                   "",
		"start_url":                "",
		"profile_arn":              "",
		"access_token":             "old-access-token",
		"refresh_token":            "old-refresh-token",
		"expires_at":               now.Add(time.Hour).Format(time.RFC3339),
		"last_refresh":             now.Add(-2 * time.Minute).Format(time.RFC3339),
		"refresh_interval_seconds": 1,
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(&kiroAutoRefreshFileExecutor{})
	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !manager.StartAutoRefresh(context.Background(), 10*time.Millisecond) {
		t.Fatal("expected Kiro provider-level auto-refresh to start")
	}
	defer manager.StopAutoRefresh()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		updatedRaw, errRead := os.ReadFile(filePath)
		if errRead != nil {
			t.Fatalf("read auth file: %v", errRead)
		}
		var updated map[string]any
		if err := json.Unmarshal(updatedRaw, &updated); err != nil {
			t.Fatalf("unmarshal updated auth file: %v", err)
		}
		if updated["access_token"] == "new-access-token" && updated["refresh_token"] == "new-refresh-token" {
			if got, _ := updated["last_refresh"].(string); got == "" {
				t.Fatalf("last_refresh was not persisted: %#v", updated["last_refresh"])
			}
			if got, _ := updated["expires_at"].(string); got == "" {
				t.Fatalf("expires_at was not persisted: %#v", updated["expires_at"])
			}
			if got, ok := updated["refresh_interval_seconds"].(float64); !ok || got != 60 {
				t.Fatalf("refresh_interval_seconds = %#v, want 60", updated["refresh_interval_seconds"])
			}
			for _, key := range []string{"client_id", "client_secret", "client_id_hash", "email", "region", "start_url", "profile_arn"} {
				if _, exists := updated[key]; exists {
					t.Fatalf("empty metadata key %q should not be persisted: %#v", key, updated[key])
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	updatedRaw, _ := os.ReadFile(filePath)
	t.Fatalf("auth file was not refreshed before deadline: %s", string(updatedRaw))
}

func assertMetadataString(t *testing.T, metadata map[string]any, key, want string) {
	t.Helper()
	if got, _ := metadata[key].(string); got != want {
		t.Fatalf("metadata[%q] = %q, want %q", key, got, want)
	}
}

func assertMetadataAbsent(t *testing.T, metadata map[string]any, key string) {
	t.Helper()
	if _, ok := metadata[key]; ok {
		t.Fatalf("metadata[%q] should be omitted, got %#v", key, metadata[key])
	}
}

type kiroAutoRefreshFileExecutor struct{}

func (e *kiroAutoRefreshFileExecutor) Identifier() string { return "kiro" }

func (e *kiroAutoRefreshFileExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *kiroAutoRefreshFileExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *kiroAutoRefreshFileExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	now := time.Now().UTC()
	updated.Metadata["access_token"] = "new-access-token"
	updated.Metadata["refresh_token"] = "new-refresh-token"
	updated.Metadata["expires_at"] = now.Add(time.Hour).Format(time.RFC3339)
	updated.Metadata["last_refresh"] = now.Format(time.RFC3339)
	updated.Metadata["refresh_interval_seconds"] = 60
	updated.NextRefreshAfter = now.Add(time.Minute)
	return updated, nil
}

func (e *kiroAutoRefreshFileExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *kiroAutoRefreshFileExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestKiroOAuthCallbackServerReceivesCode(t *testing.T) {
	srv, port, resultCh, _, err := startKiroOAuthCallbackServer(0)
	if err != nil {
		t.Fatalf("startKiroOAuthCallbackServer() error = %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/oauth/callback?code=auth-code&state=state-123", port))
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case result := <-resultCh:
		if result.Code != "auth-code" || result.State != "state-123" || result.Error != "" {
			t.Fatalf("unexpected callback result: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for callback result")
	}
}

func TestKiroOAuthCallbackServerIgnoresNonCallbackRequest(t *testing.T) {
	srv, port, resultCh, _, err := startKiroOAuthCallbackServer(0)
	if err != nil {
		t.Fatalf("startKiroOAuthCallbackServer() error = %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/favicon.ico", port))
	if err != nil {
		t.Fatalf("empty callback GET: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case result := <-resultCh:
		t.Fatalf("empty request should not complete callback, got %+v", result)
	case <-time.After(100 * time.Millisecond):
	}

	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/oauth/callback?code=auth-code&state=state-123", port))
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case result := <-resultCh:
		if result.Code != "auth-code" || result.State != "state-123" || result.Error != "" {
			t.Fatalf("unexpected callback result: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for callback result")
	}
}

func TestKiroOAuthCallbackValidation(t *testing.T) {
	if err := validateKiroOAuthCallback(kiroOAuthCallbackResult{Code: "code", State: "state"}, "state"); err != nil {
		t.Fatalf("validateKiroOAuthCallback() error = %v", err)
	}
	if err := validateKiroOAuthCallback(kiroOAuthCallbackResult{Code: "code", State: "other"}, "state"); err == nil {
		t.Fatal("expected state mismatch error")
	}
}
