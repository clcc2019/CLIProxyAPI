package util

import (
	"net/http"
	"strings"
)

// ApplyCustomHeadersFromAttrs applies user-defined headers stored in the provided attributes map.
// Custom headers override built-in defaults when conflicts occur.
func ApplyCustomHeadersFromAttrs(r *http.Request, attrs map[string]string) bool {
	if r == nil || len(attrs) == 0 {
		return false
	}
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	applied := false
	for rawKey, rawValue := range attrs {
		if !strings.HasPrefix(rawKey, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(rawKey, "header:"))
		if name == "" {
			continue
		}
		val := strings.TrimSpace(rawValue)
		if val == "" {
			continue
		}
		// net/http reads Host from req.Host (not req.Header) when writing
		// a real request, so we must mirror it there. Some callers pass
		// synthetic requests (e.g. &http.Request{Header: ...}) and only
		// consume r.Header afterwards, so keep the value in the header
		// map too.
		canonicalName := http.CanonicalHeaderKey(name)
		if canonicalName == "Host" {
			r.Host = val
		}
		setSingleHeaderValue(r.Header, canonicalName, val)
		applied = true
	}
	return applied
}

func setSingleHeaderValue(headers http.Header, key string, value string) {
	if headers == nil {
		return
	}
	if values := headers[key]; len(values) > 0 {
		values[0] = value
		headers[key] = values[:1]
		return
	}
	headers[key] = []string{value}
}
