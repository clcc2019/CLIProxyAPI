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
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
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
	out, changed, ok := redactSensitiveJSONBytes(trimmed)
	if !ok {
		return data
	}
	if !changed {
		return data
	}
	return out
}

func redactSensitiveJSONBytes(trimmed []byte) ([]byte, bool, bool) {
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, false, false
	}
	changed := false
	if text, ok := value.(string); ok {
		redacted := redactSensitiveJSONString(text)
		if redacted != text {
			value = redacted
			changed = true
		}
	} else {
		changed = redactSensitiveJSONValue(value)
	}
	if !changed {
		return trimmed, false, true
	}
	out, err := json.Marshal(value)
	if err != nil || len(out) == 0 {
		return nil, false, false
	}
	return out, true, true
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
	// Most streamed response events contain no credential-shaped keys or text.
	// Avoid unmarshalling and re-marshalling every JSON/SSE frame when the cheap
	// ASCII-folded marker scan or the allocation-free string classifier proves
	// that none of the redaction rules can match.
	// Unicode escapes and literal runes that lower-case to ASCII can hide an
	// otherwise visible marker (for example, "\\u0061uth" or "toKen"). Keep
	// those payloads on the full parser path so the prefilter cannot weaken
	// redaction.
	if !mayHideSensitiveASCIIText(trimmed) {
		hasNonTokenMarker := mayContainNonTokenSensitiveTextBytes(trimmed)
		hasTokenMarker := asciifold.ContainsBytes(trimmed, "token")
		if !hasNonTokenMarker && !hasTokenMarker {
			return data
		}
		tokenOnly := hasTokenMarker && !hasNonTokenMarker
		jsonCandidate := trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"'
		validJSON := jsonCandidate && json.Valid(trimmed)
		if validJSON {
			if jsonContainsNoSensitiveCredentialsValid(trimmed, tokenOnly) {
				return data
			}
		} else if textOrSSEContainsNoSensitiveCredentials(trimmed, tokenOnly) {
			return data
		}
	}
	if out, changed, ok := redactSensitiveJSONBytes(trimmed); ok {
		if !changed {
			return data
		}
		return out
	}
	return redactSensitivePlainTextBytes(redactSensitiveSSEDataBytes(data))
}

// mayHideSensitiveASCIIText catches encodings that can evade the raw ASCII
// marker scan while still matching the Unicode-aware normalization used by the
// full redactor. Ordinary non-ASCII text (for example CJK output) remains on
// the fast path when none of its runes lower-case to ASCII.
func mayHideSensitiveASCIIText(data []byte) bool {
	for i := 0; i < len(data); {
		value := data[i]
		if value == '\\' && i+1 < len(data) && data[i+1] == 'u' {
			return true
		}
		if value < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			return true
		}
		lower := unicode.ToLower(r)
		if lower >= 'a' && lower <= 'z' {
			return true
		}
		i += size
	}
	return false
}

// jsonContainsNoSensitiveCredentials scans the strings in valid JSON without
// decoding the value. It returns true only when none of those strings can be
// changed by the full JSON redactor. Unknown token-shaped keys and escaped
// marker-bearing strings deliberately return false to retain the conservative
// parser fallback.
func jsonContainsNoSensitiveCredentials(data []byte) bool {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return false
	}
	if !json.Valid(data) {
		return false
	}
	if mayHideSensitiveASCIIText(data) {
		return false
	}
	hasNonTokenMarker := mayContainNonTokenSensitiveTextBytes(data)
	hasTokenMarker := asciifold.ContainsBytes(data, "token")
	if !hasNonTokenMarker && !hasTokenMarker {
		return true
	}
	return jsonContainsNoSensitiveCredentialsValid(data, hasTokenMarker && !hasNonTokenMarker)
}

func jsonContainsNoSensitiveCredentialsValid(data []byte, tokenOnly bool) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		end := -1
		escaped := false
		for j := i + 1; j < len(data); j++ {
			switch data[j] {
			case '\\':
				escaped = true
				j++
			case '"':
				end = j
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return false
		}
		raw := data[i+1 : end]
		i = end
		if escaped {
			return false
		}
		if tokenOnly {
			if !asciifold.ContainsBytes(raw, "token") {
				continue
			}
		} else if !mayContainSensitiveTextBytes(raw) {
			continue
		}
		next := end + 1
		for next < len(data) && isJSONWhitespace(data[next]) {
			next++
		}
		if next < len(data) && data[next] == ':' {
			if asciifold.ContainsBytes(raw, "token") {
				if !benignTokenUsageJSONKey(raw) {
					return false
				}
				continue
			}
			if shouldRedactJSONKey(string(raw)) {
				return false
			}
			continue
		}
		if plainTextContainsSensitiveCredential(raw) {
			return false
		}
	}
	return true
}

