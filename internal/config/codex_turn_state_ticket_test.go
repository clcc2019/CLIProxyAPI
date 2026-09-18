package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_CodexTurnStateTicket(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := []byte(`
codex:
  openai-codex-ticket:
    enabled: true
    auth-files:
      - codex-primary.json
    target-length: 292
    ttl-seconds: 3600
    refresh-before-seconds: 600
    harvest-proxy-url: "socks5h://127.0.0.1:1080"
    harvest-probe-interval-seconds: 6
    harvest-attempt-timeout-seconds: 25
    fail-closed: true
    models:
      - gpt-6-astra
      - gpt-5.6-sol
`)
	if err := os.WriteFile(configPath, configYAML, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	got := cfg.Codex.OpenAICodexTicket
	if !got.Enabled || got.TargetLength != 292 || got.TTLSeconds != 3600 || got.RefreshBeforeSeconds != 600 {
		t.Fatalf("unexpected ticket timing config: %+v", got)
	}
	if got.HarvestProxyURL != "socks5h://127.0.0.1:1080" || !got.FailClosed {
		t.Fatalf("unexpected ticket proxy/fail-closed config: %+v", got)
	}
	if len(got.AuthFiles) != 1 || got.AuthFiles[0] != "codex-primary.json" {
		t.Fatalf("unexpected ticket auth files: %#v", got.AuthFiles)
	}
	if len(got.Models) != 2 || got.Models[0] != "gpt-6-astra" || got.Models[1] != "gpt-5.6-sol" {
		t.Fatalf("unexpected ticket models: %#v", got.Models)
	}
}
