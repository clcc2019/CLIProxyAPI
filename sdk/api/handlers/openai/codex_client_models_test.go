package openai

import "testing"

func TestNormalizeCodexClientReasoningLevel(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "mixed case", raw: " Medium ", want: "medium"},
		{name: "xhigh", raw: "XHIGH", want: "xhigh"},
		{name: "max", raw: "MAX", want: "max"},
		{name: "ultra", raw: "ULTRA", want: "ultra"},
		{name: "none", raw: "none", want: "none"},
		{name: "unknown", raw: "extreme", want: ""},
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