// textOrSSEContainsNoSensitiveCredentials extends the JSON classifier to SSE
// data fields and ordinary text lines. Marker-bearing non-data lines are safe
// only when none of the plain-text replacement patterns match.
func textOrSSEContainsNoSensitiveCredentials(data []byte, tokenOnly bool) bool {
	for len(data) > 0 {
		line := data
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			line = data[:newline]
			data = data[newline+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSpace(line)
		if tokenOnly {
			if !asciifold.ContainsBytes(line, "token") {
				continue
			}
		} else if !mayContainSensitiveTextBytes(line) {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			if plainTextContainsSensitiveCredential(line) || plainTextLineMayContinueCredential(line) {
				return false
			}
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if !json.Valid(payload) || !jsonContainsNoSensitiveCredentialsValid(payload, tokenOnly) {
			return false
		}
	}
	return true
}

// plainTextLineMayContinueCredential recognizes incomplete matches that the
// full plain-text expressions could legally continue across a newline through
// their \s* / \s+ segments (for example, "access_token=\nvalue").
func plainTextLineMayContinueCredential(line []byte) bool {
	line = bytes.TrimSpace(line)
	for len(line) > 0 && (line[len(line)-1] == ':' || line[len(line)-1] == '=') {
		line = bytes.TrimSpace(line[:len(line)-1])
	}
	for _, suffix := range plainCredentialContinuationSuffixes {
		if asciiBytesHasSuffixFold(line, suffix) {
			return true
		}
	}
	return false
}

var plainCredentialContinuationSuffixes = [...]string{
	"authorization",
	"bearer",
	"basic",
	"apikey",
	"api_key",
	"api-key",
	"api key",
	"api_keys",
	"api-keys",
	"api keys",
	"access_token",
	"access-token",
	"access token",
	"accesstoken",
	"access_tokens",
	"access-tokens",
	"access tokens",
	"accesstokens",
	"refresh_token",
	"refresh-token",
	"refresh token",
	"refreshtoken",
	"refresh_tokens",
	"refresh-tokens",
	"refresh tokens",
	"refreshtokens",
	"id_token",
	"id-token",
	"id token",
	"idtoken",
	"id_tokens",
	"id-tokens",
	"id tokens",
	"idtokens",
	"session_token",
	"session-token",
	"session token",
	"sessiontoken",
	"session_tokens",
	"session-tokens",
	"session tokens",
	"sessiontokens",
	"bearer_token",
	"bearer-token",
	"bearer token",
	"bearertoken",
	"bearer_tokens",
	"bearer-tokens",
	"bearer tokens",
	"bearertokens",
	"client_secret",
	"client-secret",
	"client secret",
	"clientsecret",
	"client_secrets",
	"client-secrets",
	"client secrets",
	"clientsecrets",
	"private_key",
	"private-key",
	"private key",
	"privatekey",
	"agent_private_key",
	"agent-private-key",
	"agent private key",
	"agentprivatekey",
	"password",
	"passwords",
	"passcode",
	"passcodes",
	"credential",
	"credentials",
}

func benignTokenUsageJSONKey(key []byte) bool {
	// Keep this as an explicit allowlist. Broadly accepting every *_tokens key
	// would misclassify credential collections such as access_tokens or
	// refresh_tokens as usage counters and bypass the full JSON redactor.
	for _, known := range benignTokenUsageJSONKeys {
		if asciiBytesEqualFold(key, known) {
			return true
		}
	}
	return false
}

var benignTokenUsageJSONKeys = [...]string{
	"tokens",
	"token_usage",
	"input_tokens",
	"output_tokens",
	"total_tokens",
	"prompt_tokens",
	"completion_tokens",
	"cached_tokens",
	"reasoning_tokens",
	"audio_tokens",
	"accepted_prediction_tokens",
	"rejected_prediction_tokens",
	"cache_creation_input_tokens",
	"cache_read_input_tokens",
	"input_tokens_details",
	"output_tokens_details",
	"prompt_tokens_details",
	"completion_tokens_details",
}

func asciiBytesEqualFold(data []byte, lowerASCII string) bool {
	if len(data) != len(lowerASCII) {
		return false
	}
	for i := 0; i < len(data); i++ {
		value := data[i]
		if value >= 'A' && value <= 'Z' {
			value += 'a' - 'A'
		}
		if value != lowerASCII[i] {
			return false
		}
	}
	return true
}

func asciiBytesHasSuffixFold(data []byte, lowerASCII string) bool {
	if len(data) < len(lowerASCII) {
		return false
	}
	return asciiBytesEqualFold(data[len(data)-len(lowerASCII):], lowerASCII)
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

var (
	plainAuthorizationCredentialPattern = regexp.MustCompile(`(?i)\b(authorization\s*[:=]\s*)(?:(bearer|basic|apikey|agentassertion)\s+)?([^\s,;]+)`)
	plainBearerCredentialPattern        = regexp.MustCompile(`(?i)\b(bearer|basic|agentassertion)\s+([A-Za-z0-9._~+/=-]{8,})`)
	plainKeyValueCredentialPattern      = regexp.MustCompile(`(?i)\b(api[-_ ]?keys?|access[-_ ]?tokens?|refresh[-_ ]?tokens?|id[-_ ]?tokens?|session[-_ ]?tokens?|bearer[-_ ]?tokens?|client[-_ ]?secrets?|(?:agent[-_ ]?)?private[-_ ]?keys?|passwords?|passcodes?|credentials?)\b(\s*[:=]\s*)([^\s&;,]+)`)
)

func plainTextContainsSensitiveCredential(data []byte) bool {
	if asciifold.ContainsBytes(data, "authorization") && plainAuthorizationCredentialPattern.Match(data) {
		return true
	}
	if (asciifold.ContainsBytes(data, "bearer") || asciifold.ContainsBytes(data, "basic")) &&
		plainBearerCredentialPattern.Match(data) {
		return true
	}
	if (asciifold.ContainsBytes(data, "api") ||
		asciifold.ContainsBytes(data, "access") ||
		asciifold.ContainsBytes(data, "refresh") ||
		asciifold.ContainsBytes(data, "id_token") ||
		asciifold.ContainsBytes(data, "id-token") ||
		asciifold.ContainsBytes(data, "id token") ||
		asciifold.ContainsBytes(data, "idtoken") ||
		asciifold.ContainsBytes(data, "session") ||
		asciifold.ContainsBytes(data, "bearer") ||
		asciifold.ContainsBytes(data, "secret") ||
		asciifold.ContainsBytes(data, "password") ||
		asciifold.ContainsBytes(data, "passcode") ||
		asciifold.ContainsBytes(data, "credential")) &&
		plainKeyValueCredentialPattern.Match(data) {
		return true
	}
	return false
}

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

func redactSensitiveJSONValue(value any) bool {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if shouldRedactJSONKey(key) {
				if child != "[REDACTED]" {
					typed[key] = "[REDACTED]"
					changed = true
				}
				continue
			}
			if str, ok := child.(string); ok {
				redacted := redactSensitiveJSONString(str)
				if redacted != str {
					typed[key] = redacted
					changed = true
				}
				continue
			}
			if redactSensitiveJSONValue(child) {
				changed = true
			}
		}
	case []any:
		for i, child := range typed {
			if str, ok := child.(string); ok {
				redacted := redactSensitiveJSONString(str)
				if redacted != str {
					typed[i] = redacted
					changed = true
				}
				continue
			}
			if redactSensitiveJSONValue(child) {
				changed = true
			}
		}
	}
	return changed
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
	"auth",
	"bearer",
	"basic",
	"api",
	"token",
	"secret",
	"private",
	"password",
	"passcode",
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
		if asciifold.ContainsBytes(data, marker) {
			return true
		}
	}
	return false
}

