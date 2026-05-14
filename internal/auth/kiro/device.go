package kiro

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const kiroMachineIDSeedPrefix = "cliproxyapi:kiro:machine-id:v1:"

// GenerateKiroMachineID returns a Kiro-compatible 64-hex device identifier.
func GenerateKiroMachineID() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return StableKiroMachineID("fallback")
}

// StableKiroMachineID derives a non-secret, stable fallback identifier from
// account-local identity. It deliberately avoids access/refresh tokens.
func StableKiroMachineID(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		seed = "unknown"
	}
	sum := sha256.Sum256([]byte(kiroMachineIDSeedPrefix + seed))
	return hex.EncodeToString(sum[:])
}

func NormalizeKiroMachineID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	if isKiroHexMachineID(value) {
		return value
	}
	if isKiroUUIDMachineID(value) {
		return value
	}
	return ""
}

func MachineIDFromTokenData(tokenData *TokenData) string {
	if tokenData == nil {
		return ""
	}
	if machineID := NormalizeKiroMachineID(tokenData.MachineID); machineID != "" {
		return machineID
	}
	return StableKiroMachineID(firstNonEmpty(tokenData.Email, tokenData.ProfileArn, tokenData.ClientID, tokenData.Provider))
}

func MachineIDFromMaps(metadata map[string]any, attrs map[string]string, fallbackSeed string) string {
	for _, key := range []string{"machine_id", "machineId", "device_id", "deviceId"} {
		if metadata != nil {
			if v, ok := metadata[key].(string); ok {
				if machineID := NormalizeKiroMachineID(v); machineID != "" {
					return machineID
				}
			}
		}
		if attrs != nil {
			if machineID := NormalizeKiroMachineID(attrs[key]); machineID != "" {
				return machineID
			}
		}
	}
	return StableKiroMachineID(fallbackSeed)
}

func isKiroHexMachineID(value string) bool {
	if len(value) != 64 && len(value) != 32 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func isKiroUUIDMachineID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return false
			}
		}
	}
	return true
}
