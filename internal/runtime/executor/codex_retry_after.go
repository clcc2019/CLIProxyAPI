package executor

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
	"github.com/tidwall/gjson"
)

func parseCodexRetryAfter(statusCode int, errorBody []byte, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests || len(errorBody) == 0 {
		return nil
	}
	if !isCodexUsageLimitError(errorBody) {
		return nil
	}
	if resetsAt := gjson.GetBytes(errorBody, "error.resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			retryAfter := resetAtTime.Sub(now)
			return &retryAfter
		}
	}
	if resetsInSeconds := gjson.GetBytes(errorBody, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
		retryAfter := time.Duration(resetsInSeconds) * time.Second
		return &retryAfter
	}
	if retryAfter := parseCodexRetryAfterMessage(errorBody, now); retryAfter != nil {
		return retryAfter
	}
	return nil
}

func parseCodexRetryAfterMessage(errorBody []byte, now time.Time) *time.Duration {
	candidates := []string{
		gjson.GetBytes(errorBody, "error.retry_at").String(),
		gjson.GetBytes(errorBody, "error.try_again_at").String(),
		gjson.GetBytes(errorBody, "error.message").String(),
		gjson.GetBytes(errorBody, "message").String(),
	}
	for _, candidate := range candidates {
		if retryAfter := parseCodexRetryAfterCandidate(candidate, now); retryAfter != nil {
			return retryAfter
		}
	}
	return nil
}

func parseCodexRetryAfterCandidate(candidate string, now time.Time) *time.Duration {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return nil
	}
	normalized := normalizeCodexOrdinalDaySuffixes(candidate)
	if retryAfter := parseCodexRetryAfterCandidateValue(normalized, now); retryAfter != nil {
		return retryAfter
	}
	if idx := asciifold.Index(normalized, "try again at "); idx >= 0 {
		return parseCodexRetryAfterCandidateValue(normalized[idx+len("try again at "):], now)
	}
	return nil
}

func normalizeCodexOrdinalDaySuffixes(value string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(value); i++ {
		if !isASCIIDigit(value[i]) || (i > 0 && isASCIIWord(value[i-1])) {
			continue
		}
		j := i
		for j < len(value) && isASCIIDigit(value[j]) && j-i < 2 {
			j++
		}
		if j == i || j+2 > len(value) || (j < len(value) && isASCIIDigit(value[j])) {
			continue
		}
		if !isCodexOrdinalSuffix(value[j : j+2]) {
			continue
		}
		if j+2 < len(value) && isASCIIWord(value[j+2]) {
			continue
		}
		if b.Cap() == 0 {
			b.Grow(len(value))
		}
		b.WriteString(value[last:i])
		b.WriteString(value[i:j])
		last = j + 2
		i = j + 1
	}
	if b.Cap() == 0 {
		return value
	}
	b.WriteString(value[last:])
	return b.String()
}

func isCodexOrdinalSuffix(s string) bool {
	switch s {
	case "st", "nd", "rd", "th":
		return true
	default:
		return false
	}
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isASCIIWord(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

func parseCodexRetryAfterCandidateValue(value string, now time.Time) *time.Duration {
	value = strings.TrimSpace(strings.TrimSuffix(strings.Trim(value, `"'`), "."))
	if value == "" {
		return nil
	}
	if retryAt, ok := parseCodexRetryAtTime(value, now.Location()); ok {
		retryAfter := retryAt.Sub(now)
		if retryAfter > 0 {
			return &retryAfter
		}
	}
	return nil
}

func parseCodexRetryAtTime(value string, loc *time.Location) (time.Time, bool) {
	layoutsWithLocation := []string{
		"January 2, 2006 3:04:05 PM",
		"January 2, 2006 3:04 PM",
		"Jan 2, 2006 3:04:05 PM",
		"Jan 2, 2006 3:04 PM",
	}
	for _, layout := range layoutsWithLocation {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			return parsed, true
		}
	}

	layouts := []string{
		"January 2, 2006 3:04:05 PM MST",
		"January 2, 2006 3:04 PM MST",
		"Jan 2, 2006 3:04:05 PM MST",
		"Jan 2, 2006 3:04 PM MST",
		time.RFC3339,
		time.RFC1123,
		time.RFC1123Z,
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
