package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

// openAICompatRuntimeSnapshot contains only the immutable fields needed by
// request-time API-key routing. Keeping this separate from Config avoids
// scanning and adapting the complete config on every request.
type openAICompatRuntimeSnapshot struct {
	entries  map[string]*openAICompatRuntimeEntry
	baseURLs map[string]*openAICompatRuntimeEntry
}

type openAICompatRuntimeEntry struct {
	order      int
	poolMode   bool
	aliasPools map[string][]string
	nameModels map[string]string
}

func buildOpenAICompatRuntimeSnapshot(cfg *internalconfig.Config) *openAICompatRuntimeSnapshot {
	snapshot := &openAICompatRuntimeSnapshot{}
	if cfg == nil || len(cfg.OpenAICompatibility) == 0 {
		return snapshot
	}

	snapshot.entries = make(map[string]*openAICompatRuntimeEntry, len(cfg.OpenAICompatibility))
	snapshot.baseURLs = make(map[string]*openAICompatRuntimeEntry, len(cfg.OpenAICompatibility))
	for index := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[index]
		if compat.Disabled {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(compat.Name))

		aliasPools := make(map[string][]string)
		nameModels := make(map[string]string)
		for modelIndex := range compat.Models {
			model := compat.Models[modelIndex]
			modelName := strings.TrimSpace(model.Name)
			modelAlias := strings.TrimSpace(model.Alias)
			if modelName == "" && modelAlias == "" {
				continue
			}

			if modelName != "" {
				nameKey := strings.ToLower(modelName)
				if _, exists := nameModels[nameKey]; !exists {
					nameModels[nameKey] = modelName
				}
			}
			if modelAlias != "" {
				resolved := modelName
				if resolved == "" {
					resolved = modelAlias
				}
				aliasKey := strings.ToLower(modelAlias)
				if !containsFolded(aliasPools[aliasKey], resolved) {
					aliasPools[aliasKey] = append(aliasPools[aliasKey], resolved)
				}
			}
		}

		entry := &openAICompatRuntimeEntry{
			order:      index,
			poolMode:   compat.PoolMode,
			aliasPools: nilIfEmptyStringSliceMap(aliasPools),
			nameModels: nilIfEmptyStringMap(nameModels),
		}
		if name != "" {
			if _, exists := snapshot.entries[name]; !exists {
				snapshot.entries[name] = entry
			}
		}
		baseURL := normalizeOpenAICompatRuntimeBaseURL(compat.BaseURL)
		if baseURL != "" {
			if _, exists := snapshot.baseURLs[baseURL]; !exists {
				snapshot.baseURLs[baseURL] = entry
			}
		}
	}
	if len(snapshot.entries) == 0 {
		snapshot.entries = nil
	}
	if len(snapshot.baseURLs) == 0 {
		snapshot.baseURLs = nil
	}
	return snapshot
}

func normalizeOpenAICompatRuntimeBaseURL(value string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(value), "/"))
}

func containsFolded(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func nilIfEmptyStringSliceMap(values map[string][]string) map[string][]string {
	if len(values) == 0 {
		return nil
	}
	return values
}

func nilIfEmptyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	return values
}

func (s *openAICompatRuntimeSnapshot) resolve(providerKey, compatName, authProvider, baseURL string) *openAICompatRuntimeEntry {
	if s == nil {
		return nil
	}

	var selected *openAICompatRuntimeEntry
	candidates := []string{compatName}
	for _, candidate := range []string{providerKey, authProvider} {
		if !strings.EqualFold(strings.TrimSpace(candidate), "openai-compatibility") {
			candidates = append(candidates, candidate)
		}
	}
	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		entry := s.entries[key]
		if entry == nil || (selected != nil && entry.order >= selected.order) {
			continue
		}
		selected = entry
	}
	if entry := s.baseURLs[normalizeOpenAICompatRuntimeBaseURL(baseURL)]; entry != nil {
		return entry
	}
	return selected
}

func (e *openAICompatRuntimeEntry) resolveModelPool(requestedModel string) []string {
	if e == nil {
		return nil
	}
	requestResult, candidates := modelAliasLookupCandidates(requestedModel)
	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		if targets := e.aliasPools[key]; len(targets) > 0 {
			return preserveRuntimeModelPoolSuffix(targets, requestResult)
		}
	}

	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		if name := strings.TrimSpace(e.nameModels[key]); name != "" {
			return []string{preserveResolvedModelSuffix(name, requestResult)}
		}
	}
	return nil
}

func preserveRuntimeModelPoolSuffix(targets []string, requestResult thinking.SuffixResult) []string {
	if len(targets) == 0 {
		return nil
	}
	if !requestResult.HasSuffix {
		return targets
	}
	out := make([]string, len(targets))
	for i, target := range targets {
		out[i] = preserveResolvedModelSuffix(target, requestResult)
	}
	return out
}
