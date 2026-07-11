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
	Slug                              string  `json:"slug"`
	SupportsParallelToolCalls         *bool   `json:"supports_parallel_tool_calls"`
	SupportsReasoningSummaries        *bool   `json:"supports_reasoning_summaries"`
	SupportsReasoningSummaryParameter *bool   `json:"supports_reasoning_summary_parameter"`
	DefaultReasoningLevel             *string `json:"default_reasoning_level"`
	SupportVerbosity                  *bool   `json:"support_verbosity"`
	DefaultVerbosity                  *string `json:"default_verbosity"`
	UseResponsesLite                  *bool   `json:"use_responses_lite"`
	SupportsImageDetailOriginal       *bool   `json:"supports_image_detail_original"`
	ServiceTiers                      []struct {
		ID string `json:"id"`
	} `json:"service_tiers"`
	DefaultServiceTier *string `json:"default_service_tier"`
}

type CodexClientModelCapabilities struct {
	SupportsParallelToolCalls         bool
	SupportsReasoningSummaries        bool
	SupportsReasoningSummaryParameter bool
	DefaultReasoningLevel             string
	SupportsVerbosity                 bool
	DefaultVerbosity                  string
	UseResponsesLite                  bool
	SupportsImageDetailOriginal       bool
	ServiceTiers                      []string
	DefaultServiceTier                string
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
			serviceTiers := make([]string, 0, len(model.ServiceTiers))
			for _, tier := range model.ServiceTiers {
				id := strings.TrimSpace(tier.ID)
				if id != "" {
					serviceTiers = append(serviceTiers, id)
				}
			}
			codexClientModelCapabilityMap[slug] = CodexClientModelCapabilities{
				SupportsParallelToolCalls: boolPtrValue(model.SupportsParallelToolCalls),
				// The current Codex catalog no longer emits the legacy
				// supports_reasoning_summaries field. Older catalogs used it as
				// an opt-out capability, so omission retains the previous true
				// default while request shaping follows the newer summary-parameter
				// capability below.
				SupportsReasoningSummaries:        boolPtrValueDefault(model.SupportsReasoningSummaries, true),
				SupportsReasoningSummaryParameter: boolPtrValueDefault(model.SupportsReasoningSummaryParameter, true),
				DefaultReasoningLevel:             stringPtrValue(model.DefaultReasoningLevel),
				SupportsVerbosity:                 boolPtrValue(model.SupportVerbosity),
				DefaultVerbosity:                  stringPtrValue(model.DefaultVerbosity),
				UseResponsesLite:                  boolPtrValue(model.UseResponsesLite),
				SupportsImageDetailOriginal:       boolPtrValue(model.SupportsImageDetailOriginal),
				ServiceTiers:                      serviceTiers,
				DefaultServiceTier:                stringPtrValue(model.DefaultServiceTier),
			}
		}
	})
}

func boolPtrValue(value *bool) bool {
	return value != nil && *value
}

func boolPtrValueDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
