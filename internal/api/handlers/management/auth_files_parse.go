package management

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

func boolFromGJSON(data []byte, keys ...string) (bool, bool) {
	for _, key := range keys {
		value := gjson.GetBytes(data, key)
		if !value.Exists() {
			continue
		}
		switch value.Type {
		case gjson.True:
			return true, true
		case gjson.False:
			return false, true
		case gjson.String:
			parsed, err := strconv.ParseBool(strings.TrimSpace(value.String()))
			if err == nil {
				return parsed, true
			}
		}
	}
	return false, false
}

func firstTrimmedGJSONString(data []byte, keys ...string) string {
	for _, key := range keys {
		value := gjson.GetBytes(data, key)
		if !value.Exists() || value.Type != gjson.String {
			continue
		}
		if trimmed := strings.TrimSpace(value.String()); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
