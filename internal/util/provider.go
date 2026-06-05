// Package util provides utility functions used across the CLIProxyAPI application.
// These functions handle common tasks such as determining AI service providers
// from model names and managing HTTP proxies.
package util

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

// GetProviderName determines all AI service providers capable of serving a registered model.
// It queries the global model registry to retrieve the providers backing the supplied model name.
//
// Supported providers include (but are not limited to):
//   - "codex" for OpenAI GPT-compatible providers
//   - "claude" for Anthropic models
//   - "openai-compatibility" for external OpenAI-compatible providers
//
// Parameters:
//   - modelName: The name of the model to identify providers for.
//   - cfg: The application configuration containing OpenAI compatibility settings.
//
// Returns:
//   - []string: All provider identifiers capable of serving the model, ordered by preference.
func GetProviderName(modelName string) []string {
	if modelName == "" {
		return nil
	}

	registryProviders := registry.GetGlobalRegistry().GetModelProviders(modelName)
	switch len(registryProviders) {
	case 0:
		return nil
	case 1:
		if registryProviders[0] == "" {
			return nil
		}
		return registryProviders
	}

	providers := make([]string, 0, len(registryProviders))
	seen := make(map[string]struct{}, len(registryProviders))
	for _, provider := range registryProviders {
		if provider == "" {
			continue
		}
		if _, exists := seen[provider]; exists {
			continue
		}
		seen[provider] = struct{}{}
		providers = append(providers, provider)
	}
	return providers
}

// ResolveAutoModel resolves the "auto" model name to an actual available model.
// It uses an empty handler type to get any available model from the registry.
//
// Parameters:
//   - modelName: The model name to check (should be "auto")
//
// Returns:
//   - string: The resolved model name, or the original if not "auto" or resolution fails
func ResolveAutoModel(modelName string) string {
	if modelName != "auto" {
		return modelName
	}

	// Use empty string as handler type to get any available model
	firstModel, err := registry.GetGlobalRegistry().GetFirstAvailableModel("")
	if err != nil {
		log.Warnf("Failed to resolve 'auto' model: %v, falling back to original model name", err)
		return modelName
	}

	log.Infof("Resolved 'auto' model to: %s", firstModel)
	return firstModel
}

// IsOpenAICompatibilityAlias checks if the given model name is an alias
// configured for OpenAI compatibility routing.
//
// Parameters:
//   - modelName: The model name to check
//   - cfg: The application configuration containing OpenAI compatibility settings
//
// Returns:
//   - bool: True if the model name is an OpenAI compatibility alias, false otherwise
func IsOpenAICompatibilityAlias(modelName string, cfg *config.Config) bool {
	if cfg == nil {
		return false
	}

	for _, compat := range cfg.OpenAICompatibility {
		if compat.Disabled {
			continue
		}
		for _, model := range compat.Models {
			if model.Alias == modelName {
				return true
			}
		}
	}
	return false
}

// GetOpenAICompatibilityConfig returns the OpenAI compatibility configuration
// and model details for the given alias.
//
// Parameters:
//   - alias: The model alias to find configuration for
//   - cfg: The application configuration containing OpenAI compatibility settings
//
// Returns:
//   - *config.OpenAICompatibility: The matching compatibility configuration, or nil if not found
//   - *config.OpenAICompatibilityModel: The matching model configuration, or nil if not found
func GetOpenAICompatibilityConfig(alias string, cfg *config.Config) (*config.OpenAICompatibility, *config.OpenAICompatibilityModel) {
	if cfg == nil {
		return nil, nil
	}

	for _, compat := range cfg.OpenAICompatibility {
		if compat.Disabled {
			continue
		}
		for _, model := range compat.Models {
			if model.Alias == alias {
				return &compat, &model
			}
		}
	}
	return nil, nil
}

// InArray checks if a string exists in a slice of strings.
// It iterates through the slice and returns true if the target string is found,
// otherwise it returns false.
//
// Parameters:
//   - hystack: The slice of strings to search in
//   - needle: The string to search for
//
// Returns:
//   - bool: True if the string is found, false otherwise
func InArray(hystack []string, needle string) bool {
	for _, item := range hystack {
		if needle == item {
			return true
		}
	}
	return false
}

// HideAPIKey obscures an API key for logging purposes, showing only the first and last few characters.
//
// Parameters:
//   - apiKey: The API key to hide.
//
// Returns:
//   - string: The obscured API key.
func HideAPIKey(apiKey string) string {
	if len(apiKey) > 8 {
		return apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
	} else if len(apiKey) > 4 {
		return apiKey[:2] + "..." + apiKey[len(apiKey)-2:]
	} else if len(apiKey) > 2 {
		return apiKey[:1] + "..." + apiKey[len(apiKey)-1:]
	}
	return apiKey
}

