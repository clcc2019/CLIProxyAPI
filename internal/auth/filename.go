package auth

import (
	"strconv"
	"strings"
	"time"
)

// CredentialFileName returns a stable credential filename for an account.
// Email is preferred; fallbacks are used only when email is unavailable.
func CredentialFileName(email string, fallbacks ...string) string {
	for _, value := range append([]string{email}, fallbacks...) {
		segment := sanitizeCredentialFileNameSegment(value)
		if segment != "" {
			return segment + ".json"
		}
	}

	return strconv.FormatInt(time.Now().UnixMilli(), 10) + ".json"
}

func sanitizeCredentialFileNameSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(value))

	lastDash := false
	for _, r := range value {
		if isCredentialFileNameRune(r) {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}

	return strings.Trim(builder.String(), "-.")
}

func isCredentialFileNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '@' ||
		r == '.' ||
		r == '_' ||
		r == '-' ||
		r == '+'
}
