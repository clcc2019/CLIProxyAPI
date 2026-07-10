package config

import "testing"

func TestEffectiveModelPricesIncludesClaudeDefaults(t *testing.T) {
	prices := EffectiveModelPrices(nil)

	price, ok := LookupModelPrice(prices, "claude-opus-4-7-agentic")
	if !ok {
		t.Fatal("expected opus 4.7 agentic alias to resolve")
	}
	if price.Prompt != 5 || price.Completion != 25 || price.Cache != 0.5 {
		t.Fatalf("unexpected opus price: %+v", price)
	}

	price, ok = LookupModelPrice(prices, "claude-sonnet-4-6")
	if !ok {
		t.Fatal("expected sonnet 4.6 hyphen alias to resolve")
	}
	if price.Prompt != 3 || price.Completion != 15 || price.Cache != 0.3 {
		t.Fatalf("unexpected sonnet price: %+v", price)
	}
}

func TestEffectiveModelPricesIncludesGPT56Defaults(t *testing.T) {
	prices := EffectiveModelPrices(nil)

	tests := []struct {
		name       string
		prompt     float64
		completion float64
		cache      float64
	}{
		{name: "gpt-5.6", prompt: 5, completion: 30, cache: 0.5},
		{name: "gpt-5.6-sol", prompt: 5, completion: 30, cache: 0.5},
		{name: "gpt-5.6-terra", prompt: 2.5, completion: 15, cache: 0.25},
		{name: "gpt-5.6-luna", prompt: 1, completion: 6, cache: 0.1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			price, ok := LookupModelPrice(prices, tt.name)
			if !ok {
				t.Fatalf("expected %s price to resolve", tt.name)
			}
			if price.Prompt != tt.prompt || price.Completion != tt.completion || price.Cache != tt.cache {
				t.Fatalf("unexpected %s price: %+v", tt.name, price)
			}
		})
	}

	price, ok := LookupModelPrice(prices, "gpt-5.6-sol(max)")
	if !ok {
		t.Fatal("expected gpt-5.6-sol(max) alias to resolve")
	}
	if price.Prompt != 5 || price.Completion != 30 || price.Cache != 0.5 {
		t.Fatalf("unexpected gpt-5.6-sol(max) price: %+v", price)
	}
}

func TestEffectiveModelPricesKeepsUserOverride(t *testing.T) {
	prices := EffectiveModelPrices(ModelPrices{
		"claude-sonnet-4.6": {Prompt: 9, Completion: 10, Cache: 1},
	})

	price, ok := LookupModelPrice(prices, "claude-sonnet-4-6")
	if !ok {
		t.Fatal("expected override to resolve")
	}
	if price.Prompt != 9 || price.Completion != 10 || price.Cache != 1 {
		t.Fatalf("override was not used: %+v", price)
	}
}

func TestLookupModelPriceUsesDefaultFallback(t *testing.T) {
	prices := EffectiveModelPrices(ModelPrices{
		"default": {Prompt: 1.25, Completion: 2.5, Cache: 0.25},
	})

	price, ok := LookupModelPrice(prices, "unpriced-custom-model")
	if !ok {
		t.Fatal("expected default price fallback to resolve")
	}
	if price.Prompt != 1.25 || price.Completion != 2.5 || price.Cache != 0.25 {
		t.Fatalf("unexpected fallback price: %+v", price)
	}
}
