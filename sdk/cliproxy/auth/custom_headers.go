package auth

import "strings"

func ExtractCustomHeadersFromMetadata(metadata map[string]any) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	raw, ok := metadata["headers"]
	if !ok || raw == nil {
		return nil
	}

	out := make(map[string]string)
	switch headers := raw.(type) {
	case map[string]string:
		for key, value := range headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			val := strings.TrimSpace(value)
			if val == "" {
				continue
			}
			out[name] = val
		}
	case map[string]any:
		for key, value := range headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			rawVal, ok := value.(string)
			if !ok {
				continue
			}
			val := strings.TrimSpace(rawVal)
			if val == "" {
				continue
			}
			out[name] = val
		}
	default:
		return nil
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func ApplyCustomHeadersFromMetadata(auth *Auth) {
	if auth == nil || len(auth.Metadata) == 0 {
		return
	}
	headers := extractClientProfileHeadersFromMetadata(auth.Metadata)
	if len(headers) == 0 {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for name, value := range headers {
		auth.Attributes["header:"+name] = value
	}
}

// extractClientProfileHeadersFromMetadata returns custom headers declared at
// the auth-file root or inside a client_profile/client_features object. Nested
// profile headers are part of the credential's fixed Codex fingerprint, so
// they must be projected into runtime attributes just like root headers.
//
// Root headers take precedence over nested profile headers. For multiple
// profile aliases, the order matches authFileClientProfileString: the first
// alias wins.
func extractClientProfileHeadersFromMetadata(metadata map[string]any) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	merged := make(map[string]string)
	mergeHeaders := func(headers map[string]string) {
		for name, value := range headers {
			merged[name] = value
		}
	}

	// Apply lower-priority aliases first so the first alias in the canonical
	// ordering wins when more than one is present.
	for index := len(authFileClientProfileObjectKeys) - 1; index >= 0; index-- {
		nested, ok := authFileNestedMetadata(metadata, authFileClientProfileObjectKeys[index])
		if !ok {
			continue
		}
		mergeHeaders(ExtractCustomHeadersFromMetadata(nested))
	}
	mergeHeaders(ExtractCustomHeadersFromMetadata(metadata))
	if len(merged) == 0 {
		return nil
	}
	return merged
}
