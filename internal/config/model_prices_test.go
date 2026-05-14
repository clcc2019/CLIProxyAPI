package config

import "testing"

func TestEffectiveModelPricesIncludesKiroClaudeDefaults(t *testing.T) {
	prices := EffectiveModelPrices(nil)

	price, ok := LookupModelPrice(prices, "kiro-claude-opus-4-7-agentic")
	if !ok {
		t.Fatal("expected kiro opus 4.7 alias to resolve")
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
