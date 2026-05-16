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
    note: " Team B "
    disabled: true
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
			Note:           "Team B",
			Disabled:       true,
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
		{
			APIKey:         "key-b",
			Note:           "Production",
			Disabled:       true,
			AllowedModels:  []string{"gpt-5-*"},
			ExcludedModels: []string{"*-mini"},
			Quota:          ClientAPIKeyQuota{DailyCost: 1.5, MonthlyCost: 30},
		},
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
	if second["note"] != "Production" {
		t.Fatalf("unexpected note: %#v", second["note"])
	}
	if second["disabled"] != true {
		t.Fatalf("unexpected disabled: %#v", second["disabled"])
	}
	if !reflect.DeepEqual(second["allowed-models"], []string{"gpt-5-*"}) {
		t.Fatalf("unexpected allowed-models: %#v", second["allowed-models"])
	}
	if !reflect.DeepEqual(second["excluded-models"], []string{"*-mini"}) {
		t.Fatalf("unexpected excluded-models: %#v", second["excluded-models"])
	}
	quota, ok := second["quota"].(ClientAPIKeyQuota)
	if !ok {
		t.Fatalf("expected quota to be ClientAPIKeyQuota, got %T", second["quota"])
	}
	if quota.DailyCost != 1.5 || quota.MonthlyCost != 30 {
		t.Fatalf("unexpected quota: %#v", quota)
	}
}

func TestClientAPIKeysUnmarshalJSONCompatibility(t *testing.T) {
	input := []byte(`[
		"key-a",
		{
			"apiKey": "key-b",
			"note": "Team B",
			"disabled": true,
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
		{APIKey: "key-b", Note: "Team B", Disabled: true, AllowedModels: []string{"gpt-5-*"}, ExcludedModels: []string{"*-mini"}},
	}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("unexpected api keys: %#v", parsed)
	}
}

func TestClientAPIKeysDisabledCompatibility(t *testing.T) {
	type payload struct {
		APIKeys ClientAPIKeys `yaml:"api-keys"`
	}

	input := `
api-keys:
  - api-key: "disabled-key"
    disabled: true
  - api-key: "enabled-key"
    disabled: false
`

	var parsed payload
	if err := yaml.Unmarshal([]byte(input), &parsed); err != nil {
		t.Fatalf("yaml unmarshal failed: %v", err)
	}

	want := ClientAPIKeys{
		{APIKey: "disabled-key", Disabled: true},
		{APIKey: "enabled-key"},
	}
	if !reflect.DeepEqual(parsed.APIKeys, want) {
		t.Fatalf("unexpected api keys: %#v", parsed.APIKeys)
	}
}

func TestNormalizeClientAPIKeysMergesDisabled(t *testing.T) {
	keys := NormalizeClientAPIKeys(ClientAPIKeys{
		{APIKey: "merge-key"},
		{APIKey: "merge-key", Disabled: true},
	})

	want := ClientAPIKeys{{APIKey: "merge-key", Disabled: true}}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("unexpected normalized keys: %#v", keys)
	}
}

func TestClientAPIKeysQuotaCompatibility(t *testing.T) {
	type payload struct {
		APIKeys ClientAPIKeys `yaml:"api-keys"`
	}

	input := `
api-keys:
  - api-key: "quota-key"
    quota:
      daily-cost: 1.5
      monthly-cost: 30
      total-cost: -1
  - api-key: "quota-key"
    quota:
      daily-cost: 1
      total-cost: 100
`

	var parsed payload
	if err := yaml.Unmarshal([]byte(input), &parsed); err != nil {
		t.Fatalf("yaml unmarshal failed: %v", err)
	}

	want := ClientAPIKeys{{
		APIKey: "quota-key",
		Quota: ClientAPIKeyQuota{
			DailyCost:   1,
			MonthlyCost: 30,
			TotalCost:   100,
		},
	}}
	if !reflect.DeepEqual(parsed.APIKeys, want) {
		t.Fatalf("unexpected api keys: %#v", parsed.APIKeys)
	}
}

func TestClientAPIKeysQuotaJSONAliases(t *testing.T) {
	input := []byte(`[
		{
			"api-key": "quota-key",
			"quota": {
				"dailyCost": 1.25,
				"monthly-cost": "20",
				"totalSpend": 100
			}
		}
	]`)

	var parsed ClientAPIKeys
	if err := json.Unmarshal(input, &parsed); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	want := ClientAPIKeys{{
		APIKey: "quota-key",
		Quota: ClientAPIKeyQuota{
			DailyCost:   1.25,
			MonthlyCost: 20,
			TotalCost:   100,
		},
	}}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("unexpected api keys: %#v", parsed)
	}
}

func TestClientAPIKeysJSONNoteAliases(t *testing.T) {
	input := []byte(`[
		{"api-key": "note-key-a", "remark": "Remark label"},
		{"api-key": "note-key-b", "description": "Description label"}
	]`)

	var parsed ClientAPIKeys
	if err := json.Unmarshal(input, &parsed); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	want := ClientAPIKeys{
		{APIKey: "note-key-a", Note: "Remark label"},
		{APIKey: "note-key-b", Note: "Description label"},
	}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("unexpected api keys: %#v", parsed)
	}
}

func TestNormalizeClientAPIKeysMergesFirstNote(t *testing.T) {
	keys := NormalizeClientAPIKeys(ClientAPIKeys{
		{APIKey: "merge-key", Note: ""},
		{APIKey: "merge-key", Note: "owner"},
		{APIKey: "merge-key", Note: "later"},
	})

	want := ClientAPIKeys{{APIKey: "merge-key", Note: "owner"}}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("unexpected normalized keys: %#v", keys)
	}
}

func TestNormalizeClientAPIKeysDropsExamplePlaceholders(t *testing.T) {
	keys := NormalizeClientAPIKeys(ClientAPIKeys{
		{APIKey: "your-api-key-1", AllowedModels: []string{"gpt-*"}},
		{APIKey: " real-key "},
		{APIKey: "YOUR-API-KEY-2"},
		{APIKey: "your-api-key"},
	})

	want := ClientAPIKeys{{APIKey: "real-key"}}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("unexpected normalized keys: %#v", keys)
	}
}

func TestClientAPIKeysValuesDropsExamplePlaceholders(t *testing.T) {
	keys := ClientAPIKeys{
		{APIKey: "your-api-key-1"},
		{APIKey: " real-key "},
	}

	want := []string{"real-key"}
	if !reflect.DeepEqual(keys.Values(), want) {
		t.Fatalf("unexpected key values: %#v", keys.Values())
	}
}
