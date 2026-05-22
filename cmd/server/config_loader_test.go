package main

import (
	"path/filepath"
	"testing"
)

func TestLoadConfigResult_KiroLoginAllowsMissingDefaultConfig(t *testing.T) {
	workdir := t.TempDir()

	result, err := loadConfigResult(runtimeFlags{kiroLogin: true}, startupContext{workdir: workdir}, storeSettings{})
	if err != nil {
		t.Fatalf("loadConfigResult() error = %v", err)
	}
	if result.cfg == nil {
		t.Fatal("expected empty config, got nil")
	}
	if result.configFilePath != filepath.Join(workdir, "config.yaml") {
		t.Fatalf("configFilePath = %q", result.configFilePath)
	}
}

func TestLoadConfigResult_KiroRefreshAllowsMissingDefaultConfig(t *testing.T) {
	workdir := t.TempDir()

	result, err := loadConfigResult(runtimeFlags{kiroRefresh: true}, startupContext{workdir: workdir}, storeSettings{})
	if err != nil {
		t.Fatalf("loadConfigResult() error = %v", err)
	}
	if result.cfg == nil {
		t.Fatal("expected empty config, got nil")
	}
	if result.configFilePath != filepath.Join(workdir, "config.yaml") {
		t.Fatalf("configFilePath = %q", result.configFilePath)
	}
}

func TestLoadConfigResult_XAILoginAllowsMissingDefaultConfig(t *testing.T) {
	workdir := t.TempDir()

	result, err := loadConfigResult(runtimeFlags{xaiLogin: true}, startupContext{workdir: workdir}, storeSettings{})
	if err != nil {
		t.Fatalf("loadConfigResult() error = %v", err)
	}
	if result.cfg == nil {
		t.Fatal("expected empty config, got nil")
	}
	if result.configFilePath != filepath.Join(workdir, "config.yaml") {
		t.Fatalf("configFilePath = %q", result.configFilePath)
	}
}

func TestLoadConfigResult_MissingDefaultConfigStillErrorsForServer(t *testing.T) {
	_, err := loadConfigResult(runtimeFlags{}, startupContext{workdir: t.TempDir()}, storeSettings{})
	if err == nil {
		t.Fatal("expected error for missing default config")
	}
}

func TestLoadConfigResult_KiroLoginExplicitMissingConfigStillErrors(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "missing.yaml")

	_, err := loadConfigResult(runtimeFlags{kiroLogin: true, configPath: configPath}, startupContext{workdir: t.TempDir()}, storeSettings{})
	if err == nil {
		t.Fatal("expected error for explicit missing config")
	}
}
