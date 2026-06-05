package registry

import "testing"

func TestCodexStaticModelsIncludeGPT55WithExpectedContextLength(t *testing.T) {
	tests := []struct {
		name   string
		models []*ModelInfo
	}{
		{name: "team", models: GetCodexTeamModels()},
		{name: "plus", models: GetCodexPlusModels()},
		{name: "pro", models: GetCodexProModels()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := findModelInfo(tt.models, "gpt-5.5")
			if info == nil {
				t.Fatal("gpt-5.5 not found")
			}
			if info.ContextLength != 1050000 {
				t.Fatalf("context length = %d, want 1050000", info.ContextLength)
			}
		})
	}

	info := LookupStaticModelInfo("gpt-5.5")
	if info == nil {
		t.Fatal("LookupStaticModelInfo did not find gpt-5.5")
	}
	if info.ContextLength != 1050000 {
		t.Fatalf("lookup context length = %d, want 1050000", info.ContextLength)
	}
}

func TestCodexFreeStaticModelsIncludeGPT55WithExpectedContextLength(t *testing.T) {
	info := findModelInfo(GetCodexFreeModels(), "gpt-5.5")
	if info == nil {
		t.Fatal("gpt-5.5 not found in codex-free")
	}
	if info.ContextLength != 272000 {
		t.Fatalf("context length = %d, want 272000", info.ContextLength)
	}
}

func TestCodexClientModelSupportsParallelToolCallsUsesEmbeddedCatalog(t *testing.T) {
	supported, ok := CodexClientModelSupportsParallelToolCalls("gpt-5.4")
	if !ok {
		t.Fatal("expected gpt-5.4 in embedded Codex client model catalog")
	}
	if !supported {
		t.Fatal("gpt-5.4 should support parallel tool calls")
	}

	if _, ok := CodexClientModelSupportsParallelToolCalls("unknown-model"); ok {
		t.Fatal("unknown model should not have embedded Codex client capabilities")
	}
}

func TestCodexClientModelCapabilitiesForModelUsesEmbeddedCatalog(t *testing.T) {
	capabilities, ok := CodexClientModelCapabilitiesForModel("gpt-5.4")
	if !ok {
		t.Fatal("expected gpt-5.4 in embedded Codex client model catalog")
	}
	if !capabilities.SupportsParallelToolCalls {
		t.Fatal("gpt-5.4 should support parallel tool calls")
	}
	if !capabilities.SupportsReasoningSummaries {
		t.Fatal("gpt-5.4 should support reasoning summaries")
	}
	if capabilities.DefaultReasoningLevel != "xhigh" {
		t.Fatalf("default reasoning level = %q, want xhigh", capabilities.DefaultReasoningLevel)
	}
	if !capabilities.SupportsVerbosity {
		t.Fatal("gpt-5.4 should support verbosity")
	}
	if capabilities.DefaultVerbosity != "low" {
		t.Fatalf("default verbosity = %q, want low", capabilities.DefaultVerbosity)
	}

	if _, ok := CodexClientModelCapabilitiesForModel("unknown-model"); ok {
		t.Fatal("unknown model should not have embedded Codex client capabilities")
	}
}

func TestWithXAIBuiltinsAddsVideoModel(t *testing.T) {
	models := WithXAIBuiltins(nil)
	found := false
	for _, model := range models {
		if model != nil && model.ID == xaiBuiltinVideoModelID {
			found = true
			if model.OwnedBy != "xai" {
				t.Fatalf("OwnedBy = %q, want xai", model.OwnedBy)
			}
		}
	}
	if !found {
		t.Fatalf("expected %s builtin model", xaiBuiltinVideoModelID)
	}
}

func TestStaticModelDefinitionsByChannelSupportsXAI(t *testing.T) {
	for _, channel := range []string{"xai", "x-ai", "grok"} {
		t.Run(channel, func(t *testing.T) {
			info := findModelInfo(GetStaticModelDefinitionsByChannel(channel), xaiBuiltinImageModelID)
			if info == nil {
				t.Fatalf("expected %s in static models for %s", xaiBuiltinImageModelID, channel)
			}
		})
	}
}

func TestValidateModelsCatalogAllowsMissingSections(t *testing.T) {
	data := validTestModelsCatalog()
	data.XAI = nil

	if err := validateModelsCatalog(data); err != nil {
		t.Fatalf("validateModelsCatalog() error = %v", err)
	}
}

func TestValidateModelsCatalogRejectsInvalidDefinitions(t *testing.T) {
	data := validTestModelsCatalog()
	data.Claude = []*ModelInfo{{ID: ""}}

	if err := validateModelsCatalog(data); err == nil {
		t.Fatal("expected invalid model definition error")
	}
}

func validTestModelsCatalog() *staticModelsJSON {
	models := []*ModelInfo{{ID: "test-model"}}
	return &staticModelsJSON{
		Claude:    models,
		CodexFree: models,
		CodexTeam: models,
		CodexPlus: models,
		CodexPro:  models,
		Kimi:      models,
		XAI:       models,
	}
}

func findModelInfo(models []*ModelInfo, id string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

func TestWithXAIBuiltinsIncludesVideoPreviewModel(t *testing.T) {
	models := WithXAIBuiltins(nil)

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15PreviewModelID {
			return
		}
	}

	t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15PreviewModelID)
}
