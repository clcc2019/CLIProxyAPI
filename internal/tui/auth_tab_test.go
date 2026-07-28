//go:build has_tui

package tui

import (
	"strings"
	"testing"
)

func TestAuthTabRenderDetailIncludesLastErrorAndModelState(t *testing.T) {
	model := newAuthTabModel(nil)

	detail := model.renderDetail(map[string]any{
		"name":           "claude-auth.json",
		"status":         "error",
		"status_message": "request failed",
		"last_error": map[string]any{
			"code":        "upstream_failure",
			"message":     "provider 502",
			"http_status": 502,
			"retryable":   true,
		},
		"model_states": map[string]any{
			"claude-sonnet-4-5": map[string]any{
				"status":         "error",
				"status_message": "quota exhausted",
				"last_error": map[string]any{
					"message":     "429 too many requests",
					"http_status": 429,
				},
			},
		},
	})

	for _, want := range []string{
		"Last Error",
		"provider 502",
		"Error Code",
		"upstream_failure",
		"HTTP Status",
		"502",
		"claude-sonnet-4-5",
		"quota exhausted",
		"HTTP 429",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("renderDetail() missing %q in:\n%s", want, detail)
		}
	}
}
