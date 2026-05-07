package config

import (
	"math"
	"strings"
)

// ModelPrice stores server-side pricing used to calculate client API key spend.
// Values are USD per 1M tokens.
type ModelPrice struct {
	Prompt     float64 `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Completion float64 `yaml:"completion,omitempty" json:"completion,omitempty"`
	Cache      float64 `yaml:"cache,omitempty" json:"cache,omitempty"`
}

type ModelPrices map[string]ModelPrice

func NormalizeModelPrices(prices ModelPrices) ModelPrices {
	if len(prices) == 0 {
		return nil
	}
	out := make(ModelPrices, len(prices))
	for model, rawPrice := range prices {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		price := ModelPrice{
			Prompt:     normalizeModelPriceValue(rawPrice.Prompt),
			Completion: normalizeModelPriceValue(rawPrice.Completion),
			Cache:      normalizeModelPriceValue(rawPrice.Cache),
		}
		if price.Prompt == 0 && price.Completion == 0 && price.Cache == 0 {
			continue
		}
		out[model] = price
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func CloneModelPrices(prices ModelPrices) ModelPrices {
	if len(prices) == 0 {
		return nil
	}
	out := make(ModelPrices, len(prices))
	for model, price := range prices {
		out[model] = price
	}
	return out
}

func normalizeModelPriceValue(value float64) float64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
