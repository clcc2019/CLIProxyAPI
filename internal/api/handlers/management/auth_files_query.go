package management

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var lastRefreshKeys = []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"}

const (
	anthropicCallbackPort    = 54545
	codexCallbackPort        = 1455
	maxAuthFileUploadBytes   = 2 << 20
	maxAuthFilesListPageSize = 200
)

var (
	errAuthFileMustBeJSON = errors.New("auth file must be .json")
	errAuthFileNotFound   = errors.New("auth file not found")
)

func extractLastRefreshTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range lastRefreshKeys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseLastRefreshValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func authFileLastRefresh(auth *coreauth.Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	if !auth.LastRefreshedAt.IsZero() {
		return auth.LastRefreshedAt.UTC(), true
	}
	if ts, ok := extractLastRefreshTimestamp(auth.Metadata); ok {
		return ts, true
	}
	for _, key := range lastRefreshKeys {
		if val := strings.TrimSpace(authAttribute(auth, key)); val != "" {
			if ts, ok := parseLastRefreshValue(val); ok {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func parseLastRefreshValue(v any) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	case float64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case int64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(val, 0).UTC(), true
	case int:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case json.Number:
		if i, err := val.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

func isWebUIRequest(c *gin.Context) bool {
	return isTruthyQueryValue(c.Query("is_webui"))
}

func isTruthyQueryValue(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	switch {
	case raw == "1":
		return true
	case strings.EqualFold(raw, "true"):
		return true
	case strings.EqualFold(raw, "yes"):
		return true
	case strings.EqualFold(raw, "on"):
		return true
	default:
		return false
	}
}

func isRefreshQueryValue(raw string) bool {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "1":
		return true
	case strings.EqualFold(raw, "refresh"):
		return true
	case strings.EqualFold(raw, "force"):
		return true
	case strings.EqualFold(raw, "fetch"):
		return true
	default:
		return isTruthyQueryValue(raw)
	}
}

func isSkipQueryValue(raw string) bool {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "0":
		return true
	case strings.EqualFold(raw, "skip"):
		return true
	case strings.EqualFold(raw, "none"):
		return true
	case strings.EqualFold(raw, "off"):
		return true
	case strings.EqualFold(raw, "false"):
		return true
	case strings.EqualFold(raw, "no"):
		return true
	default:
		return false
	}
}

func firstNonEmptyQueryValue(c *gin.Context, keys ...string) string {
	if c == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(c.Query(key)); value != "" {
			return value
		}
	}
	return ""
}
