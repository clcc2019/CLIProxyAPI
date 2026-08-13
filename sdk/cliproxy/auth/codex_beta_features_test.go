package auth

import "testing"

func TestSanitizeCodexAuthMetadataRemovesRemoteCompactionV2Everywhere(t *testing.T) {
	metadata := map[string]any{
		"type":          "codex",
		"beta_features": "keep-root, remote_compaction_v2",
		"headers": map[string]any{
			"X-Codex-Beta-Features": "remote_compaction_v2",
			"X-Keep":                "value",
		},
		"agentIdentity": map[string]any{
			"clientFeatures": map[string]any{
				"headers": map[string]string{
					"X-Codex-Beta-Features": "keep-nested REMOTE_COMPACTION_V2",
				},
			},
		},
	}

	sanitized, changed := SanitizeCodexAuthMetadata(metadata)
	if !changed {
		t.Fatal("SanitizeCodexAuthMetadata() changed = false, want true")
	}
	if got := sanitized["beta_features"]; got != "keep-root" {
		t.Fatalf("beta_features = %#v, want keep-root", got)
	}
	headers := sanitized["headers"].(map[string]any)
	if _, exists := headers["X-Codex-Beta-Features"]; exists {
		t.Fatalf("empty beta feature header was retained: %#v", headers)
	}
	if got := headers["X-Keep"]; got != "value" {
		t.Fatalf("unrelated header = %#v, want value", got)
	}
	nested := sanitized["agentIdentity"].(map[string]any)["clientFeatures"].(map[string]any)["headers"].(map[string]string)
	if got := nested["X-Codex-Beta-Features"]; got != "keep-nested" {
		t.Fatalf("nested beta features = %q, want keep-nested", got)
	}

	if got := metadata["beta_features"]; got != "keep-root, remote_compaction_v2" {
		t.Fatalf("input metadata was mutated: beta_features = %#v", got)
	}
	originalHeaders := metadata["headers"].(map[string]any)
	if got := originalHeaders["X-Codex-Beta-Features"]; got != "remote_compaction_v2" {
		t.Fatalf("input nested headers were mutated: %#v", originalHeaders)
	}
}

func TestStripNonPersistentCodexFeaturesRemovesProjectedAttributes(t *testing.T) {
	auth := &Auth{
		Metadata: map[string]any{"betaFeatures": "remote_compaction_v2,keep"},
		Attributes: map[string]string{
			"header:X-Codex-Beta-Features": "keep, remote_compaction_v2",
			"keep":                         "value",
		},
	}

	if changed := StripNonPersistentCodexFeatures(auth); !changed {
		t.Fatal("StripNonPersistentCodexFeatures() changed = false, want true")
	}
	if got := auth.Metadata["betaFeatures"]; got != "keep" {
		t.Fatalf("metadata betaFeatures = %#v, want keep", got)
	}
	if got := auth.Attributes["header:X-Codex-Beta-Features"]; got != "keep" {
		t.Fatalf("attribute beta features = %q, want keep", got)
	}
	if got := auth.Attributes["keep"]; got != "value" {
		t.Fatalf("unrelated attribute = %q, want value", got)
	}
}
