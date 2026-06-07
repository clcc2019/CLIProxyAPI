package handlers

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/asciifold"
)

func isBlockedUpstreamHeader(key string) bool {
	if _, blocked := hopByHopHeaders[key]; blocked {
		return true
	}
	switch len(key) {
	case len("Te"):
		return asciifold.Equal(key, "Te")
	case len("Trailer"):
		return asciifold.Equal(key, "Trailer") || asciifold.Equal(key, "Upgrade")
	case len("Set-Cookie"):
		return asciifold.Equal(key, "Set-Cookie") || asciifold.Equal(key, "Connection") || asciifold.Equal(key, "Keep-Alive")
	case len("Content-Length"):
		return asciifold.Equal(key, "Content-Length")
	case len("Content-Encoding"):
		return asciifold.Equal(key, "Content-Encoding")
	case len("Transfer-Encoding"):
		return asciifold.Equal(key, "Transfer-Encoding")
	case len("Proxy-Authenticate"):
		return asciifold.Equal(key, "Proxy-Authenticate")
	case len("Proxy-Authorization"):
		return asciifold.Equal(key, "Proxy-Authorization")
	default:
		return false
	}
}

func isGatewayHeader(key string) bool {
	for _, prefix := range gatewayHeaderPrefixes {
		if asciifold.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func isConnectionScopedHeader(key string, values []string) bool {
	for _, rawValue := range values {
		for rawValue != "" {
			token, rest, found := strings.Cut(rawValue, ",")
			rawValue = rest
			if asciifold.Equal(trimHTTPTokenSpace(token), key) {
				return true
			}
			if !found {
				break
			}
		}
	}
	return false
}

func trimHTTPTokenSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && isHTTPTokenSpace(s[start]) {
		start++
	}
	for start < end && isHTTPTokenSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isHTTPTokenSpace(b byte) bool {
	return b == ' ' || b == '\t'
}
