package watcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeAuthFileCodexFeaturesRemovesRemoteCompactionV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.json")
	data := []byte(`{"type":"codex","beta_features":"keep,remote_compaction_v2"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	cleaned := sanitizeAuthFileCodexFeatures(path, data)
	var metadata map[string]any
	if err := json.Unmarshal(cleaned, &metadata); err != nil {
		t.Fatalf("unmarshal cleaned data: %v", err)
	}
	if got := metadata["beta_features"]; got != "keep" {
		t.Fatalf("cleaned beta_features = %#v, want keep", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	if err = json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("unmarshal persisted auth file: %v", err)
	}
	if got := metadata["beta_features"]; got != "keep" {
		t.Fatalf("persisted beta_features = %#v, want keep", got)
	}
}
