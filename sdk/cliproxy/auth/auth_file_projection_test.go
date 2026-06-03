package auth

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNewAuthFromAuthFileMetadataAppliesCommonProjection(t *testing.T) {
	now := time.Date(2026, 5, 20, 8, 30, 0, 0, time.UTC)
	baseDir := filepath.Join("tmp", "auths")
	path := filepath.Join(baseDir, "team", "codex.json")

	auth := NewAuthFromAuthFileMetadata(map[string]any{
		"type":      "codex",
		"email":     "user@example.com",
		"prefix":    "team-a",
		"proxy_url": "http://127.0.0.1:7890",
		"priority":  float64(7),
		"note":      "primary account",
		"headers": map[string]any{
			"User-Agent": "Codex/1.0",
		},
		"websockets": true,
		"disabled":   true,
	}, AuthFileProjectionOptions{
		Path:                   path,
		BaseDir:                baseDir,
		UseBaseNameAsFileName:  true,
		IncludeSourceAttribute: true,
		Now:                    now,
	})

	if auth.ID != filepath.Join("team", "codex.json") {
		t.Fatalf("ID = %q, want relative auth path", auth.ID)
	}
	if auth.FileName != "codex.json" {
		t.Fatalf("FileName = %q, want basename", auth.FileName)
	}
	if auth.Provider != "codex" || auth.Label != "user@example.com" {
		t.Fatalf("provider/label = %q/%q", auth.Provider, auth.Label)
	}
	if !auth.Disabled || auth.Status != StatusDisabled {
		t.Fatalf("disabled/status = %v/%s", auth.Disabled, auth.Status)
	}
	if auth.Prefix != "team-a" || auth.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("prefix/proxy = %q/%q", auth.Prefix, auth.ProxyURL)
	}
	if auth.Attributes["path"] != path || auth.Attributes["source"] != path {
		t.Fatalf("path/source attrs not projected: %#v", auth.Attributes)
	}
	if auth.Attributes["priority"] != "7" || auth.Attributes["note"] != "primary account" {
		t.Fatalf("editable attrs not projected: %#v", auth.Attributes)
	}
	if auth.Attributes["header:User-Agent"] != "Codex/1.0" {
		t.Fatalf("headers not projected: %#v", auth.Attributes)
	}
	if auth.Attributes["websockets"] != "true" {
		t.Fatalf("websockets not projected: %#v", auth.Attributes)
	}
	if !auth.CreatedAt.Equal(now) || !auth.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps = %s/%s, want %s", auth.CreatedAt, auth.UpdatedAt, now)
	}
}

func TestNewAuthFromAuthFileMetadataAppliesCodexClientProfileFields(t *testing.T) {
	auth := NewAuthFromAuthFileMetadata(map[string]any{
		"type":                   "codex",
		"originator":             " codex_vscode ",
		"beta_features":          " feature-a,feature-b ",
		"installation_id":        " install-1 ",
		"include_timing_metrics": "true",
	}, AuthFileProjectionOptions{ID: "codex.json"})

	want := map[string]string{
		"originator":                                   "codex_vscode",
		"header:Originator":                            "codex_vscode",
		"header:X-Codex-Beta-Features":                 "feature-a,feature-b",
		"header:X-Codex-Installation-Id":               "install-1",
		"header:x-responsesapi-include-timing-metrics": "true",
	}
	for key, expected := range want {
		if got := auth.Attributes[key]; got != expected {
			t.Fatalf("Attributes[%q] = %q, want %q; attrs=%#v", key, got, expected, auth.Attributes)
		}
	}
}

func TestNewAuthFromAuthFileMetadataClearsCodexClientProfileHeaderAliases(t *testing.T) {
	auth := &Auth{
		Attributes: map[string]string{
			"originator":                                   "old",
			"header:Originator":                            "old",
			"beta_features":                                "old",
			"header:X-Codex-Beta-Features":                 "old",
			"installation_id":                              "old",
			"header:X-Codex-Installation-Id":               "old",
			"include_timing_metrics":                       "old",
			"header:x-responsesapi-include-timing-metrics": "old",
		},
		Metadata: map[string]any{
			"originator":             "",
			"beta-features":          "",
			"installationId":         "",
			"include_timing_metrics": false,
		},
	}

	ApplyAuthFileOptionsFromMetadata(auth)

	for _, key := range []string{
		"originator",
		"header:Originator",
		"beta_features",
		"header:X-Codex-Beta-Features",
		"installation_id",
		"header:X-Codex-Installation-Id",
		"include_timing_metrics",
		"header:x-responsesapi-include-timing-metrics",
	} {
		if _, ok := auth.Attributes[key]; ok {
			t.Fatalf("Attributes[%q] should be removed when auth-file field is empty/false: %#v", key, auth.Attributes)
		}
	}
}

