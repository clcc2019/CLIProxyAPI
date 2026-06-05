// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"strings"
	"sync"
	"sync/atomic"
)

const (
	codexBuiltinImageModelID        = "gpt-image-2"
	xaiBuiltinImageModelID          = "grok-imagine-image"
	xaiBuiltinImageQualityModelID   = "grok-imagine-image-quality"
	xaiBuiltinVideoModelID          = "grok-imagine-video"
	xaiBuiltinVideo15PreviewModelID = "grok-imagine-video-1.5-preview"
)

var codexBuiltinImageModels = []struct {
	id          string
	displayName string
	created     int64
}{
	{id: "gpt-image-1", displayName: "GPT Image 1", created: 1733875200},
	{id: "gpt-image-1.5", displayName: "GPT Image 1.5", created: 1735689600},
	{id: "gpt-image-2", displayName: "GPT Image 2", created: 1738368000},
}

type staticModelLookupSnapshot struct {
	data *staticModelsJSON
	byID map[string]*ModelInfo
}

var (
	staticModelLookupCache atomic.Value // stores *staticModelLookupSnapshot
	staticModelLookupMu    sync.Mutex
)

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude    []*ModelInfo `json:"claude"`
	CodexFree []*ModelInfo `json:"codex-free"`
	CodexTeam []*ModelInfo `json:"codex-team"`
	CodexPlus []*ModelInfo `json:"codex-plus"`
	CodexPro  []*ModelInfo `json:"codex-pro"`
	Kimi      []*ModelInfo `json:"kimi"`
	XAI       []*ModelInfo `json:"xai"`
}

// GetClaudeModels returns the standard Claude model definitions.
func GetClaudeModels() []*ModelInfo {
	return cloneModelInfos(getModels().Claude)
}

// GetCodexFreeModels returns model definitions for the Codex free plan tier.
func GetCodexFreeModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexFree))
}

// GetCodexTeamModels returns model definitions for the Codex team plan tier.
func GetCodexTeamModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexTeam))
}

// GetCodexPlusModels returns model definitions for the Codex plus plan tier.
func GetCodexPlusModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexPlus))
}

// GetCodexProModels returns model definitions for the Codex pro plan tier.
func GetCodexProModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexPro))
}

// GetKimiModels returns the standard Kimi (Moonshot AI) model definitions.
func GetKimiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Kimi)
}

// GetXAIModels returns the standard xAI Grok model definitions.
func GetXAIModels() []*ModelInfo {
	return WithXAIBuiltins(cloneModelInfos(getModels().XAI))
}

// WithCodexBuiltins injects hard-coded Codex-only model definitions that should
// not depend on remote models.json updates. Built-ins replace any matching IDs
// already present in the provided slice.
func WithCodexBuiltins(models []*ModelInfo) []*ModelInfo {
	extras := make([]*ModelInfo, 0, len(codexBuiltinImageModels))
	for _, model := range codexBuiltinImageModels {
		extras = append(extras, codexBuiltinImageModelInfo(model.id, model.displayName, model.created))
	}
	return upsertModelInfos(models, extras...)
}

// WithXAIBuiltins injects hard-coded xAI image/video model definitions that should
// not depend on remote models.json updates.
func WithXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, xaiBuiltinImageModelInfo(), xaiBuiltinImageQualityModelInfo(), xaiBuiltinVideoModelInfo(), xaiBuiltinVideo15PreviewModelInfo())
}

func codexBuiltinImageModelInfo(id string, displayName string, created int64) *ModelInfo {
	return &ModelInfo{
		ID:          id,
		Object:      "model",
		Created:     created,
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: displayName,
		Version:     id,
	}
}

func xaiBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image",
		Name:        xaiBuiltinImageModelID,
		Description: "xAI Grok image generation model.",
	}
}

func xaiBuiltinImageQualityModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageQualityModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image Quality",
		Name:        xaiBuiltinImageQualityModelID,
		Description: "xAI Grok higher-fidelity image generation model.",
	}
}

func xaiBuiltinVideoModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideoModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video",
		Name:        xaiBuiltinVideoModelID,
		Description: "xAI Grok video generation model.",
	}
}

func xaiBuiltinVideo15PreviewModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideo15PreviewModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video 1.5 Preview",
		Name:        xaiBuiltinVideo15PreviewModelID,
		Description: "xAI Grok preview video generation model.",
	}
}

func upsertModelInfos(models []*ModelInfo, extras ...*ModelInfo) []*ModelInfo {
	if len(extras) == 0 {
		return models
	}

	extraIDs := make(map[string]struct{}, len(extras))
	extraList := make([]*ModelInfo, 0, len(extras))
	for _, extra := range extras {
		if extra == nil {
			continue
		}
		id := strings.TrimSpace(extra.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := extraIDs[key]; exists {
			continue
		}
		extraIDs[key] = struct{}{}
		extraList = append(extraList, cloneModelInfo(extra))
	}

	if len(extraList) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models)+len(extraList))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := extraIDs[strings.ToLower(id)]; exists {
			continue
		}
		filtered = append(filtered, model)
	}

	filtered = append(filtered, extraList...)
	return filtered
}

// cloneModelInfos returns a shallow copy of the slice with each element deep-cloned.
func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - codex
//   - kimi
//   - xai
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "codex":
		return GetCodexProModels()
	case "kimi":
		return GetKimiModels()
	case "xai", "x-ai", "grok":
		return GetXAIModels()
	default:
		return nil
	}
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	data := getModels()
	if data == nil {
		return nil
	}
	snapshot := staticModelLookupSnapshotFor(data)
	if snapshot == nil {
		return nil
	}
	return cloneModelInfo(snapshot.byID[modelID])
}

func staticModelLookupSnapshotFor(data *staticModelsJSON) *staticModelLookupSnapshot {
	if data == nil {
		return nil
	}
	if cached, ok := staticModelLookupCache.Load().(*staticModelLookupSnapshot); ok && cached != nil && cached.data == data {
		return cached
	}

	staticModelLookupMu.Lock()
	defer staticModelLookupMu.Unlock()
	if cached, ok := staticModelLookupCache.Load().(*staticModelLookupSnapshot); ok && cached != nil && cached.data == data {
		return cached
	}

	snapshot := &staticModelLookupSnapshot{
		data: data,
		byID: buildStaticModelLookup(data),
	}
	staticModelLookupCache.Store(snapshot)
	return snapshot
}

func buildStaticModelLookup(data *staticModelsJSON) map[string]*ModelInfo {
	if data == nil {
		return nil
	}
	allModels := [][]*ModelInfo{
		data.Claude,
		data.CodexPro,
		data.Kimi,
	}
	total := 0
	for _, models := range allModels {
		total += len(models)
	}
	byID := make(map[string]*ModelInfo, total)
	for _, models := range allModels {
		for _, m := range models {
			if m == nil || m.ID == "" {
				continue
			}
			if _, exists := byID[m.ID]; exists {
				continue
			}
			byID[m.ID] = m
		}
	}
	return byID
}
