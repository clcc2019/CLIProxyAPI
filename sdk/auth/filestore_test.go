package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
