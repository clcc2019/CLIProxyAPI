package config

import (
	"encoding/json"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClientAPIKeysUnmarshalYAMLCompatibility(t *testing.T) {
	type payload struct {
		APIKeys ClientAPIKeys `yaml:"api-keys"`
	}

	input := `
api-keys:
  - " key-a "
  - api-key: "key-b"
    allowed-models:
      - " GPT-5-* "
      - "gpt-5-*"
    excluded-models:
      - "*"
      - "gpt-5-mini"
  - api-key: "key-b"
    allowed-models:
      - "claude-*"
`

	var parsed payload
	if err := yaml.Unmarshal([]byte(input), &parsed); err != nil {
		t.Fatalf("yaml unmarshal failed: %v", err)
	}

	want := ClientAPIKeys{
		{APIKey: "key-a"},
		{
			APIKey:         "key-b",
			AllowedModels:  []string{"gpt-5-*", "claude-*"},
			ExcludedModels: []string{"*", "gpt-5-mini"},
		},
	}
	if !reflect.DeepEqual(parsed.APIKeys, want) {
		t.Fatalf("unexpected api keys: %#v", parsed.APIKeys)
	}
}

func TestClientAPIKeysMarshalYAMLPreservesLegacyShape(t *testing.T) {
	keys := ClientAPIKeys{
		{APIKey: "key-a"},
		{APIKey: "key-b", AllowedModels: []string{"gpt-5-*"}, ExcludedModels: []string{"*-mini"}},
	}

	serialized, err := keys.MarshalYAML()
	if err != nil {
		t.Fatalf("marshal yaml failed: %v", err)
	}

	items, ok := serialized.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", serialized)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if first, ok := items[0].(string); !ok || first != "key-a" {
		t.Fatalf("expected first item to stay string, got %#v", items[0])
	}
	second, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("expected second item to be map, got %T", items[1])
	}
	if second["api-key"] != "key-b" {
		t.Fatalf("unexpected api-key: %#v", second["api-key"])
	}
	if !reflect.DeepEqual(second["allowed-models"], []string{"gpt-5-*"}) {
		t.Fatalf("unexpected allowed-models: %#v", second["allowed-models"])
	}
	if !reflect.DeepEqual(second["excluded-models"], []string{"*-mini"}) {
		t.Fatalf("unexpected excluded-models: %#v", second["excluded-models"])
	}
}

func TestClientAPIKeysUnmarshalJSONCompatibility(t *testing.T) {
	input := []byte(`[
		"key-a",
		{
			"apiKey": "key-b",
			"allowedModels": ["GPT-5-*"],
			"excluded-models": ["*-mini"]
		}
	]`)

	var parsed ClientAPIKeys
	if err := json.Unmarshal(input, &parsed); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	want := ClientAPIKeys{
		{APIKey: "key-a"},
		{APIKey: "key-b", AllowedModels: []string{"gpt-5-*"}, ExcludedModels: []string{"*-mini"}},
	}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("unexpected api keys: %#v", parsed)
	}
}