func TestNewAuthFromAuthFileMetadataAppliesProxyAliasesAndLegacyWebsocket(t *testing.T) {
	auth := NewAuthFromAuthFileMetadata(map[string]any{
		"type":      "codex",
		"proxy-url": " none ",
		"websocket": "false",
	}, AuthFileProjectionOptions{Path: "/auth/codex.json", BaseDir: "/auth"})

	if auth.ProxyURL != "none" {
		t.Fatalf("ProxyURL = %q, want none", auth.ProxyURL)
	}
	if got := auth.Metadata["proxy-url"]; got != " none " {
		t.Fatalf("proxy-url metadata = %v, want preserved raw value", got)
	}
	if got := auth.Attributes["websockets"]; got != "false" {
		t.Fatalf("websockets attr = %q, want false", got)
	}
}

func TestNewAuthFromAuthFileDataNormalizesImportedOpenAISession(t *testing.T) {
	auth, err := NewAuthFromAuthFileData([]byte(`{
		"authProvider":"openai",
		"accessToken":"token-1",
		"user":{"email":"user@example.com"},
		"account":{"id":"acct-1","planType":"plus"},
		"user_agent":"Codex/1.0"
	}`), AuthFileProjectionOptions{Path: "/auth/codex.json", BaseDir: "/auth"})
	if err != nil {
		t.Fatalf("NewAuthFromAuthFileData error: %v", err)
	}

	if auth.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", auth.Provider)
	}
	if auth.Metadata["access_token"] != "token-1" || auth.Metadata["account_id"] != "acct-1" {
		t.Fatalf("metadata was not normalized: %#v", auth.Metadata)
	}
	if auth.Attributes["email"] != "user@example.com" || auth.Attributes["plan_type"] != "plus" {
		t.Fatalf("codex attributes not projected: %#v", auth.Attributes)
	}
	if auth.Attributes["header:User-Agent"] != "Codex/1.0" {
		t.Fatalf("user agent was not projected: %#v", auth.Attributes)
	}
}

func TestNewAuthFromAuthFileMetadataIgnoresNonStringEditableText(t *testing.T) {
	auth := NewAuthFromAuthFileMetadata(map[string]any{
		"type":       "codex",
		"note":       12345,
		"user_agent": 67890,
	}, AuthFileProjectionOptions{ID: "codex.json"})

	if _, ok := auth.Attributes["note"]; ok {
		t.Fatalf("numeric note should not be projected: %#v", auth.Attributes)
	}
	if _, ok := auth.Attributes["header:User-Agent"]; ok {
		t.Fatalf("numeric user agent should not be projected: %#v", auth.Attributes)
	}
}

func TestPrepareAuthFileMetadataForSaveSetsDisabledAndCleansKiro(t *testing.T) {
	auth := &Auth{
		Disabled: true,
		Metadata: map[string]any{
			"type":         "kiro",
			"email":        "  ",
			"profile_arn":  nil,
			"machine_id":   "machine-1",
			"access_token": "token-1",
		},
	}

	metadata := PrepareAuthFileMetadataForSave(auth)
	if disabled, _ := metadata["disabled"].(bool); !disabled {
		t.Fatalf("disabled = %#v, want true", metadata["disabled"])
	}
	if _, ok := metadata["email"]; ok {
		t.Fatalf("empty Kiro email should be removed: %#v", metadata)
	}
	if _, ok := metadata["profile_arn"]; ok {
		t.Fatalf("nil Kiro profile_arn should be removed: %#v", metadata)
	}
	if metadata["machine_id"] != "machine-1" || metadata["access_token"] != "token-1" {
		t.Fatalf("non-empty metadata should be preserved: %#v", metadata)
	}
}

func TestPrepareAuthFileMetadataForSaveCreatesMetadata(t *testing.T) {
	auth := &Auth{}
	metadata := PrepareAuthFileMetadataForSave(auth)
	if metadata == nil || auth.Metadata == nil {
		t.Fatal("metadata should be created")
	}
	if disabled, ok := metadata["disabled"].(bool); !ok || disabled {
		t.Fatalf("disabled = %#v, want false", metadata["disabled"])
	}
}
