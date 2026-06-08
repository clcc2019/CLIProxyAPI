package xai

import "testing"

func TestCredentialFileNameUsesEmailWithoutProviderPrefix(t *testing.T) {
	got := CredentialFileName("user@example.com", "subject-123")
	want := "user@example.com.json"
	if got != want {
		t.Fatalf("CredentialFileName() = %q, want %q", got, want)
	}
}

func TestCredentialFileNameUsesSubjectWhenEmailMissing(t *testing.T) {
	got := CredentialFileName("", "subject-123")
	want := "subject-123.json"
	if got != want {
		t.Fatalf("CredentialFileName() = %q, want %q", got, want)
	}
}
