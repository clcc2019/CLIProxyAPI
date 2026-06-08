package codex

import "testing"

func TestCredentialFileNameUsesOnlyEmail(t *testing.T) {
	got := CredentialFileName("user@example.com", "team", "hash-account-id", true)
	want := "user@example.com.json"
	if got != want {
		t.Fatalf("CredentialFileName() = %q, want %q", got, want)
	}
}
