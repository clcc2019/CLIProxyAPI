package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type testTokenStorage struct {
	meta map[string]any
}

func (s *testTokenStorage) SetMetadata(meta map[string]any) { s.meta = meta }

func (s *testTokenStorage) SaveTokenToFile(authFilePath string) error {
	raw, err := json.Marshal(s.meta)
	if err != nil {
		return err
	}
	return os.WriteFile(authFilePath, raw, 0o600)
}

func TestFileTokenStoreSaveDisabledPersistsFlagForTokenStorage(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "disabled.json")

	if err := os.WriteFile(path, []byte(`{"type":"test","disabled":true}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	storage := &testTokenStorage{}
	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		Provider: "test",
		FileName: "disabled.json",
		Disabled: true,
		Storage:  storage,
		Metadata: map[string]any{"type": "test"},
	}

	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if disabled, _ := meta["disabled"].(bool); !disabled {
		t.Fatalf("disabled=%v, want true (raw=%s)", meta["disabled"], string(raw))
	}
}

func TestFileTokenStoreSaveStabilizesCodexInstallationID(t *testing.T) {
	ctx := context.Background()

	t.Run("reauth preserves existing ID", func(t *testing.T) {
		baseDir := t.TempDir()
		path := filepath.Join(baseDir, "codex.json")
		if err := os.WriteFile(path, []byte(`{"type":"codex","installation_id":"installation-existing"}`), 0o600); err != nil {
			t.Fatalf("seed auth file: %v", err)
		}

		store := NewFileTokenStore()
		store.SetBaseDir(baseDir)
		auth := &cliproxyauth.Auth{
			ID:       "codex.json",
			Provider: "codex",
			FileName: "codex.json",
			Metadata: map[string]any{
				"type":            "codex",
				"installation_id": "installation-generated",
			},
		}

		if _, err := store.Save(ctx, auth); err != nil {
			t.Fatalf("Save() error: %v", err)
		}
		if got := cliproxyauth.CodexInstallationID(auth); got != "installation-existing" {
			t.Fatalf("record installation ID = %q, want existing value", got)
		}
		if got := readInstallationID(t, path); got != "installation-existing" {
			t.Fatalf("persisted installation ID = %q, want existing value", got)
		}
	})

	t.Run("explicit override rotates ID", func(t *testing.T) {
		baseDir := t.TempDir()
		path := filepath.Join(baseDir, "codex.json")
		if err := os.WriteFile(path, []byte(`{"type":"codex","installation_id":"installation-existing"}`), 0o600); err != nil {
			t.Fatalf("seed auth file: %v", err)
		}

		store := NewFileTokenStore()
		store.SetBaseDir(baseDir)
		auth := &cliproxyauth.Auth{
			ID:       "codex.json",
			Provider: "codex",
			FileName: "codex.json",
			Metadata: map[string]any{
				"type":            "codex",
				"installation_id": "installation-explicit",
			},
		}
		cliproxyauth.MarkCodexInstallationIDExplicit(auth, true)

		if _, err := store.Save(ctx, auth); err != nil {
			t.Fatalf("Save() error: %v", err)
		}
		if got := readInstallationID(t, path); got != "installation-explicit" {
			t.Fatalf("persisted installation ID = %q, want explicit value", got)
		}
	})

	t.Run("legacy credential is backfilled", func(t *testing.T) {
		baseDir := t.TempDir()
		path := filepath.Join(baseDir, "codex.json")
		if err := os.WriteFile(path, []byte(`{"type":"codex","email":"user@example.com"}`), 0o600); err != nil {
			t.Fatalf("seed auth file: %v", err)
		}

		store := NewFileTokenStore()
		store.SetBaseDir(baseDir)
		auth := &cliproxyauth.Auth{
			ID:       "codex.json",
			Provider: "codex",
			FileName: "codex.json",
			Metadata: map[string]any{"type": "codex", "email": "user@example.com"},
		}

		if _, err := store.Save(ctx, auth); err != nil {
			t.Fatalf("Save() error: %v", err)
		}
		installationID := readInstallationID(t, path)
		if _, err := uuid.Parse(installationID); err != nil {
			t.Fatalf("persisted installation ID = %q, want UUID: %v", installationID, err)
		}
	})
}

func TestFileTokenStoreConcurrentFirstCodexSaveConvergesInstallationID(t *testing.T) {
	baseDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auths := []*cliproxyauth.Auth{
		{
			ID:       "codex.json",
			Provider: "codex",
			FileName: "codex.json",
			Metadata: map[string]any{"type": "codex", "installation_id": "installation-a"},
		},
		{
			ID:       "codex.json",
			Provider: "codex",
			FileName: "codex.json",
			Metadata: map[string]any{"type": "codex", "installation_id": "installation-b"},
		},
	}

	start := make(chan struct{})
	errCh := make(chan error, len(auths))
	var wg sync.WaitGroup
	for _, auth := range auths {
		wg.Add(1)
		go func(record *cliproxyauth.Auth) {
			defer wg.Done()
			<-start
			_, err := store.Save(context.Background(), record)
			errCh <- err
		}(auth)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("Save() error: %v", err)
		}
	}

	first := cliproxyauth.CodexInstallationID(auths[0])
	second := cliproxyauth.CodexInstallationID(auths[1])
	if first == "" || second != first {
		t.Fatalf("concurrent installation IDs = %q and %q, want one stable value", first, second)
	}
	if got := readInstallationID(t, filepath.Join(baseDir, "codex.json")); got != first {
		t.Fatalf("persisted installation ID = %q, want %q", got, first)
	}
}

func readInstallationID(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decode auth file: %v", err)
	}
	value, _ := metadata[cliproxyauth.AuthFileCodexInstallationIDKey].(string)
	return value
}

func TestFileTokenStoreListReadsJSONFilesInWalkOrder(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	nestedDir := filepath.Join(baseDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o700); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}
	files := map[string]string{
		"a.json":             `{"type":"codex","label":"a"}`,
		"b.json":             `{"type":"claude","label":"b"}`,
		"nested/c.json":      `{"type":"xai","label":"c"}`,
		"nested/empty.json":  ``,
		"nested/ignored.txt": `{"type":"codex","label":"ignored"}`,
	}
	for name, data := range files {
		path := filepath.Join(baseDir, filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auths, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	labels := make([]string, 0, len(auths))
	for _, auth := range auths {
		labels = append(labels, auth.Label)
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %#v, want %#v", labels, want)
	}
}

func TestFileTokenStoreListRemovesRemoteCompactionV2FromAuthFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex.json")
	if err := os.WriteFile(path, []byte(`{
		"type":"codex",
		"beta_features":"feature-a,remote_compaction_v2",
		"headers":{"X-Codex-Beta-Features":"remote_compaction_v2","X-Keep":"value"}
	}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("List() auth count = %d, want 1", len(auths))
	}
	if got := auths[0].Attributes["header:X-Codex-Beta-Features"]; got != "feature-a" {
		t.Fatalf("runtime beta features = %q, want feature-a", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cleaned auth file: %v", err)
	}
	var metadata map[string]any
	if err = json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("unmarshal cleaned auth file: %v", err)
	}
	if got := metadata["beta_features"]; got != "feature-a" {
		t.Fatalf("persisted beta_features = %#v, want feature-a", got)
	}
	headers := metadata["headers"].(map[string]any)
	if _, exists := headers["X-Codex-Beta-Features"]; exists {
		t.Fatalf("request-scoped header remains in auth file: %#v", headers)
	}
	if got := headers["X-Keep"]; got != "value" {
		t.Fatalf("unrelated header = %#v, want value", got)
	}
}

