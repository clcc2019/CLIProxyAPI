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

func TestBuildShortNameMapRespectsLegacyLimitAndUniqueness(t *testing.T) {
	names := []string{strings.Repeat("x", 80), strings.Repeat("x", 79) + "y"}
	mapped := BuildShortNameMap(names)
	if mapped[names[0]] == mapped[names[1]] {
		t.Fatal("shortened tool names collided")
	}
	for _, name := range mapped {
		if len(name) > LegacyToolNameLimit {
			t.Fatalf("shortened tool name length = %d, want <= %d", len(name), LegacyToolNameLimit)
		}
	}
}
