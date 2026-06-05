package config

import (
	"math"
	"regexp"
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

var modelPriceDateSuffixRE = regexp.MustCompile(`-\d{8}$`)

func DefaultModelPrices() ModelPrices {
	return CloneModelPrices(defaultModelPrices)
}

func EffectiveModelPrices(prices ModelPrices) ModelPrices {
	defaults := DefaultModelPrices()
	out := CloneModelPrices(defaults)
	for model, price := range NormalizeModelPrices(prices) {
		out[model] = price
		for _, key := range ModelPriceLookupKeys(model) {
			if _, ok := defaults[key]; ok {
				out[key] = price
				break
			}
		}
	}
	return NormalizeModelPrices(out)
}

func LookupModelPrice(prices ModelPrices, names ...string) (ModelPrice, bool) {
	if len(prices) == 0 {
		return ModelPrice{}, false
	}
	for _, name := range names {
		for _, key := range ModelPriceLookupKeys(name) {
			if price, ok := prices[key]; ok {
				return price, true
			}
		}
	}
	for _, key := range []string{"default", "*"} {
		if price, ok := prices[key]; ok {
			return price, true
		}
	}
	return ModelPrice{}, false
}

func ModelPriceLookupKeys(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var keys []string
	var queue []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		keys = append(keys, value)
		queue = append(queue, value)
	}

	add(name)
	add(strings.ToLower(name))
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, candidate := range modelPriceAliasCandidates(current) {
			add(candidate)
			add(strings.ToLower(candidate))
		}
	}
	return keys
}

func modelPriceAliasCandidates(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" && value != name {
			out = append(out, value)
		}
	}
	if cut, _, ok := strings.Cut(name, "("); ok {
		add(strings.TrimSpace(cut))
	}
	if strings.HasPrefix(name, "models/") {
		add(strings.TrimPrefix(name, "models/"))
	}
	if slash := strings.LastIndex(name, "/"); slash >= 0 && slash+1 < len(name) {
		add(name[slash+1:])
	}
	for _, prefix := range []string{"amazonq-"} {
		if strings.HasPrefix(name, prefix) {
			add(strings.TrimPrefix(name, prefix))
		}
	}
	for _, suffix := range []string{"-agentic", "-chat", "-thinking", "-1m"} {
		if strings.HasSuffix(name, suffix) {
			add(strings.TrimSuffix(name, suffix))
		}
	}
	if stripped := modelPriceDateSuffixRE.ReplaceAllString(name, ""); stripped != name {
		add(stripped)
	}
	if converted := hyphenNumericVersionToDot(name); converted != name {
		add(converted)
	}
	if converted := dotNumericVersionToHyphen(name); converted != name {
		add(converted)
	}
	return out
}

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

func hyphenNumericVersionToDot(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return name
	}
	for i := 0; i < len(parts)-1; i++ {
		if isDigits(parts[i]) && isDigits(parts[i+1]) {
			converted := make([]string, 0, len(parts)-1)
			converted = append(converted, parts[:i]...)
			converted = append(converted, parts[i]+"."+parts[i+1])
			converted = append(converted, parts[i+2:]...)
			return strings.Join(converted, "-")
		}
	}
	return name
}

func dotNumericVersionToHyphen(name string) string {
	parts := strings.Split(name, "-")
	for i, part := range parts {
		left, right, ok := strings.Cut(part, ".")
		if ok && isDigits(left) && isDigits(right) {
			converted := append([]string(nil), parts...)
			converted[i] = left + "-" + right
			return strings.Join(converted, "-")
		}
	}
	return name
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var defaultModelPrices = buildDefaultModelPrices()

func buildDefaultModelPrices() ModelPrices {
	prices := ModelPrices{
		"gpt-5.4": {Prompt: 2.5, Completion: 15, Cache: 0.25},
		"gpt-5.5": {Prompt: 5, Completion: 30, Cache: 0.25},
	}

	// USD per 1M tokens. Cache is the read-hit price; cache write tokens stay in
	// prompt input for cost calculation because the shared price schema has one
	// cache bucket.
	opus45 := ModelPrice{Prompt: 5, Completion: 25, Cache: 0.5}
	sonnet4 := ModelPrice{Prompt: 3, Completion: 15, Cache: 0.3}
	haiku45 := ModelPrice{Prompt: 1, Completion: 5, Cache: 0.1}
	opus4Legacy := ModelPrice{Prompt: 15, Completion: 75, Cache: 1.5}

	for _, model := range []string{
		"claude-opus-4.7",
		"claude-opus-4.6",
		"claude-opus-4.5",
	} {
		addDefaultModelPrice(prices, model, opus45)
	}
	for _, model := range []string{
		"claude-sonnet-4.6",
		"claude-sonnet-4.5",
		"claude-sonnet-4",
	} {
		addDefaultModelPrice(prices, model, sonnet4)
	}
	addDefaultModelPrice(prices, "claude-haiku-4.5", haiku45)
	for _, model := range []string{
		"claude-opus-4.1",
		"claude-opus-4",
	} {
		addDefaultModelPrice(prices, model, opus4Legacy)
	}

	return prices
}

func addDefaultModelPrice(prices ModelPrices, model string, price ModelPrice) {
	model = strings.TrimSpace(model)
	if model != "" {
		prices[model] = price
	}
}
