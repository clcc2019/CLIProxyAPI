package registry

import (
	"encoding/json"
	"strings"
	"sync"
)

type codexClientModelCapabilityPayload struct {
	Models []codexClientModelCapability `json:"models"`
}

type codexClientModelCapability struct {
	Slug                       string  `json:"slug"`
	SupportsParallelToolCalls  *bool   `json:"supports_parallel_tool_calls"`
	SupportsReasoningSummaries *bool   `json:"supports_reasoning_summaries"`
	DefaultReasoningLevel      *string `json:"default_reasoning_level"`
	SupportVerbosity           *bool   `json:"support_verbosity"`
	DefaultVerbosity           *string `json:"default_verbosity"`
}

type CodexClientModelCapabilities struct {
	SupportsParallelToolCalls  bool
	SupportsReasoningSummaries bool
	DefaultReasoningLevel      string
	SupportsVerbosity          bool
	DefaultVerbosity           string
}

var (
	codexClientModelCapabilityOnce sync.Once
	codexClientModelCapabilityMap  map[string]CodexClientModelCapabilities
	codexClientModelCapabilityErr  error
)

// CodexClientModelCapabilitiesForModel returns the embedded official Codex
// model catalog capabilities when the model slug is known.
func CodexClientModelCapabilitiesForModel(modelID string) (CodexClientModelCapabilities, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return CodexClientModelCapabilities{}, false
	}

	loadCodexClientModelCapabilities()
	if codexClientModelCapabilityErr != nil {
		return CodexClientModelCapabilities{}, false
	}
	capabilities, ok := codexClientModelCapabilityMap[modelID]
	return capabilities, ok
}

// CodexClientModelSupportsParallelToolCalls returns the official Codex model
// catalog parallel-tool-call capability when the embedded catalog knows it.
func CodexClientModelSupportsParallelToolCalls(modelID string) (bool, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false, false
	}

	capabilities, ok := CodexClientModelCapabilitiesForModel(modelID)
	if !ok {
		return false, false
	}
	return capabilities.SupportsParallelToolCalls, true
}

func loadCodexClientModelCapabilities() {
	codexClientModelCapabilityOnce.Do(func() {
		var payload codexClientModelCapabilityPayload
		codexClientModelCapabilityErr = json.Unmarshal(GetCodexClientModelsJSON(), &payload)
		if codexClientModelCapabilityErr != nil {
			return
		}

		codexClientModelCapabilityMap = make(map[string]CodexClientModelCapabilities, len(payload.Models))
		for _, model := range payload.Models {
			slug := strings.TrimSpace(model.Slug)
			if slug == "" {
				continue
			}
			codexClientModelCapabilityMap[slug] = CodexClientModelCapabilities{
				SupportsParallelToolCalls:  boolPtrValue(model.SupportsParallelToolCalls),
				SupportsReasoningSummaries: boolPtrValue(model.SupportsReasoningSummaries),
				DefaultReasoningLevel:      stringPtrValue(model.DefaultReasoningLevel),
				SupportsVerbosity:          boolPtrValue(model.SupportVerbosity),
				DefaultVerbosity:           stringPtrValue(model.DefaultVerbosity),
			}
		}
	})
}

func boolPtrValue(value *bool) bool {
	return value != nil && *value
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
