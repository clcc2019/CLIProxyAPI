package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialWeightsParseAndValidate(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
claude-api-key:
  - api-key: claude
    weight: 5
codex-api-key:
  - api-key: codex
    base-url: https://api.openai.example/v1
    weight: -3
openai-compatibility:
  - name: compat
    base-url: https://compat.example/v1
    api-key-entries:
      - api-key: key
        weight: 0
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.ClaudeKey[0].Weight == nil || *cfg.ClaudeKey[0].Weight != 5 {
		t.Fatalf("claude weight = %#v, want 5", cfg.ClaudeKey[0].Weight)
	}
	if cfg.CodexKey[0].Weight == nil || *cfg.CodexKey[0].Weight != -3 {
		t.Fatalf("codex weight = %#v, want -3", cfg.CodexKey[0].Weight)
	}
	if cfg.OpenAICompatibility[0].APIKeyEntries[0].Weight == nil || *cfg.OpenAICompatibility[0].APIKeyEntries[0].Weight != 0 {
		t.Fatalf("OpenAI compatibility weight = %#v, want 0", cfg.OpenAICompatibility[0].APIKeyEntries[0].Weight)
	}
}

func TestCredentialWeightsRejectValuesAboveMaximumOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("claude-api-key:\n  - api-key: claude\n    weight: 1000001\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "weight must not exceed") {
		t.Fatalf("LoadConfig() error = %v, want maximum-weight validation error", err)
	}
}

func TestSaveConfigPreserveCommentsRejectsInvalidCredentialWeight(t *testing.T) {
	weight := MaxCredentialWeight + 1
	err := SaveConfigPreserveComments(filepath.Join(t.TempDir(), "config.yaml"), &Config{
		ClaudeKey: []ClaudeKey{{APIKey: "claude", Weight: &weight}},
	})
	if err == nil || !strings.Contains(err.Error(), "validate credential weights") {
		t.Fatalf("SaveConfigPreserveComments() error = %v, want validation error", err)
	}
}
