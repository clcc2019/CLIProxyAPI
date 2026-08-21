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
			if info.ContextLength != 272000 {
				t.Fatalf("context length = %d, want 272000", info.ContextLength)
			}
		})
	}

	info := LookupStaticModelInfo("gpt-5.5")
	if info == nil {
		t.Fatal("LookupStaticModelInfo did not find gpt-5.5")
	}
	if info.ContextLength != 272000 {
		t.Fatalf("lookup context length = %d, want 272000", info.ContextLength)
	}
}

func TestCodexStaticModelsIncludeGPT56Family(t *testing.T) {
	tests := []struct {
		name   string
		models []*ModelInfo
		ids    []string
	}{
		{name: "free", models: GetCodexFreeModels(), ids: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
		{name: "team", models: GetCodexTeamModels(), ids: []string{"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}},
		{name: "plus", models: GetCodexPlusModels(), ids: []string{"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}},
		{name: "pro", models: GetCodexProModels(), ids: []string{"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, id := range tt.ids {
				info := findModelInfo(tt.models, id)
				if info == nil {
					t.Fatalf("%s not found", id)
				}
				wantContextLength := 272000
				if id == "gpt-5.6" {
					wantContextLength = 372000
				}
				if info.ContextLength != wantContextLength {
					t.Fatalf("%s context length = %d, want %d", id, info.ContextLength, wantContextLength)
				}
			}
		})
	}

	info := LookupStaticModelInfo("gpt-5.6-sol")
	if info == nil {
		t.Fatal("LookupStaticModelInfo did not find gpt-5.6-sol")
	}
	if info.ContextLength != 272000 {
		t.Fatalf("lookup context length = %d, want 272000", info.ContextLength)
	}

	luna := findModelInfo(GetCodexFreeModels(), "gpt-5.6-luna")
	if luna == nil || luna.Config == nil {
		t.Fatal("gpt-5.6-luna model config was not loaded")
	}
	if got := luna.Config.OverrideHeader["user-agent"]; got != "codex-tui/0.144.1 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.1)" {
		t.Fatalf("gpt-5.6-luna user-agent override = %q", got)
	}
	if got := luna.Config.OverrideHeader["originator"]; got != "codex-tui" {
		t.Fatalf("gpt-5.6-luna originator override = %q, want codex-tui", got)
	}
	overrides := LookupModelHeaderOverrides("gpt-5.6-luna", "codex")
	if overrides.UserAgent != luna.Config.OverrideHeader["user-agent"] || overrides.Originator != "codex-tui" {
		t.Fatalf("static model header projection = %+v", overrides)
	}

	clone := cloneModelInfo(luna)
	clone.Config.OverrideHeader["originator"] = "mutated"
	if got := luna.Config.OverrideHeader["originator"]; got != "codex-tui" {
		t.Fatalf("model config clone shares override map, original originator = %q", got)
	}
}

func BenchmarkLookupModelHeaderOverrides(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		overrides := LookupModelHeaderOverrides("gpt-5.6-luna", "codex")
		if overrides.UserAgent == "" || overrides.Originator == "" {
			b.Fatal("missing model header overrides")
		}
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

func TestStaticProviderModelsIncludeLatestSyncedModels(t *testing.T) {
	tests := []struct {
		name   string
		models []*ModelInfo
		id     string
	}{
		{name: "claude opus 5", models: GetClaudeModels(), id: "claude-opus-5"},
		{name: "claude sonnet 5", models: GetClaudeModels(), id: "claude-sonnet-5"},
		{name: "claude fable 5", models: GetClaudeModels(), id: "claude-fable-5"},
		{name: "codex plus spark", models: GetCodexPlusModels(), id: "gpt-5.3-codex-spark"},
		{name: "kimi k2.7 code", models: GetKimiModels(), id: "kimi-k2.7-code"},
		{name: "kimi k2.7 code highspeed", models: GetKimiModels(), id: "kimi-k2.7-code-highspeed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if findModelInfo(tt.models, tt.id) == nil {
				t.Fatalf("%s not found", tt.id)
			}
		})
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
	if !capabilities.SupportsReasoningSummaryParameter {
		t.Fatal("gpt-5.4 should default to supporting the reasoning.summary parameter")
	}
	if capabilities.DefaultReasoningLevel != "medium" {
		t.Fatalf("default reasoning level = %q, want medium", capabilities.DefaultReasoningLevel)
	}
	if len(capabilities.SupportedReasoningLevels) == 0 {
		t.Fatal("gpt-5.4 should expose the embedded supported reasoning levels")
	}
	if !capabilities.SupportsVerbosity {
		t.Fatal("gpt-5.4 should support verbosity")
	}
	if capabilities.DefaultVerbosity != "low" {
		t.Fatalf("default verbosity = %q, want low", capabilities.DefaultVerbosity)
	}
	if capabilities.UseResponsesLite {
		t.Fatal("gpt-5.4 should not use responses_lite in the current official catalog")
	}
	if !capabilities.SupportsImageDetailOriginal {
		t.Fatal("gpt-5.4 should support original image detail")
	}
	if len(capabilities.ServiceTiers) != 1 || capabilities.ServiceTiers[0] != "priority" {
		t.Fatalf("service tiers = %#v, want [priority]", capabilities.ServiceTiers)
	}
	if capabilities.DefaultServiceTier != "" {
		t.Fatalf("default service tier = %q, want empty", capabilities.DefaultServiceTier)
	}

	gpt52Capabilities, ok := CodexClientModelCapabilitiesForModel("gpt-5.2")
	if !ok {
		t.Fatal("expected gpt-5.2 in embedded Codex client model catalog")
	}
	if gpt52Capabilities.SupportsImageDetailOriginal {
		t.Fatal("gpt-5.2 should not support original image detail")
	}

	if _, ok := CodexClientModelCapabilitiesForModel("unknown-model"); ok {
		t.Fatal("unknown model should not have embedded Codex client capabilities")
	}
}

func TestCodexClientModelCapabilitiesIncludeGPT56(t *testing.T) {
	capabilities, ok := CodexClientModelCapabilitiesForModel("gpt-5.6-sol")
	if !ok {
		t.Fatal("expected gpt-5.6-sol in embedded Codex client model catalog")
	}
	if !capabilities.SupportsParallelToolCalls {
		t.Fatal("gpt-5.6-sol should support parallel tool calls")
	}
	if capabilities.DefaultReasoningLevel != "low" {
		t.Fatalf("default reasoning level = %q, want low", capabilities.DefaultReasoningLevel)
	}
	if len(capabilities.ServiceTiers) != 2 || capabilities.ServiceTiers[0] != "priority" || capabilities.ServiceTiers[1] != "ultrafast" {
		t.Fatalf("service tiers = %#v, want [priority ultrafast]", capabilities.ServiceTiers)
	}
}

func TestParseCodexClientModelCatalogPreservesAccountScopedResponsesLite(t *testing.T) {
	models, err := ParseCodexClientModelCatalog([]byte(`{"models":[{"slug":"gpt-5.6-sol","display_name":"Account Sol","context_window":123456,"supports_parallel_tool_calls":false,"use_responses_lite":false,"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"medium"},{"effort":"high"}]}]}`), GetCodexProModels())
	if err != nil {
		t.Fatalf("ParseCodexClientModelCatalog: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("model count = %d, want 1", len(models))
	}
	model := models[0]
	if model.ID != "gpt-5.6-sol" || model.DisplayName != "Account Sol" || model.ContextLength != 123456 {
		t.Fatalf("parsed model = %#v", model)
	}
	if model.CodexCapabilities == nil {
		t.Fatal("missing account-scoped Codex capabilities")
	}
	if model.CodexCapabilities.UseResponsesLite {
		t.Fatal("account catalog should override the embedded responses_lite=true value")
	}
	if got, want := model.CodexCapabilities.SupportedReasoningLevels, []string{"low", "medium", "high"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("account catalog supported reasoning levels = %#v, want %#v", got, want)
	}

	registry := GetGlobalRegistry()
	const clientID = "test-account-scoped-codex-capabilities"
	registry.RegisterClient(clientID, "codex", models)
	t.Cleanup(func() { registry.UnregisterClient(clientID) })
	capabilities, ok := registry.GetCodexClientModelCapabilities(clientID, "gpt-5.6-sol")
	if !ok {
		t.Fatal("account-scoped capabilities were not registered")
	}
	if capabilities.UseResponsesLite || capabilities.SupportsParallelToolCalls {
		t.Fatalf("account-scoped capabilities = %#v", capabilities)
	}
}

func TestParseCodexClientModelCatalogPreservesEmbeddedCapabilitiesWhenRemoteFieldsAreOmitted(t *testing.T) {
	models, err := ParseCodexClientModelCatalog([]byte(`{"models":[{"slug":"gpt-5.6-sol","display_name":"Partial Account Sol"}]}`), GetCodexProModels())
	if err != nil {
		t.Fatalf("ParseCodexClientModelCatalog: %v", err)
	}
	if len(models) != 1 || models[0].CodexCapabilities == nil {
		t.Fatalf("parsed models = %#v", models)
	}

	got := models[0].CodexCapabilities
	want, ok := CodexClientModelCapabilitiesForModel("gpt-5.6-sol")
	if !ok {
		t.Fatal("missing embedded gpt-5.6-sol capabilities")
	}
	if got.ModelSlug != want.ModelSlug ||
		got.SupportsParallelToolCalls != want.SupportsParallelToolCalls ||
		got.SupportsReasoningSummaries != want.SupportsReasoningSummaries ||
		got.SupportsReasoningSummaryParameter != want.SupportsReasoningSummaryParameter ||
		got.DefaultReasoningLevel != want.DefaultReasoningLevel ||
		got.SupportsVerbosity != want.SupportsVerbosity ||
		got.DefaultVerbosity != want.DefaultVerbosity ||
		got.UseResponsesLite != want.UseResponsesLite ||
		got.SupportsImageDetailOriginal != want.SupportsImageDetailOriginal ||
		got.DefaultServiceTier != want.DefaultServiceTier {
		t.Fatalf("partial remote capabilities = %#v, want embedded %#v", *got, want)
	}
	if len(got.SupportedReasoningLevels) != len(want.SupportedReasoningLevels) {
		t.Fatalf("partial remote supported reasoning levels = %#v, want %#v", got.SupportedReasoningLevels, want.SupportedReasoningLevels)
	}
	for i := range want.SupportedReasoningLevels {
		if got.SupportedReasoningLevels[i] != want.SupportedReasoningLevels[i] {
			t.Fatalf("partial remote supported reasoning levels = %#v, want %#v", got.SupportedReasoningLevels, want.SupportedReasoningLevels)
		}
	}
	if len(got.ServiceTiers) != len(want.ServiceTiers) {
		t.Fatalf("partial remote service tiers = %#v, want %#v", got.ServiceTiers, want.ServiceTiers)
	}
	for i := range want.ServiceTiers {
		if got.ServiceTiers[i] != want.ServiceTiers[i] {
			t.Fatalf("partial remote service tiers = %#v, want %#v", got.ServiceTiers, want.ServiceTiers)
		}
	}
}

func TestParseCodexClientModelCatalogAcceptsStandardOpenAIList(t *testing.T) {
	models, err := ParseCodexClientModelCatalog([]byte(`{"object":"list","data":[{"id":"gpt-5.6-sol","object":"model"},{"id":"  "},{"id":"custom-codex-model"}]}`), GetCodexProModels())
	if err != nil {
		t.Fatalf("ParseCodexClientModelCatalog: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("model count = %d, want 2", len(models))
	}
	if models[0].ID != "gpt-5.6-sol" || models[1].ID != "custom-codex-model" {
		t.Fatalf("model IDs = %q, %q", models[0].ID, models[1].ID)
	}
	if models[0].CodexCapabilities == nil {
		t.Fatal("known model should retain embedded Codex capabilities")
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

func TestLookupStaticModelInfoIncludesXAIModels(t *testing.T) {
	info := LookupStaticModelInfo("grok-4.3")
	if info == nil {
		t.Fatal("LookupStaticModelInfo did not find grok-4.3")
	}
	if info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		t.Fatalf("grok-4.3 thinking metadata = %#v, want levels", info.Thinking)
	}
}
