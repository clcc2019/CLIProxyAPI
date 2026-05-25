package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestApplyCodexHeadersOmitsEmptyAuthorizationToken(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer stale")

	applyCodexHeaders(req, nil, "  ", true, nil)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
}

func TestApplyCodexHeadersAllowsCustomAuthorizationWithoutToken(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"header:Authorization": "Bearer custom",
	}}

	applyCodexHeaders(req, auth, "", true, nil)

	if got := req.Header.Get("Authorization"); got != "Bearer custom" {
		t.Fatalf("Authorization = %q, want custom bearer", got)
	}
}
