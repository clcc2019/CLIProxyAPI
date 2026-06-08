package auth

import "testing"

func TestCredentialFileNameUsesEmail(t *testing.T) {
	got := CredentialFileName(" user+team@example.com ", "fallback-subject")
	want := "user+team@example.com.json"
	if got != want {
		t.Fatalf("CredentialFileName() = %q, want %q", got, want)
	}
}

func TestCredentialFileNameUsesFallbackWhenEmailMissing(t *testing.T) {
	got := CredentialFileName("", "subject-123")
	want := "subject-123.json"
	if got != want {
		t.Fatalf("CredentialFileName() = %q, want %q", got, want)
	}
}

func TestCredentialFileNameSanitizesUnsafePathSegments(t *testing.T) {
	got := CredentialFileName("../user@example.com")
	want := "user@example.com.json"
	if got != want {
		t.Fatalf("CredentialFileName() = %q, want %q", got, want)
	}
}
