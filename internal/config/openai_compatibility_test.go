package config

import (
	"os"
	"testing"
)

func TestResolveOpenAICompatibilityFallsBackToBaseURL(t *testing.T) {
	entries := []OpenAICompatibility{
		{Name: "named", BaseURL: "https://named.example/v1"},
		{BaseURL: "https://anonymous.example/v1/"},
	}

	got := ResolveOpenAICompatibility(
		entries,
		"openai-compatibility",
		"",
		"openai-compatibility",
		"https://anonymous.example/v1",
	)
	if got == nil || got.BaseURL != entries[1].BaseURL {
		t.Fatalf("resolved entry = %#v, want anonymous provider", got)
	}
}

func TestResolveOpenAICompatibilityPrefersExplicitName(t *testing.T) {
	entries := []OpenAICompatibility{
		{Name: "first", BaseURL: "https://shared.example/v1"},
		{Name: "second", BaseURL: "https://shared.example/v1"},
	}

	got := ResolveOpenAICompatibility(entries, "second", "second", "second", entries[0].BaseURL)
	if got == nil || got.Name != "second" {
		t.Fatalf("resolved entry = %#v, want second", got)
	}
}

func TestResolveOpenAICompatibilityUsesBaseURLToDisambiguateDuplicateNames(t *testing.T) {
	entries := []OpenAICompatibility{
		{Name: "provider", BaseURL: "https://first.example/v1"},
		{Name: "provider", BaseURL: "https://second.example/v1"},
	}

	got := ResolveOpenAICompatibility(entries, "provider", "provider", "provider", entries[1].BaseURL)
	if got == nil || got.BaseURL != entries[1].BaseURL {
		t.Fatalf("resolved entry = %#v, want second base URL", got)
	}
}

func TestLoadConfigMigratesLegacySingleOpenAICompatibilityKeyInMemory(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	content := []byte("openai-compatibility:\n  - base-url: https://example.com/v1\n    api-key: legacy-key\n    models:\n      - name: upstream\n        alias: public\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OpenAICompatibility) != 1 || len(cfg.OpenAICompatibility[0].APIKeyEntries) != 1 {
		t.Fatalf("migrated config = %#v", cfg.OpenAICompatibility)
	}
	if got := cfg.OpenAICompatibility[0].APIKeyEntries[0].APIKey; got != "legacy-key" {
		t.Fatalf("api key = %q, want legacy-key", got)
	}
}
