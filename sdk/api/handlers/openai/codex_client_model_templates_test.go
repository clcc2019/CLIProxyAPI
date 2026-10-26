package openai

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestCodexClientModelTemplatesWithoutLegacyDefault(t *testing.T) {
	for _, idField := range []string{"slug", "id"} {
		t.Run(idField, func(t *testing.T) {
			var cache codexClientModelTemplateCache
			data := []byte(fmt.Sprintf(`{"models":[{%q:"gpt-6.1-sol","display_name":"GPT-6.1-Sol","visibility":"list","supported_in_api":true,"supported_reasoning_levels":[{"effort":"ultra"}],"default_reasoning_level":"ultra"}]}`, idField))
			templates, defaults, err := cache.load(data, 1)
			if err != nil || defaults == nil {
				t.Fatalf("load catalog without gpt-5.5: defaults=%v, err=%v", defaults, err)
			}
			models := buildCodexClientModelsWithTemplates([]map[string]any{{"id": "gpt-6.1-sol"}, {"id": "custom-model"}}, templates, defaults)
			if len(models) != 2 {
				t.Fatalf("models = %#v, want two models", models)
			}
			bySlug := make(map[string]map[string]any)
			for _, model := range models {
				bySlug[stringModelValue(model, "slug")] = model
				for _, field := range []string{"shell_type", "truncation_policy", "input_modalities"} {
					if model[field] == nil {
						t.Errorf("model %v missing built-in default %s", model["slug"], field)
					}
				}
			}
			sol := bySlug["gpt-6.1-sol"]
			if sol == nil || sol["visibility"] != "list" || sol["display_name"] != "GPT-6.1-Sol" || sol["default_reasoning_level"] != "ultra" {
				t.Fatalf("remote metadata not preserved: %#v", sol)
			}
			if bySlug["custom-model"] == nil {
				t.Fatal("custom model missing from response")
			}
			// Responses must not mutate the cached template, including nested defaults.
			sol["display_name"] = "mutated"
			sol["truncation_policy"].(map[string]any)["mode"] = "mutated"
			again := buildCodexClientModelsWithTemplates([]map[string]any{{"id": "gpt-6.1-sol"}}, templates, defaults)[0]
			if again["display_name"] != "GPT-6.1-Sol" || again["truncation_policy"].(map[string]any)["mode"] == "mutated" {
				t.Fatalf("response mutated cached defaults: %#v", again)
			}
		})
	}
}

func TestCodexClientModelTemplatesRefreshOnRevision(t *testing.T) {
	var cache codexClientModelTemplateCache
	first := []byte(`{"models":[{"slug":"gpt-6.1-sol","display_name":"Before","visibility":"hide"},{"slug":"removed-model"}]}`)
	second := []byte(`{"models":[{"slug":"gpt-6.1-sol","display_name":"After","visibility":"list"},{"slug":"new-model"}]}`)
	templates, defaults, err := cache.load(first, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldTemplates := templates
	models := []map[string]any{{"id": "gpt-6.1-sol"}}
	before, err := json.Marshal(buildCodexClientModelsWithTemplates(models, templates, defaults))
	if err != nil {
		t.Fatal(err)
	}
	// An unchanged revision reuses the published immutable maps.
	cached, _, err := cache.load(first, 1)
	if err != nil || cached["gpt-6.1-sol"]["display_name"] != "Before" {
		t.Fatalf("cached snapshot = %#v, err=%v", cached, err)
	}
	templates, defaults, err = cache.load(second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if templates["removed-model"] != nil || templates["new-model"] == nil {
		t.Fatalf("refresh must replace, not merge, templates: %#v", templates)
	}
	if oldTemplates["gpt-6.1-sol"]["display_name"] != "Before" {
		t.Fatal("refresh mutated an already published snapshot")
	}
	afterModels := buildCodexClientModelsWithTemplates(models, templates, defaults)
	if afterModels[0]["display_name"] != "After" || afterModels[0]["visibility"] != "list" {
		t.Fatalf("refreshed metadata not reflected: %#v", afterModels)
	}
	after, err := json.Marshal(afterModels)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) == sha256.Sum256(after) {
		t.Fatal("catalog refresh must change the response content ETag")
	}
}

func TestCodexClientModelTemplatesRecoverOnRevision(t *testing.T) {
	var cache codexClientModelTemplateCache
	if _, _, err := cache.load([]byte(`{"models":`), 1); err == nil {
		t.Fatal("invalid catalog should fail")
	}
	templates, defaults, err := cache.load([]byte(`{"models":[{"slug":"gpt-6.1-sol"}]}`), 2)
	if err != nil || defaults == nil || templates["gpt-6.1-sol"] == nil {
		t.Fatalf("new revision did not recover: templates=%v, defaults=%v, err=%v", templates, defaults, err)
	}
}

func TestCodexClientModelTemplatesConcurrentSnapshots(t *testing.T) {
	var cache codexClientModelTemplateCache
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			slug := fmt.Sprintf("model-%d", i)
			data := []byte(fmt.Sprintf(`{"models":[{"slug":%q}]}`, slug))
			for range 10 {
				templates, defaults, err := cache.load(data, uint64(i+1))
				if err != nil || defaults == nil || templates[slug] == nil {
					t.Errorf("inconsistent snapshot for %s: templates=%v, err=%v", slug, templates, err)
					return
				}
				buildCodexClientModelsWithTemplates([]map[string]any{{"id": slug}}, templates, defaults)
			}
		})
	}
	wg.Wait()
}

func TestParseCodexClientModelTemplatesDefaultWithoutLegacySlug(t *testing.T) {
	templates, defaults, err := parseCodexClientModelTemplates([]byte(`{"models":[{"id":"replacement-default","shell_type":"shell_command"}]}`))
	if err != nil || templates["replacement-default"] == nil || defaults["slug"] != "replacement-default" {
		t.Fatalf("replacement default = %#v, templates=%v, err=%v", defaults, templates, err)
	}
	if _, _, err := parseCodexClientModelTemplates([]byte(`{"models":[]}`)); err == nil {
		t.Fatal("empty catalog should fail")
	}
}
