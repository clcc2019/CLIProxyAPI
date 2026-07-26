package misc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteCredentialJSONAtomicWritesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "auth.json")

	if err := WriteCredentialJSONAtomic(path, map[string]any{"refresh_token": "rt-1"}, ""); err != nil {
		t.Fatalf("WriteCredentialJSONAtomic: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	if got["refresh_token"] != "rt-1" {
		t.Errorf("refresh_token = %v, want rt-1", got["refresh_token"])
	}
}

func TestWriteCredentialJSONAtomicPermissionsAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on windows")
	}
	path := filepath.Join(t.TempDir(), "auth.json")

	if err := WriteCredentialJSONAtomic(path, map[string]any{"k": "v"}, ""); err != nil {
		t.Fatalf("WriteCredentialJSONAtomic: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Credentials must never be group- or world-readable.
	if perm := info.Mode().Perm(); perm != credentialFileMode {
		t.Errorf("permissions = %#o, want %#o", perm, credentialFileMode)
	}
}

// The whole point of the rename: a failed write must leave the previous
// credential intact rather than truncating it. Encoding a channel makes
// json.Encoder fail after the temp file already exists.
func TestWriteCredentialJSONAtomicPreservesExistingFileOnEncodeFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	const existing = `{"refresh_token":"single-use-token"}`
	if err := os.WriteFile(path, []byte(existing), credentialFileMode); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	err := WriteCredentialJSONAtomic(path, map[string]any{"bad": make(chan int)}, "")
	if err == nil {
		t.Fatal("expected an encode error, got nil")
	}

	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("previous credential file is gone: %v", readErr)
	}
	if string(raw) != existing {
		t.Errorf("previous credential was clobbered\n got: %s\nwant: %s", raw, existing)
	}
}

// A failed write must not leave temp files accumulating in the credential dir.
func TestWriteCredentialJSONAtomicCleansUpTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	if err := WriteCredentialJSONAtomic(path, map[string]any{"bad": make(chan int)}, ""); err == nil {
		t.Fatal("expected an encode error, got nil")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", entry.Name())
		}
	}
}

func TestWriteCredentialJSONAtomicIndent(t *testing.T) {
	dir := t.TempDir()

	compact := filepath.Join(dir, "compact.json")
	if err := WriteCredentialJSONAtomic(compact, map[string]any{"a": "b"}, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	raw, err := os.ReadFile(compact)
	if err != nil {
		t.Fatalf("read compact: %v", err)
	}
	if strings.Contains(string(raw), "\n  ") {
		t.Errorf("compact output is indented: %s", raw)
	}

	indented := filepath.Join(dir, "indented.json")
	if err := WriteCredentialJSONAtomic(indented, map[string]any{"a": "b"}, "  "); err != nil {
		t.Fatalf("indented: %v", err)
	}
	raw, err = os.ReadFile(indented)
	if err != nil {
		t.Fatalf("read indented: %v", err)
	}
	if !strings.Contains(string(raw), "\n  ") {
		t.Errorf("indented output is not indented: %s", raw)
	}
}

func TestSaveCredentialJSONAtomicMergesMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	source := struct {
		Type string `json:"type"`
	}{Type: "codex"}

	if err := SaveCredentialJSONAtomic(path, source, map[string]any{"email": "a@b.c"}, ""); err != nil {
		t.Fatalf("SaveCredentialJSONAtomic: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	if got["type"] != "codex" {
		t.Errorf("type = %v, want codex", got["type"])
	}
	if got["email"] != "a@b.c" {
		t.Errorf("email = %v, want a@b.c (metadata not merged)", got["email"])
	}
}

// Atomic replacement swaps the path itself. When that path is a symlink the
// link is replaced, not followed — pinned here so the behaviour is a decision
// rather than a surprise.
func TestWriteCredentialJSONAtomicReplacesSymlinkItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "link.json")

	const original = `{"owner":"target"}`
	if err := os.WriteFile(target, []byte(original), credentialFileMode); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := WriteCredentialJSONAtomic(link, map[string]any{"owner": "link"}, ""); err != nil {
		t.Fatalf("WriteCredentialJSONAtomic: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("path is still a symlink; the write did not replace it")
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != original {
		t.Errorf("symlink target was modified: %s (err=%v)", raw, err)
	}
}
