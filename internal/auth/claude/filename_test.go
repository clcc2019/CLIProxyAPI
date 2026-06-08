package claude

import "testing"

func TestCredentialFileNameUsesEmailWithoutProviderPrefix(t *testing.T) {
	got := CredentialFileName("user@example.com")
	want := "user@example.com.json"
	if got != want {
		t.Fatalf("CredentialFileName() = %q, want %q", got, want)
	}
}
