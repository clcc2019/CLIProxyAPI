package common

import "github.com/tidwall/gjson"

func CachedInputTokens(usage gjson.Result) gjson.Result {
	if cached := usage.Get("prompt_tokens_details.cached_tokens"); cached.Exists() {
		return cached
	}
	if cached := usage.Get("input_tokens_details.cached_tokens"); cached.Exists() {
		return cached
	}
	if cached := usage.Get("cached_input_tokens"); cached.Exists() {
		return cached
	}
	return usage.Get("cache_read_input_tokens")
}

func ReasoningOutputTokens(usage gjson.Result) gjson.Result {
	if reasoning := usage.Get("completion_tokens_details.reasoning_tokens"); reasoning.Exists() {
		return reasoning
	}
	if reasoning := usage.Get("output_tokens_details.reasoning_tokens"); reasoning.Exists() {
		return reasoning
	}
	if reasoning := usage.Get("reasoning_output_tokens"); reasoning.Exists() {
		return reasoning
	}
	return usage.Get("reasoning_tokens")
}
