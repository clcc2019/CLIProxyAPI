package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type statOnDeleteStore struct {
	deletedPath string
	existed     bool
}

type failingDeleteStore struct{}

func setupManagementDeleteTest(t *testing.T) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
}

func writeAuthTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write auth file %s: %v", name, err)
	}
	return path
}

func newAuthDeleteTestHandler(authDir string, manager *coreauth.Manager, store coreauth.Store) *Handler {
	if manager == nil {
		manager = coreauth.NewManager(nil, nil, nil)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store
	return h
}

func performAuthDelete(t *testing.T, h *Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodDelete, target, nil)
	h.DeleteAuthFile(ctx)
	return rec
}

func performAuthDeleteBody(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files", bytes.NewBufferString(body))
	h.DeleteAuthFile(ctx)
	return rec
}

func assertHTTPStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("expected status %d, got %d with body %s", want, rec.Code, rec.Body.String())
	}
}

func assertFileMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed, stat err: %v", err)
	}
}

func assertAuthRemoved(t *testing.T, manager *coreauth.Manager, id string) {
	t.Helper()
	if auth, ok := manager.GetByID(id); ok || auth != nil {
		t.Fatalf("expected in-memory auth %q to be removed after delete, got auth=%#v ok=%v", id, auth, ok)
	}
}

func listAuthFilesForTest(t *testing.T, h *Handler) []any {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)
	assertHTTPStatus(t, rec, http.StatusOK)
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode list payload: %v", err)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok {
		t.Fatalf("expected files array, payload: %#v", payload)
	}
	return filesRaw
}

func (s *statOnDeleteStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }

func (s *statOnDeleteStore) Save(context.Context, *coreauth.Auth) (string, error) { return "", nil }

func (s *statOnDeleteStore) Delete(_ context.Context, id string) error {
	s.deletedPath = id
	if _, err := os.Stat(id); err == nil {
		s.existed = true
		return nil
	} else if os.IsNotExist(err) {
		s.existed = false
		return nil
	} else {
		return fmt.Errorf("stat in delete: %w", err)
	}
}

func (s *failingDeleteStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }

func (s *failingDeleteStore) Save(context.Context, *coreauth.Auth) (string, error) { return "", nil }

func (s *failingDeleteStore) Delete(context.Context, string) error {
	return fmt.Errorf("delete failed")
}

func TestDeleteAuthFile_UsesAuthPathFromManager(t *testing.T) {
	setupManagementDeleteTest(t)

	tempDir := t.TempDir()
	authDir := filepath.Join(tempDir, "auth")
	externalDir := filepath.Join(tempDir, "external")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}
	if errMkdirExternal := os.MkdirAll(externalDir, 0o700); errMkdirExternal != nil {
		t.Fatalf("failed to create external dir: %v", errMkdirExternal)
	}

	fileName := "codex-user@example.com-plus.json"
	shadowPath := writeAuthTestFile(t, authDir, fileName, `{"type":"codex","email":"shadow@example.com"}`)
	realPath := writeAuthTestFile(t, externalDir, fileName, `{"type":"codex","email":"real@example.com"}`)

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:          "legacy/" + fileName,
		FileName:    fileName,
		Provider:    "codex",
		Status:      coreauth.StatusError,
		Unavailable: true,
		Attributes: map[string]string{
			"path": realPath,
		},
		Metadata: map[string]any{
			"type":  "codex",
			"email": "real@example.com",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := newAuthDeleteTestHandler(authDir, manager, &memoryAuthStore{})

	deleteRec := performAuthDelete(t, h, "/v0/management/auth-files?name="+url.QueryEscape(fileName))
	assertHTTPStatus(t, deleteRec, http.StatusOK)
	assertFileMissing(t, realPath)
	if _, errStatShadow := os.Stat(shadowPath); errStatShadow != nil {
		t.Fatalf("expected shadow auth file to remain, stat err: %v", errStatShadow)
	}

	filesRaw := listAuthFilesForTest(t, h)
	if len(filesRaw) != 0 {
		t.Fatalf("expected removed auth to be hidden from list, got %d entries", len(filesRaw))
	}
}

func TestDeleteAuthFile_FallbackToAuthDirPath(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	fileName := "fallback-user.json"
	filePath := writeAuthTestFile(t, authDir, fileName, `{"type":"codex"}`)

	h := newAuthDeleteTestHandler(authDir, nil, &memoryAuthStore{})

	deleteRec := performAuthDelete(t, h, "/v0/management/auth-files?name="+url.QueryEscape(fileName))
	assertHTTPStatus(t, deleteRec, http.StatusOK)
	assertFileMissing(t, filePath)
}

func TestDeleteAuthFile_ByAuthIndexDoesNotRecreateKiroFile(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	fileName := "kiro-google-user-example-com.json"
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "kiro",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filepath.Join(authDir, fileName),
		},
		Metadata: map[string]any{
			"type":          "kiro",
			"provider":      "google",
			"email":         "user@example.com",
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
		},
	}
	authIndex := record.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	filePath := filepath.Join(authDir, fileName)
	if _, errStat := os.Stat(filePath); errStat != nil {
		t.Fatalf("expected auth file to be saved, stat err: %v", errStat)
	}

	h := newAuthDeleteTestHandler(authDir, manager, store)

	rec := performAuthDelete(t, h, "/v0/management/auth-files?auth_index="+url.QueryEscape(authIndex))
	assertHTTPStatus(t, rec, http.StatusOK)
	assertFileMissing(t, filePath)
	assertAuthRemoved(t, manager, fileName)

	staleRefreshResult := record.Clone()
	staleRefreshResult.Metadata["access_token"] = "rotated-access-token"
	staleRefreshResult.Metadata["refresh_token"] = "rotated-refresh-token"
	staleRefreshResult.Status = coreauth.StatusActive
	staleRefreshResult.Disabled = false
	if _, errUpdate := manager.Update(context.Background(), staleRefreshResult); errUpdate != nil {
		t.Fatalf("stale update after delete failed: %v", errUpdate)
	}
	assertFileMissing(t, filePath)
	assertAuthRemoved(t, manager, fileName)
}

