package util

import "strings"

// IsWebSearchTool checks if a tool name or type indicates web search capability.
func IsWebSearchTool(name, toolType string) bool {
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, "web_search") {
		return true
	}

	toolType = strings.TrimSpace(toolType)
	return hasPrefixEqualFold(toolType, "web_search")
}

func hasPrefixEqualFold(value, prefix string) bool {
	if len(value) < len(prefix) {
		return false
	}
	return strings.EqualFold(value[:len(prefix)], prefix)
}