// maskAuthorizationHeader masks the Authorization header value while preserving the auth type prefix.
// Common formats: "Bearer <token>", "Basic <credentials>", "ApiKey <key>", etc.
// It preserves the prefix (e.g., "Bearer ") and only masks the token/credential part.
//
// Parameters:
//   - value: The Authorization header value
//
// Returns:
//   - string: The masked Authorization value with prefix preserved
func MaskAuthorizationHeader(value string) string {
	parts := strings.SplitN(strings.TrimSpace(value), " ", 2)
	if len(parts) < 2 {
		return HideAPIKey(value)
	}
	return parts[0] + " " + HideAPIKey(parts[1])
}

// MaskSensitiveHeaderValue masks sensitive header values while preserving expected formats.
//
// Behavior by header key (case-insensitive):
//   - "Authorization": Preserve the auth type prefix (e.g., "Bearer ") and mask only the credential part.
//   - Headers containing "api-key": Mask the entire value using HideAPIKey.
//   - Others: Return the original value unchanged.
//
// Parameters:
//   - key:   The HTTP header name to inspect (case-insensitive matching).
//   - value: The header value to mask when sensitive.
//
// Returns:
//   - string: The masked value according to the header type; unchanged if not sensitive.
func MaskSensitiveHeaderValue(key, value string) string {
	lowerKey := strings.ToLower(strings.TrimSpace(key))
	switch {
	case strings.Contains(lowerKey, "authorization"):
		return MaskAuthorizationHeader(value)
	case strings.Contains(lowerKey, "api-key"),
		strings.Contains(lowerKey, "apikey"),
		strings.Contains(lowerKey, "token"),
		strings.Contains(lowerKey, "secret"):
		return HideAPIKey(value)
	default:
		return value
	}
}

// RedactSensitiveJSONBytes masks credential-like JSON fields before writing
// request payloads to diagnostic logs. Non-JSON payloads are returned unchanged.
func RedactSensitiveJSONBytes(data []byte) []byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return data
	}
	out, ok := redactSensitiveJSONBytes(trimmed)
	if !ok {
		return data
	}
	return out
}

func redactSensitiveJSONBytes(trimmed []byte) ([]byte, bool) {
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, false
	}
	redactSensitiveJSONValue(value)
	out, err := json.Marshal(value)
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// RedactSensitiveLogBytes masks credential-like JSON fields in plain JSON and
// in SSE data lines before diagnostic payloads are written to request logs. It
// also handles plain-text error strings that include header-like or query-like
// credentials.
func RedactSensitiveLogBytes(data []byte) []byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return data
	}
	if out, ok := redactSensitiveJSONBytes(trimmed); ok {
		return out
	}
	return redactSensitivePlainTextBytes(redactSensitiveSSEDataBytes(data))
}

var (
	plainAuthorizationCredentialPattern = regexp.MustCompile(`(?i)\b(authorization\s*[:=]\s*)(?:(bearer|basic|apikey)\s+)?([^\s,;]+)`)
	plainBearerCredentialPattern        = regexp.MustCompile(`(?i)\b(bearer|basic)\s+([A-Za-z0-9._~+/=-]{8,})`)
	plainKeyValueCredentialPattern      = regexp.MustCompile(`(?i)\b(api[-_ ]?key|access[-_ ]?token|refresh[-_ ]?token|id[-_ ]?token|session[-_ ]?token|bearer[-_ ]?token|client[-_ ]?secret|password|credential|credentials)\b(\s*[:=]\s*)([^\s&;,]+)`)
)

func redactSensitivePlainTextBytes(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	if !mayContainSensitiveTextBytes(data) {
		return data
	}
	redacted := plainAuthorizationCredentialPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		parts := plainAuthorizationCredentialPattern.FindSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		out := make([]byte, 0, len(parts[1])+len(parts[2])+len(" [REDACTED]")+len("[REDACTED]"))
		out = append(out, parts[1]...)
		if len(parts[2]) > 0 {
			out = append(out, parts[2]...)
			out = append(out, ' ')
		}
		out = append(out, "[REDACTED]"...)
		return out
	})
	redacted = plainBearerCredentialPattern.ReplaceAll(redacted, []byte(`${1} [REDACTED]`))
	redacted = plainKeyValueCredentialPattern.ReplaceAll(redacted, []byte(`${1}${2}[REDACTED]`))
	return redacted
}

func redactSensitiveJSONValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if shouldRedactJSONKey(key) {
				typed[key] = "[REDACTED]"
				continue
			}
			if str, ok := child.(string); ok {
				typed[key] = redactSensitiveJSONString(str)
				continue
			}
			redactSensitiveJSONValue(child)
		}
	case []any:
		for i, child := range typed {
			if str, ok := child.(string); ok {
				typed[i] = redactSensitiveJSONString(str)
				continue
			}
			redactSensitiveJSONValue(child)
		}
	}
}

