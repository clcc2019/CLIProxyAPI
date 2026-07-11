package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWriteCodexClientModelsResponseProvidesStableETagAndSupportsRevalidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	models := []map[string]any{{"id": "gpt-5.4"}}

	firstRecorder := httptest.NewRecorder()
	firstContext, _ := gin.CreateTestContext(firstRecorder)
	firstContext.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=1.0.0", nil)
	WriteCodexClientModelsResponse(firstContext, models)

	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body=%s", firstRecorder.Code, firstRecorder.Body.String())
	}
	etag := firstRecorder.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first response did not include ETag")
	}
	if got := firstRecorder.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("Cache-Control = %q, want private, no-cache", got)
	}
	secondRecorder := httptest.NewRecorder()
	secondContext, _ := gin.CreateTestContext(secondRecorder)
	secondContext.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=1.0.0", nil)
	secondContext.Request.Header.Set("If-None-Match", "W/"+etag)
	WriteCodexClientModelsResponse(secondContext, models)

	if secondRecorder.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d, want 304; body=%s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if secondRecorder.Body.Len() != 0 {
		t.Fatalf("304 response body = %q, want empty", secondRecorder.Body.String())
	}
	if got := secondRecorder.Header().Get("ETag"); got != etag {
		t.Fatalf("revalidation ETag = %q, want %q", got, etag)
	}
}

func TestCodexClientModelsResponsePreservesOfficialServiceTiers(t *testing.T) {
	response := CodexClientModelsResponse([]map[string]any{{"id": "gpt-5.4"}})
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v, want one model", response["models"])
	}
	serviceTiers, ok := models[0]["service_tiers"].([]any)
	if !ok || len(serviceTiers) != 1 {
		t.Fatalf("service_tiers = %#v, want one official tier", models[0]["service_tiers"])
	}
	tier, ok := serviceTiers[0].(map[string]any)
	if !ok {
		t.Fatalf("service_tiers[0] = %#v, want object", serviceTiers[0])
	}
	if got := stringModelValue(tier, "id"); got != "priority" {
		t.Fatalf("service tier id = %q, want priority", got)
	}
}

func TestCodexClientModelsResponseDoesNotLeakDefaultServiceTiersToCustomModels(t *testing.T) {
	response := CodexClientModelsResponse([]map[string]any{{"id": "custom-model"}})
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v, want one model", response["models"])
	}
	if _, ok := models[0]["service_tiers"]; ok {
		t.Fatalf("custom model should not inherit default template service_tiers: %#v", models[0])
	}
	if _, ok := models[0]["default_service_tier"]; ok {
		t.Fatalf("custom model should not inherit default template default_service_tier: %#v", models[0])
	}
}

func TestCodexClientModelsResponseCopiesCustomServiceTiers(t *testing.T) {
	response := CodexClientModelsResponse([]map[string]any{{
		"id": "custom-model",
		"service_tiers": []any{map[string]any{
			"id":          "flex",
			"name":        "Flex",
			"description": "Lower priority processing",
		}},
		"default_service_tier": "flex",
	}})
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v, want one model", response["models"])
	}
	serviceTiers, ok := models[0]["service_tiers"].([]any)
	if !ok || len(serviceTiers) != 1 {
		t.Fatalf("service_tiers = %#v, want custom tier", models[0]["service_tiers"])
	}
	tier, ok := serviceTiers[0].(map[string]any)
	if !ok {
		t.Fatalf("service_tiers[0] = %#v, want object", serviceTiers[0])
	}
	if got := stringModelValue(tier, "id"); got != "flex" {
		t.Fatalf("service tier id = %q, want flex", got)
	}
	if got := stringModelValue(models[0], "default_service_tier"); got != "flex" {
		t.Fatalf("default_service_tier = %q, want flex", got)
	}
}

func TestNormalizeCodexClientReasoningLevel(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "mixed case", raw: " Medium ", want: "medium"},
		{name: "minimal", raw: "MINIMAL", want: "minimal"},
		{name: "xhigh", raw: "XHIGH", want: "xhigh"},
		{name: "max", raw: "MAX", want: "max"},
		{name: "ultra", raw: "ULTRA", want: "ultra"},
		{name: "none", raw: "none", want: "none"},
		{name: "custom", raw: "extreme", want: "extreme"},
		{name: "empty", raw: " ", want: ""},
	}

	for i := range tests {
		if got := normalizeCodexClientReasoningLevel(tests[i].raw); got != tests[i].want {
			t.Fatalf("%s: got %q, want %q", tests[i].name, got, tests[i].want)
		}
	}
}

func TestCodexClientModelsResponseKeepsGPT55XHighForClient(t *testing.T) {
	response := CodexClientModelsResponse([]map[string]any{
		{"id": "gpt-5.5"},
	})

	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) == 0 {
		t.Fatalf("models = %#v, want non-empty []map[string]any", response["models"])
	}

	var gpt55 map[string]any
	for _, model := range models {
		if stringModelValue(model, "slug") == "gpt-5.5" {
			gpt55 = model
			break
		}
	}
	if gpt55 == nil {
		t.Fatalf("gpt-5.5 not found in response: %#v", models)
	}

	levels, ok := gpt55["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("supported_reasoning_levels = %#v, want []any", gpt55["supported_reasoning_levels"])
	}
	for _, rawLevel := range levels {
		level, ok := rawLevel.(map[string]any)
		if ok && stringModelValue(level, "effort") == "xhigh" {
			return
		}
	}
	t.Fatalf("gpt-5.5 response should keep xhigh for clients: %#v", levels)
}

func TestCodexClientModelsResponseKeepsGPT56SolMaxAndUltraForClient(t *testing.T) {
	response := CodexClientModelsResponse([]map[string]any{
		{"id": "gpt-5.6-sol"},
	})

	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) == 0 {
		t.Fatalf("models = %#v, want non-empty []map[string]any", response["models"])
	}

	var gpt56 map[string]any
	for _, model := range models {
		if stringModelValue(model, "slug") == "gpt-5.6-sol" {
			gpt56 = model
			break
		}
	}
	if gpt56 == nil {
		t.Fatalf("gpt-5.6-sol not found in response: %#v", models)
	}

	levels, ok := gpt56["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("supported_reasoning_levels = %#v, want []any", gpt56["supported_reasoning_levels"])
	}
	foundXHigh := false
	foundMax := false
	foundUltra := false
	for _, rawLevel := range levels {
		level, ok := rawLevel.(map[string]any)
		if !ok {
			continue
		}
		switch stringModelValue(level, "effort") {
		case "xhigh":
			foundXHigh = true
		case "max":
			foundMax = true
		case "ultra":
			foundUltra = true
		}
	}
	if !foundXHigh {
		t.Fatalf("gpt-5.6-sol response should keep xhigh for clients: %#v", levels)
	}
	if !foundMax {
		t.Fatalf("gpt-5.6-sol response should keep max for clients: %#v", levels)
	}
	if !foundUltra {
		t.Fatalf("gpt-5.6-sol response should keep ultra for clients: %#v", levels)
	}
}

func BenchmarkNormalizeCodexClientReasoningLevel(b *testing.B) {
	for b.Loop() {
		if got := normalizeCodexClientReasoningLevel(" Medium "); got != "medium" {
			b.Fatalf("normalizeCodexClientReasoningLevel() = %q", got)
		}
	}
}