func TestDeleteAuthFile_StoreDeleteRunsBeforeLocalRemove(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	fileName := "kiro-google-user-example-com.json"
	filePath := writeAuthTestFile(t, authDir, fileName, `{"type":"kiro"}`)

	store := &statOnDeleteStore{}
	h := newAuthDeleteTestHandler(authDir, nil, store)

	rec := performAuthDelete(t, h, "/v0/management/auth-files?name="+url.QueryEscape(fileName))
	assertHTTPStatus(t, rec, http.StatusOK)
	if store.deletedPath != filePath {
		t.Fatalf("store deleted path = %q, want %q", store.deletedPath, filePath)
	}
	if !store.existed {
		t.Fatal("expected backing store Delete to see the local file before fallback removal")
	}
	assertFileMissing(t, filePath)
}

func TestDeleteAuthFile_DeleteRecordFailureRestoresAuthState(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	fileName := "kiro-google-user-example-com.json"
	filePath := writeAuthTestFile(t, authDir, fileName, `{"type":"kiro"}`)
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "kiro",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{"type": "kiro"},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := newAuthDeleteTestHandler(authDir, manager, &failingDeleteStore{})

	rec := performAuthDelete(t, h, "/v0/management/auth-files?name="+url.QueryEscape(fileName))
	assertHTTPStatus(t, rec, http.StatusInternalServerError)
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("expected auth file to remain after delete failure, stat err: %v", err)
	}
	restored, ok := manager.GetByID(fileName)
	if !ok || restored == nil {
		t.Fatalf("expected auth %q to be restored after delete failure", fileName)
	}
	if restored.Disabled || restored.Status != coreauth.StatusActive {
		t.Fatalf("restored disabled/status = %v/%s, want false/%s", restored.Disabled, restored.Status, coreauth.StatusActive)
	}
}

func TestDeleteAuthFile_AcceptsFilenameBody(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	fileName := "kiro-google-user-example-com.json"
	filePath := writeAuthTestFile(t, authDir, fileName, `{"type":"kiro"}`)

	h := newAuthDeleteTestHandler(authDir, nil, &memoryAuthStore{})

	rec := performAuthDeleteBody(t, h, `{"filename":"`+fileName+`"}`)
	assertHTTPStatus(t, rec, http.StatusOK)
	assertFileMissing(t, filePath)
}

func TestDeleteAuthFile_RemovesLegacyKiroIDWithoutJSONSuffix(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	legacyID := "kiro-google-user-example-com"
	fileName := legacyID + ".json"
	filePath := writeAuthTestFile(t, authDir, fileName, `{"type":"kiro"}`)

	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       legacyID,
		Provider: "kiro",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{"type": "kiro"},
	}); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := newAuthDeleteTestHandler(authDir, manager, &memoryAuthStore{})

	rec := performAuthDelete(t, h, "/v0/management/auth-files?name="+url.QueryEscape(fileName))
	assertHTTPStatus(t, rec, http.StatusOK)
	assertFileMissing(t, filePath)
	assertAuthRemoved(t, manager, legacyID)
}

func TestDeleteAuthFile_LegacyKiroRecordWithoutPathDeletesJSONFile(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	legacyName := "kiro-google-user-example-com"
	fileName := legacyName + ".json"
	filePath := writeAuthTestFile(t, authDir, fileName, `{"type":"kiro"}`)

	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       legacyName,
		FileName: legacyName,
		Provider: "kiro",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "kiro"},
	}); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := newAuthDeleteTestHandler(authDir, manager, &memoryAuthStore{})

	rec := performAuthDelete(t, h, "/v0/management/auth-files?name="+url.QueryEscape(legacyName))
	assertHTTPStatus(t, rec, http.StatusOK)
	assertFileMissing(t, filePath)
	assertAuthRemoved(t, manager, legacyName)
}

func TestListAuthFiles_HidesFileBackedAuthMissingOnDisk(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	fileName := "kiro-google-user-example-com.json"
	filePath := filepath.Join(authDir, fileName)

	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "kiro",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{"type": "kiro"},
	}); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := newAuthDeleteTestHandler(authDir, manager, nil)

	filesRaw := listAuthFilesForTest(t, h)
	if len(filesRaw) != 0 {
		t.Fatalf("expected missing file-backed auth to be hidden, got %d entries", len(filesRaw))
	}
}

func TestDeleteAuthFile_ByAbsolutePathFromManagedAuth(t *testing.T) {
	setupManagementDeleteTest(t)

	authDir := t.TempDir()
	fileName := "kiro-google-user-example-com.json"
	filePath := writeAuthTestFile(t, authDir, fileName, `{"type":"kiro"}`)

	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "kiro",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{"type": "kiro"},
	}); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := newAuthDeleteTestHandler(authDir, manager, &memoryAuthStore{})

	rec := performAuthDelete(t, h, "/v0/management/auth-files?path="+url.QueryEscape(filePath))
	assertHTTPStatus(t, rec, http.StatusOK)
	assertFileMissing(t, filePath)
	assertAuthRemoved(t, manager, fileName)
}
