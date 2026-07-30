package auth

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/credentialweight"
)

// AttributeWeight is the credential attribute used by weighted-round-robin.
const AttributeWeight = "weight"

func authWeight(auth *Auth) int64 {
	if auth == nil {
		return credentialweight.Default
	}
	if rawWeight, ok := auth.Attributes[AttributeWeight]; ok && strings.TrimSpace(rawWeight) != "" {
		weight, err := credentialweight.ParseString(rawWeight)
		if err != nil {
			return 0
		}
		return weight
	}
	if rawWeight, ok := auth.Metadata[AttributeWeight]; ok {
		weight, err := credentialweight.ParseValue(rawWeight)
		if err != nil {
			return 0
		}
		return weight
	}
	return credentialweight.Default
}

// ValidateAuthWeight validates every explicit credential weight source.
func ValidateAuthWeight(auth *Auth) error {
	if auth == nil {
		return nil
	}
	if rawWeight, ok := auth.Attributes[AttributeWeight]; ok {
		if _, err := credentialweight.ParseString(rawWeight); err != nil {
			return fmt.Errorf("invalid attributes weight: %w", err)
		}
	}
	if rawWeight, ok := auth.Metadata[AttributeWeight]; ok {
		if _, err := credentialweight.ParseValue(rawWeight); err != nil {
			return fmt.Errorf("invalid metadata weight: %w", err)
		}
	}
	return nil
}

// ApplyAuthWeightMetadata validates the auth and applies a source metadata weight.
func ApplyAuthWeightMetadata(auth *Auth, metadata map[string]any) error {
	if err := ValidateAuthWeight(auth); err != nil {
		return err
	}
	if auth == nil || metadata == nil {
		return nil
	}
	rawWeight, ok := metadata[AttributeWeight]
	if !ok {
		return nil
	}
	weight, err := credentialweight.ParseValue(rawWeight)
	if err != nil {
		return fmt.Errorf("invalid metadata weight: %w", err)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[AttributeWeight] = strconv.FormatInt(weight, 10)
	return nil
}
