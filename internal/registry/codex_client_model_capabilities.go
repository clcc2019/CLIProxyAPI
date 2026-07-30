package registry

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type codexClientModelCapabilityPayload struct {
	Models []codexClientModelCapability `json:"models"`
	Data   []codexClientModelListEntry  `json:"data"`
}

// codexClientModelListEntry is the subset returned by the standard OpenAI
// GET /v1/models response. Custom API-key upstreams commonly expose this
// envelope instead of Codex's richer models/slug catalog.
type codexClientModelListEntry struct {
	ID string `json:"id"`
}

type codexClientModelCapability struct {
	Slug                              string  `json:"slug"`
	SupportsParallelToolCalls         *bool   `json:"supports_parallel_tool_calls"`
	SupportsReasoningSummaries        *bool   `json:"supports_reasoning_summaries"`
	SupportsReasoningSummaryParameter *bool   `json:"supports_reasoning_summary_parameter"`
	DefaultReasoningLevel             *string `json:"default_reasoning_level"`
	SupportedReasoningLevels          []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
	SupportVerbosity            *bool   `json:"support_verbosity"`
	DefaultVerbosity            *string `json:"default_verbosity"`
	UseResponsesLite            *bool   `json:"use_responses_lite"`
	SupportsImageDetailOriginal *bool   `json:"supports_image_detail_original"`
	ServiceTiers                []struct {
		ID string `json:"id"`
	} `json:"service_tiers"`
	DefaultServiceTier *string `json:"default_service_tier"`
	DisplayName        string  `json:"display_name"`
	Description        string  `json:"description"`
	ContextWindow      int     `json:"context_window"`
	MaxContextWindow   int     `json:"max_context_window"`
}

type CodexClientModelCapabilities struct {
	ModelSlug                         string
	SupportsParallelToolCalls         bool
	SupportsReasoningSummaries        bool
	SupportsReasoningSummaryParameter bool
	DefaultReasoningLevel             string
	SupportedReasoningLevels          []string
	SupportsVerbosity                 bool
	DefaultVerbosity                  string
	UseResponsesLite                  bool
	SupportsImageDetailOriginal       bool
	ServiceTiers                      []string
	DefaultServiceTier                string
}

// ParseCodexClientModelCatalog converts the account-scoped Codex /models
// response into registry model entries while preserving static metadata as a
// fallback for fields the remote catalog omits.
func ParseCodexClientModelCatalog(data []byte, fallback []*ModelInfo) ([]*ModelInfo, error) {
	var payload codexClientModelCapabilityPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse Codex model catalog: %w", err)
	}
	if payload.Models == nil && len(payload.Data) > 0 {
		payload.Models = make([]codexClientModelCapability, 0, len(payload.Data))
		for _, entry := range payload.Data {
			if id := strings.TrimSpace(entry.ID); id != "" {
				payload.Models = append(payload.Models, codexClientModelCapability{Slug: id})
			}
		}
	}

	fallbackByID := make(map[string]*ModelInfo, len(fallback))
	for _, model := range fallback {
		if model == nil {
			continue
		}
		if id := strings.TrimSpace(model.ID); id != "" {
			fallbackByID[id] = model
		}
	}

	models := make([]*ModelInfo, 0, len(payload.Models))
	for _, remote := range payload.Models {
		slug := strings.TrimSpace(remote.Slug)
		if slug == "" {
			continue
		}
		model := cloneModelInfo(fallbackByID[slug])
		if model == nil {
			model = &ModelInfo{
				ID:      slug,
				Object:  "model",
				OwnedBy: "openai",
				Type:    "openai",
				Name:    slug,
				Version: slug,
			}
		}
		model.ID = slug
		if displayName := strings.TrimSpace(remote.DisplayName); displayName != "" {
			model.DisplayName = displayName
		}
		if description := strings.TrimSpace(remote.Description); description != "" {
			model.Description = description
		}
		if remote.ContextWindow > 0 {
			model.ContextLength = remote.ContextWindow
			model.InputTokenLimit = remote.ContextWindow
		} else if remote.MaxContextWindow > 0 {
			model.ContextLength = remote.MaxContextWindow
			model.InputTokenLimit = remote.MaxContextWindow
		}
		capabilities := defaultCodexClientModelCapabilities(slug)
		if model.CodexCapabilities != nil {
			capabilities = cloneCodexClientModelCapabilities(*model.CodexCapabilities)
		} else if embedded, ok := CodexClientModelCapabilitiesForModel(slug); ok {
			capabilities = embedded
		}
		capabilities = overlayCodexClientModelCapabilities(capabilities, remote)
		model.CodexCapabilities = &capabilities
		models = append(models, model)
	}
	return models, nil
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

