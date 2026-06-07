package handlers

import "net/http"

// gatewayHeaderPrefixes lists header name prefixes injected by known AI gateway
// proxies. Claude Code's client-side telemetry detects these and reports the
// gateway type, so we strip them from upstream responses to avoid detection.
var gatewayHeaderPrefixes = []string{
	"x-litellm-",
	"helicone-",
	"x-portkey-",
	"cf-aig-",
	"x-kong-",
	"x-bt-",
}

// hopByHopHeaders lists RFC 7230 Section 6.1 hop-by-hop headers that MUST NOT
// be forwarded by proxies, plus security-sensitive headers that should not leak.
var hopByHopHeaders = map[string]struct{}{
	// RFC 7230 hop-by-hop
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	// Security-sensitive
	"Set-Cookie": {},
	// CPA-managed (set by handlers, not upstream)
	"Content-Length":   {},
	"Content-Encoding": {},
}

// FilterUpstreamHeaders returns a copy of src with hop-by-hop and security-sensitive
// headers removed. Returns nil if src is nil or empty after filtering.
func FilterUpstreamHeaders(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	connectionValues := src.Values("Connection")
	var dst http.Header
	for key, values := range src {
		if isBlockedUpstreamHeader(key) {
			continue
		}
		if len(connectionValues) > 0 && isConnectionScopedHeader(key, connectionValues) {
			continue
		}
		// Strip headers injected by known AI gateway proxies to avoid
		// Claude Code client-side gateway detection.
		if isGatewayHeader(key) {
			continue
		}
		if dst == nil {
			dst = make(http.Header)
		}
		dst[key] = values
	}
	return dst
}

// WriteUpstreamHeaders writes filtered upstream headers to the gin response writer.
// Headers already set by CPA (e.g., Content-Type) are NOT overwritten.
func WriteUpstreamHeaders(dst http.Header, src http.Header) {
	if src == nil {
		return
	}
	for key, values := range src {
		// Don't overwrite headers already set by CPA handlers
		if dst.Get(key) != "" {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}
