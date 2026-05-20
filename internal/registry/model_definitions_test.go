package registry

import "testing"

func TestKiroStaticModelsReturnsEmptyAfterDynamicOnlyMigration(t *testing.T) {
	// Kiro now pulls its model catalog directly from the upstream API using the
	// auth's access token (see sdk/cliproxy/service.go fetchKiroModels). The
	// hard-coded static list was removed so the advertised catalog always
	// matches what the upstream accepts for this account. GetKiroModels is
	// kept as a stub that returns a non-nil empty slice so callers iterating
	// over known channels keep working.
	models := GetKiroModels()
	if models == nil {
		t.Fatalf("GetKiroModels() = nil, want non-nil empty slice")
	}
	if len(models) != 0 {
		t.Fatalf("GetKiroModels() = %d entries, want 0 (dynamic-only)", len(models))
	}
}

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
		Claude:      models,
		Gemini:      models,
		Vertex:      models,
		GeminiCLI:   models,
		AIStudio:    models,
		CodexFree:   models,
		CodexTeam:   models,
		CodexPlus:   models,
		CodexPro:    models,
		Kimi:        models,
		Antigravity: models,
		XAI:         models,
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