func mayContainNonTokenSensitiveTextBytes(data []byte) bool {
	for _, marker := range sensitiveTextMarkers {
		if marker == "token" {
			continue
		}
		if asciifold.ContainsBytes(data, marker) {
			return true
		}
	}
	return false
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
		"secret", "client_secret", "private_key", "agent_private_key", "password", "passcode", "credential", "credentials",
		"auth_tokens", "access_tokens", "refresh_tokens", "id_tokens", "bearer_tokens",
		"session_tokens", "api_tokens", "secret_tokens":
		return true
	}
	switch compact {
	case "authtoken", "accesstoken", "refreshtoken", "idtoken", "bearertoken", "sessiontoken",
		"apikey", "xapikey", "clientsecret", "privatekey", "agentprivatekey", "authtokens", "accesstokens", "refreshtokens",
		"idtokens", "bearertokens", "sessiontokens", "apitokens", "secrettokens":
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

	var out strings.Builder
	changed := false
	copyFrom := 0
	for partStart := 0; partStart <= len(raw); {
		partEnd := len(raw)
		if separator := strings.IndexByte(raw[partStart:], '&'); separator >= 0 {
			partEnd = partStart + separator
		}
		part := raw[partStart:partEnd]
		if part == "" {
			if partEnd == len(raw) {
				break
			}
			partStart = partEnd + 1
			continue
		}

		keyPart, valuePart, _ := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(keyPart)
		if err != nil {
			decodedKey = keyPart
		}
		if shouldMaskQueryParam(decodedKey) {
			decodedValue, err := url.QueryUnescape(valuePart)
			if err != nil {
				decodedValue = valuePart
			}
			if !changed {
				out.Grow(len(raw))
				changed = true
			}
			out.WriteString(raw[copyFrom:partStart])
			out.WriteString(keyPart)
			out.WriteByte('=')
			out.WriteString(url.QueryEscape(HideAPIKey(strings.TrimSpace(decodedValue))))
			copyFrom = partEnd
		}

		if partEnd == len(raw) {
			break
		}
		partStart = partEnd + 1
	}
	if !changed {
		return raw
	}
	out.WriteString(raw[copyFrom:])
	return out.String()
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
