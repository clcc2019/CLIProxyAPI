package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClientAPIKeyEntry describes one client-facing API key that can authenticate
// requests sent to CLIProxyAPI. The key may optionally be restricted to a subset
// of client-visible model IDs.
type ClientAPIKeyEntry struct {
	APIKey         string   `yaml:"api-key" json:"api-key"`
	AllowedModels  []string `yaml:"allowed-models,omitempty" json:"allowed-models,omitempty"`
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
}

// ClientAPIKeys keeps backward compatibility with the historical
// `api-keys: ["k1", "k2"]` format while allowing richer object entries.
type ClientAPIKeys []ClientAPIKeyEntry

// SanitizeClientAPIKeys normalizes the configured client-facing API keys.
func (cfg *SDKConfig) SanitizeClientAPIKeys() {
	if cfg == nil {
		return
	}
	cfg.APIKeys = NormalizeClientAPIKeys(cfg.APIKeys)
}

// Values returns the plain API key values in order.
func (keys ClientAPIKeys) Values() []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, entry := range keys {
		if trimmed := strings.TrimSpace(entry.APIKey); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MarshalYAML emits plain strings when an entry has no extra settings, and an
// object entry otherwise.
func (keys ClientAPIKeys) MarshalYAML() (any, error) {
	if len(keys) == 0 {
		return []any{}, nil
	}
	out := make([]any, 0, len(keys))
	for _, raw := range keys {
		entry := normalizeClientAPIKeyEntry(raw)
		if entry.APIKey == "" {
			continue
		}
		if len(entry.AllowedModels) == 0 && len(entry.ExcludedModels) == 0 {
			out = append(out, entry.APIKey)
			continue
		}
		item := map[string]any{
			"api-key": entry.APIKey,
		}
		if len(entry.AllowedModels) > 0 {
			item["allowed-models"] = entry.AllowedModels
		}
		if len(entry.ExcludedModels) > 0 {
			item["excluded-models"] = entry.ExcludedModels
		}
		out = append(out, item)
	}
	return out, nil
}

// UnmarshalYAML accepts both string and object entries.
func (keys *ClientAPIKeys) UnmarshalYAML(value *yaml.Node) error {
	if keys == nil {
		return nil
	}
	if value == nil || value.Kind == 0 || value.Tag == "!!null" {
		*keys = nil
		return nil
	}
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("api-keys must be a sequence")
	}
	parsed := make(ClientAPIKeys, 0, len(value.Content))
	for _, item := range value.Content {
		if item == nil {
			continue
		}
		switch item.Kind {
		case yaml.ScalarNode:
			parsed = append(parsed, ClientAPIKeyEntry{APIKey: strings.TrimSpace(item.Value)})
		case yaml.MappingNode:
			var entry ClientAPIKeyEntry
			if err := item.Decode(&entry); err != nil {
				return err
			}
			parsed = append(parsed, entry)
		default:
			return fmt.Errorf("api-keys entries must be strings or objects")
		}
	}
	*keys = NormalizeClientAPIKeys(parsed)
	return nil
}

// MarshalJSON mirrors MarshalYAML for management API responses.
func (keys ClientAPIKeys) MarshalJSON() ([]byte, error) {
	serializable, err := keys.MarshalYAML()
	if err != nil {
		return nil, err
	}
	return json.Marshal(serializable)
}

// UnmarshalJSON accepts both string and object entries.
func (keys *ClientAPIKeys) UnmarshalJSON(data []byte) error {
	if keys == nil {
		return nil
	}
	var raw []any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	parsed := make(ClientAPIKeys, 0, len(raw))
	for _, item := range raw {
		switch typed := item.(type) {
		case string:
			parsed = append(parsed, ClientAPIKeyEntry{APIKey: strings.TrimSpace(typed)})
		case map[string]any:
			entry := ClientAPIKeyEntry{}
			if value, ok := typed["api-key"]; ok {
				entry.APIKey = strings.TrimSpace(fmt.Sprintf("%v", value))
			} else if value, ok := typed["apiKey"]; ok {
				entry.APIKey = strings.TrimSpace(fmt.Sprintf("%v", value))
			} else if value, ok := typed["key"]; ok {
				entry.APIKey = strings.TrimSpace(fmt.Sprintf("%v", value))
			}
			entry.AllowedModels = extractClientAPIKeyModels(typed, "allowed-models", "allowedModels")
			entry.ExcludedModels = extractClientAPIKeyModels(typed, "excluded-models", "excludedModels")
			parsed = append(parsed, entry)
		default:
			return fmt.Errorf("api-keys entries must be strings or objects")
		}
	}
	*keys = NormalizeClientAPIKeys(parsed)
	return nil
}

func extractClientAPIKeyModels(record map[string]any, names ...string) []string {
	for _, name := range names {
		raw, ok := record[name]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case []any:
			items := make([]string, 0, len(typed))
			for _, item := range typed {
				items = append(items, fmt.Sprintf("%v", item))
			}
			return NormalizeModelPatternList(items)
		case []string:
			return NormalizeModelPatternList(typed)
		case string:
			return NormalizeModelPatternList(strings.FieldsFunc(typed, func(r rune) bool {
				return r == ',' || r == '\n'
			}))
		}
	}
	return nil
}

func normalizeClientAPIKeyEntry(entry ClientAPIKeyEntry) ClientAPIKeyEntry {
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.AllowedModels = NormalizeModelPatternList(entry.AllowedModels)
	entry.ExcludedModels = NormalizeModelPatternList(entry.ExcludedModels)
	return entry
}

// NormalizeClientAPIKeys trims, deduplicates, and merges repeated api-key
// entries while preserving the order of first appearance.
func NormalizeClientAPIKeys(entries ClientAPIKeys) ClientAPIKeys {
	if len(entries) == 0 {
		return nil
	}
	out := make(ClientAPIKeys, 0, len(entries))
	indexByKey := make(map[string]int, len(entries))
	for _, raw := range entries {
		entry := normalizeClientAPIKeyEntry(raw)
		if entry.APIKey == "" {
			continue
		}
		key := entry.APIKey
		if index, exists := indexByKey[key]; exists {
			current := out[index]
			current.AllowedModels = mergeModelPatternLists(current.AllowedModels, entry.AllowedModels)
			current.ExcludedModels = mergeModelPatternLists(current.ExcludedModels, entry.ExcludedModels)
			out[index] = current
			continue
		}
		indexByKey[key] = len(out)
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeModelPatternLists(base, extra []string) []string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	return NormalizeModelPatternList(append(append([]string{}, base...), extra...))
}