func TestFileTokenStoreSaveRejectsPathOutsideBaseDir(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "outside.json")

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auth := &cliproxyauth.Auth{
		ID:       "outside.json",
		Provider: "test",
		Attributes: map[string]string{
			"path": outsidePath,
		},
		Metadata: map[string]any{"type": "test"},
	}

	if _, err := store.Save(ctx, auth); err == nil {
		t.Fatalf("Save() error = nil, want outside auth directory error")
	}
	if _, err := os.Stat(outsidePath); !os.IsNotExist(err) {
		t.Fatalf("outside file stat error = %v, want not exist", err)
	}
}

func TestFileTokenStoreSaveRejectsSymlinkEscape(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outsidePath, []byte(`{"type":"outside"}`), 0o600); err != nil {
		t.Fatalf("seed outside auth file: %v", err)
	}
	linkPath := filepath.Join(baseDir, "escape.json")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auth := &cliproxyauth.Auth{
		ID:       "escape.json",
		Provider: "test",
		FileName: "escape.json",
		Metadata: map[string]any{"type": "test"},
	}

	if _, err := store.Save(ctx, auth); err == nil {
		t.Fatalf("Save() error = nil, want symlink escape error")
	}
	raw, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatalf("read outside auth file: %v", err)
	}
	if string(raw) != `{"type":"outside"}` {
		t.Fatalf("outside file was modified: %s", raw)
	}
}

func TestFileTokenStoreSaveTokenStorageRejectsSymlinkEscape(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outsidePath, []byte(`{"type":"outside"}`), 0o600); err != nil {
		t.Fatalf("seed outside auth file: %v", err)
	}
	linkPath := filepath.Join(baseDir, "escape-token.json")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auth := &cliproxyauth.Auth{
		ID:       "escape-token.json",
		Provider: "test",
		FileName: "escape-token.json",
		Storage:  &testTokenStorage{},
		Metadata: map[string]any{"type": "test"},
	}

	if _, err := store.Save(ctx, auth); err == nil {
		t.Fatalf("Save() error = nil, want symlink escape error")
	}
	raw, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatalf("read outside auth file: %v", err)
	}
	if string(raw) != `{"type":"outside"}` {
		t.Fatalf("outside file was modified: %s", raw)
	}
}