func redactSensitiveJSONString(value string) string {
	if value == "" {
		return value
	}
	if !mayContainSensitiveTextString(value) {
		return value
	}
	redacted := redactSensitivePlainTextBytes([]byte(value))
	if len(redacted) == 0 {
		return value
	}
	return string(redacted)
}

var sensitiveTextMarkers = []string{
	"authorization",
	"bearer",
	"basic",
	"api",
	"token",
	"secret",
	"password",
	"credential",
}

func mayContainSensitiveTextString(value string) bool {
	value = strings.ToLower(value)
	for _, marker := range sensitiveTextMarkers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func mayContainSensitiveTextBytes(data []byte) bool {
	for _, marker := range sensitiveTextMarkers {
		if containsASCIIFold(data, marker) {
			return true
		}
	}
	return false
}

func containsASCIIFold(data []byte, marker string) bool {
	if len(marker) == 0 {
		return true
	}
	if len(data) < len(marker) {
		return false
	}
	for i := 0; i <= len(data)-len(marker); i++ {
		matched := true
		for j := 0; j < len(marker); j++ {
			if lowerASCII(data[i+j]) != marker[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func shouldRedactJSONKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	normalized := strings.ReplaceAll(key, "-", "_")
	normalized = strings.ReplaceAll(normalized, " ", "_")
	compact := strings.ReplaceAll(normalized, "_", "")
	switch normalized {
	case "authorization", "auth", "auth_token", "access_token", "refresh_token", "id_token",
		"token", "bearer_token", "session_token", "api_key", "apikey", "x_api_key",
		"secret", "client_secret", "password", "passcode", "credential", "credentials":
		return true
	}
	switch compact {
	case "authtoken", "accesstoken", "refreshtoken", "idtoken", "bearertoken", "sessiontoken",
		"apikey", "xapikey", "clientsecret":
		return true
	}
	return strings.Contains(normalized, "authorization") ||
		strings.Contains(normalized, "api_key") ||
		strings.Contains(normalized, "apikey") ||
		strings.HasSuffix(normalized, "_token") ||
		strings.HasSuffix(normalized, "_secret") ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "credential")
}

func redactSensitiveSSEDataBytes(data []byte) []byte {
	if len(data) == 0 || !bytes.Contains(data, []byte("data:")) {
		return data
	}
	segments := bytes.SplitAfter(data, []byte("\n"))
	changed := false
	for i, segment := range segments {
		line, suffix := trimLineEnding(segment)
		trimmedLeft := bytes.TrimLeft(line, " \t")
		leadingLen := len(line) - len(trimmedLeft)
		if !bytes.HasPrefix(trimmedLeft, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmedLeft[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
			continue
		}
		redactedPayload := RedactSensitiveJSONBytes(payload)
		if bytes.Equal(redactedPayload, payload) {
			continue
		}
		updated := make([]byte, 0, leadingLen+len("data: ")+len(redactedPayload)+len(suffix))
		updated = append(updated, line[:leadingLen]...)
		updated = append(updated, "data: "...)
		updated = append(updated, redactedPayload...)
		updated = append(updated, suffix...)
		segments[i] = updated
		changed = true
	}
	if !changed {
		return data
	}
	return bytes.Join(segments, nil)
}

func trimLineEnding(line []byte) ([]byte, []byte) {
	if len(line) == 0 {
		return line, nil
	}
	if line[len(line)-1] != '\n' {
		return line, nil
	}
	if len(line) >= 2 && line[len(line)-2] == '\r' {
		return line[:len(line)-2], line[len(line)-2:]
	}
	return line[:len(line)-1], line[len(line)-1:]
}

// MaskSensitiveQuery masks sensitive query parameters, e.g. auth_token, within the raw query string.
func MaskSensitiveQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	changed := false
	for i, part := range parts {
		if part == "" {
			continue
		}
		keyPart := part
		valuePart := ""
		if idx := strings.Index(part, "="); idx >= 0 {
			keyPart = part[:idx]
			valuePart = part[idx+1:]
		}
		decodedKey, err := url.QueryUnescape(keyPart)
		if err != nil {
			decodedKey = keyPart
		}
		if !shouldMaskQueryParam(decodedKey) {
			continue
		}
		decodedValue, err := url.QueryUnescape(valuePart)
		if err != nil {
			decodedValue = valuePart
		}
		masked := HideAPIKey(strings.TrimSpace(decodedValue))
		parts[i] = keyPart + "=" + url.QueryEscape(masked)
		changed = true
	}
	if !changed {
		return raw
	}
	return strings.Join(parts, "&")
}

func shouldMaskQueryParam(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	key = strings.TrimSuffix(key, "[]")
	if key == "key" || strings.Contains(key, "api-key") || strings.Contains(key, "apikey") || strings.Contains(key, "api_key") {
		return true
	}
	if strings.Contains(key, "token") || strings.Contains(key, "secret") {
		return true
	}
	return false
}
