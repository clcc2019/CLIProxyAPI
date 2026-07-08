package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_RequestLogDefaultsDisabled(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.RequestLog {
		t.Fatal("request-log default = true, want false")
	}
}

func TestParseConfigBytes_RequestLogDefaultsDisabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.RequestLog {
		t.Fatal("request-log default = true, want false")
	}
}

func TestParseConfigBytes_RequestLogExplicitTrue(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("request-log: true\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if !cfg.RequestLog {
		t.Fatal("request-log explicit true was not preserved")
	}
}