// GetCodexClientModelCapabilities returns the account-scoped capabilities
// registered for one auth. Aliased model IDs fall back to the original remote
// slug stored in the capability record.
func (r *ModelRegistry) GetCodexClientModelCapabilities(clientID, modelID string) (CodexClientModelCapabilities, bool) {
	clientID = strings.TrimSpace(clientID)
	modelID = strings.TrimSpace(modelID)
	if r == nil || clientID == "" || modelID == "" {
		return CodexClientModelCapabilities{}, false
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	infos := r.clientModelInfos[clientID]
	if info := infos[modelID]; info != nil && info.CodexCapabilities != nil {
		return cloneCodexClientModelCapabilities(*info.CodexCapabilities), true
	}
	for _, info := range infos {
		if info == nil || info.CodexCapabilities == nil {
			continue
		}
		if info.CodexCapabilities.ModelSlug == modelID {
			return cloneCodexClientModelCapabilities(*info.CodexCapabilities), true
		}
	}
	return CodexClientModelCapabilities{}, false
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
			codexClientModelCapabilityMap[slug] = codexClientModelCapabilities(model)
		}
	})
}

func codexClientModelCapabilities(model codexClientModelCapability) CodexClientModelCapabilities {
	return overlayCodexClientModelCapabilities(
		defaultCodexClientModelCapabilities(strings.TrimSpace(model.Slug)),
		model,
	)
}

func defaultCodexClientModelCapabilities(slug string) CodexClientModelCapabilities {
	return CodexClientModelCapabilities{
		ModelSlug:                         strings.TrimSpace(slug),
		SupportsReasoningSummaries:        true,
		SupportsReasoningSummaryParameter: true,
	}
}

// overlayCodexClientModelCapabilities applies only fields explicitly present
// in an account-scoped catalog record. Omitted fields retain the embedded or
// previously registered values so a partial /models response cannot silently
// switch the request wire protocol.
func overlayCodexClientModelCapabilities(base CodexClientModelCapabilities, model codexClientModelCapability) CodexClientModelCapabilities {
	base = cloneCodexClientModelCapabilities(base)
	if slug := strings.TrimSpace(model.Slug); slug != "" {
		base.ModelSlug = slug
	}
	if model.SupportsParallelToolCalls != nil {
		base.SupportsParallelToolCalls = *model.SupportsParallelToolCalls
	}
	if model.SupportsReasoningSummaries != nil {
		base.SupportsReasoningSummaries = *model.SupportsReasoningSummaries
	}
	if model.SupportsReasoningSummaryParameter != nil {
		base.SupportsReasoningSummaryParameter = *model.SupportsReasoningSummaryParameter
	}
	if model.DefaultReasoningLevel != nil {
		base.DefaultReasoningLevel = strings.TrimSpace(*model.DefaultReasoningLevel)
	}
	if model.SupportedReasoningLevels != nil {
		levels := make([]string, 0, len(model.SupportedReasoningLevels))
		seen := make(map[string]struct{}, len(model.SupportedReasoningLevels))
		for _, level := range model.SupportedReasoningLevels {
			effort := strings.TrimSpace(level.Effort)
			if effort == "" {
				continue
			}
			key := strings.ToLower(effort)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			levels = append(levels, effort)
		}
		base.SupportedReasoningLevels = levels
	}
	if model.SupportVerbosity != nil {
		base.SupportsVerbosity = *model.SupportVerbosity
	}
	if model.DefaultVerbosity != nil {
		base.DefaultVerbosity = strings.TrimSpace(*model.DefaultVerbosity)
	}
	if model.UseResponsesLite != nil {
		base.UseResponsesLite = *model.UseResponsesLite
	}
	if model.SupportsImageDetailOriginal != nil {
		base.SupportsImageDetailOriginal = *model.SupportsImageDetailOriginal
	}
	if model.ServiceTiers != nil {
		serviceTiers := make([]string, 0, len(model.ServiceTiers))
		for _, tier := range model.ServiceTiers {
			if id := strings.TrimSpace(tier.ID); id != "" {
				serviceTiers = append(serviceTiers, id)
			}
		}
		base.ServiceTiers = serviceTiers
	}
	if model.DefaultServiceTier != nil {
		base.DefaultServiceTier = strings.TrimSpace(*model.DefaultServiceTier)
	}
	return base
}

func cloneCodexClientModelCapabilities(capabilities CodexClientModelCapabilities) CodexClientModelCapabilities {
	if len(capabilities.SupportedReasoningLevels) > 0 {
		capabilities.SupportedReasoningLevels = append([]string(nil), capabilities.SupportedReasoningLevels...)
	}
	if len(capabilities.ServiceTiers) > 0 {
		capabilities.ServiceTiers = append([]string(nil), capabilities.ServiceTiers...)
	}
	return capabilities
}
