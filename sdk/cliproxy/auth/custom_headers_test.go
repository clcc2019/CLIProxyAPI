package auth

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
)

func TestExtractCustomHeadersFromMetadata(t *testing.T) {
	meta := map[string]any{
		"headers": map[string]any{
			" X-Test ": " value ",
			"":         "ignored",
			"X-Empty":  "   ",
			"X-Num":    float64(1),
		},
	}

	got := ExtractCustomHeadersFromMetadata(meta)
	want := map[string]string{"X-Test": "value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractCustomHeadersFromMetadata() = %#v, want %#v", got, want)
	}
}

func TestApplyCustomHeadersFromMetadata(t *testing.T) {
	auth := &Auth{
		Metadata: map[string]any{
			"headers": map[string]string{
				"X-Test":  "new",
				"X-Empty": "   ",
			},
		},
		Attributes: map[string]string{
			"header:X-Test": "old",
			"keep":          "1",
		},
	}

	ApplyCustomHeadersFromMetadata(auth)

	if got := auth.Attributes["header:X-Test"]; got != "new" {
		t.Fatalf("header:X-Test = %q, want %q", got, "new")
	}
	if _, ok := auth.Attributes["header:X-Empty"]; ok {
		t.Fatalf("expected header:X-Empty to be absent, got %#v", auth.Attributes["header:X-Empty"])
	}
	if got := auth.Attributes["keep"]; got != "1" {
		t.Fatalf("keep = %q, want %q", got, "1")
	}
}

func TestApplyCodexMetadataFromMetadataUsesTopLevelFields(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":      "codex@example.com",
			"account_id": "acct_123",
			"plan_type":  "plus",
		},
	}

	ApplyCodexMetadataFromMetadata(auth)

	if got := auth.Attributes["email"]; got != "codex@example.com" {
		t.Fatalf("email = %q, want %q", got, "codex@example.com")
	}
	if got := auth.Attributes["account_id"]; got != "acct_123" {
		t.Fatalf("account_id = %q, want %q", got, "acct_123")
	}
	if got := auth.Attributes["plan_type"]; got != "plus" {
		t.Fatalf("plan_type = %q, want %q", got, "plus")
	}
}

func TestApplyCodexMetadataFromMetadataFallsBackToIDToken(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"id_token": fakeJWT(t, map[string]any{
				"email": "jwt@example.com",
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_account_id": "acct_jwt",
					"chatgpt_plan_type":  "team",
				},
			}),
		},
	}

	ApplyCodexMetadataFromMetadata(auth)

	if got := auth.Metadata["email"]; got != "jwt@example.com" {
		t.Fatalf("metadata email = %#v, want %q", got, "jwt@example.com")
	}
	if got := auth.Metadata["account_id"]; got != "acct_jwt" {
		t.Fatalf("metadata account_id = %#v, want %q", got, "acct_jwt")
	}
	if got := auth.Metadata["plan_type"]; got != "team" {
		t.Fatalf("metadata plan_type = %#v, want %q", got, "team")
	}
	if got := auth.Attributes["plan_type"]; got != "team" {
		t.Fatalf("attribute plan_type = %q, want %q", got, "team")
	}
}

func TestNormalizeImportedAuthMetadata_ConvertsOpenAISessionExport(t *testing.T) {
	metadata := map[string]any{
		"WARNING_BANNER": "sensitive",
		"accessToken":    "access-token",
		"authProvider":   "openai",
		"user": map[string]any{
			"email": "codex@example.com",
		},
		"account": map[string]any{
			"id":                    "acct_123",
			"planType":              "plus",
			"subscriptionExpiresAt": "2026-07-01T00:00:00Z",
		},
	}

	normalized, changed := NormalizeImportedAuthMetadata(metadata)
	if !changed {
		t.Fatal("expected session export to be normalized")
	}
	if got := normalized["type"]; got != "codex" {
		t.Fatalf("type = %#v, want %q", got, "codex")
	}
	if got := normalized["access_token"]; got != "access-token" {
		t.Fatalf("access_token = %#v, want %q", got, "access-token")
	}
	if got := normalized["email"]; got != "codex@example.com" {
		t.Fatalf("email = %#v, want %q", got, "codex@example.com")
	}
	if got := normalized["account_id"]; got != "acct_123" {
		t.Fatalf("account_id = %#v, want %q", got, "acct_123")
	}
	if got := normalized["plan_type"]; got != "plus" {
		t.Fatalf("plan_type = %#v, want %q", got, "plus")
	}
	if got := normalized["subscription_expires_at"]; got != "2026-07-01T00:00:00Z" {
		t.Fatalf("subscription_expires_at = %#v, want %q", got, "2026-07-01T00:00:00Z")
	}
	if _, ok := normalized["sessionToken"]; ok {
		t.Fatal("sessionToken should not be preserved in normalized auth metadata")
	}
}

func TestNormalizeImportedAuthMetadata_IgnoresLookalikeWithoutOpenAIProvider(t *testing.T) {
	metadata := map[string]any{
		"accessToken": "access-token",
		"user": map[string]any{
			"email": "codex@example.com",
		},
		"account": map[string]any{
			"id": "acct_123",
		},
	}

	normalized, changed := NormalizeImportedAuthMetadata(metadata)
	if changed {
		t.Fatal("expected lookalike metadata without authProvider=openai to remain unchanged")
	}
	if !reflect.DeepEqual(normalized, metadata) {
		t.Fatalf("normalized = %#v, want %#v", normalized, metadata)
	}
}

func TestNormalizeImportedAuthMetadata_ConvertsKiroCLIToken(t *testing.T) {
	metadata := map[string]any{
		"accessToken":  "access-token",
		"refreshToken": "refresh-token",
		"provider":     "google",
		"profileArn":   "arn:aws:codewhisperer:us-east-1:123:profile/test",
		"clientId":     "client-id",
		"clientSecret": "client-secret",
		"expiresAt":    "2026-05-09T00:00:00Z",
	}

	normalized, changed := NormalizeImportedAuthMetadata(metadata)
	if !changed {
		t.Fatal("expected Kiro token to be normalized")
	}
	if got := normalized["type"]; got != "kiro" {
		t.Fatalf("type = %#v, want %q", got, "kiro")
	}
	if got := normalized["provider"]; got != "google" {
		t.Fatalf("provider = %#v, want google", got)
	}
	if got := normalized["auth_method"]; got != "kiro-cli-social" {
		t.Fatalf("auth_method = %#v, want kiro-cli-social", got)
	}
	for key, want := range map[string]string{
		"access_token":  "access-token",
		"refresh_token": "refresh-token",
		"profile_arn":   "arn:aws:codewhisperer:us-east-1:123:profile/test",
		"client_id":     "client-id",
		"client_secret": "client-secret",
		"expires_at":    "2026-05-09T00:00:00Z",
	} {
		if got := normalized[key]; got != want {
			t.Fatalf("%s = %#v, want %q", key, got, want)
		}
	}
}

func fakeJWT(t *testing.T, payload map[string]any) string {
	t.Helper()

	headerRaw, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	return base64.RawURLEncoding.EncodeToString(headerRaw) + "." +
		base64.RawURLEncoding.EncodeToString(payloadRaw) + ".signature"
}
