package config

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/credentialweight"
)

// MaxCredentialWeight is the largest positive credential routing weight.
const MaxCredentialWeight = int(credentialweight.Max)

// ValidateCredentialWeight validates one optional config credential weight.
func ValidateCredentialWeight(weight *int) error {
	if weight == nil {
		return nil
	}
	_, err := credentialweight.Normalize(int64(*weight))
	return err
}

// ValidateCredentialWeights validates weights for every API-key family supported
// by this build.
func (cfg *Config) ValidateCredentialWeights() error {
	if cfg == nil {
		return nil
	}
	for index := range cfg.ClaudeKey {
		if err := ValidateCredentialWeight(cfg.ClaudeKey[index].Weight); err != nil {
			return fmt.Errorf("claude-api-key[%d].weight: %w", index, err)
		}
	}
	for index := range cfg.CodexKey {
		if err := ValidateCredentialWeight(cfg.CodexKey[index].Weight); err != nil {
			return fmt.Errorf("codex-api-key[%d].weight: %w", index, err)
		}
	}
	for providerIndex := range cfg.OpenAICompatibility {
		for keyIndex := range cfg.OpenAICompatibility[providerIndex].APIKeyEntries {
			weight := cfg.OpenAICompatibility[providerIndex].APIKeyEntries[keyIndex].Weight
			if err := ValidateCredentialWeight(weight); err != nil {
				return fmt.Errorf("openai-compatibility[%d].api-key-entries[%d].weight: %w", providerIndex, keyIndex, err)
			}
		}
	}
	return nil
}
