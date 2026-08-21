package common

import (
	"strings"
	"testing"
)

func TestShortenNameIfNeededUsesResponsesLimit(t *testing.T) {
	atLimit := strings.Repeat("a", 128)
	if got := ShortenNameIfNeeded(atLimit); got != atLimit {
		t.Fatalf("128-byte tool name changed: len=%d", len(got))
	}
	if got := ShortenNameIfNeeded(atLimit + "b"); len(got) != 128 {
		t.Fatalf("129-byte tool name shortened to %d bytes, want 128", len(got))
	}
}
