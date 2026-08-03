package management

import (
	"encoding/json"
	"testing"
)

func TestValueAsString_JSONNumber(t *testing.T) {
	if got := valueAsString(json.Number("42.5")); got != "42.5" {
		t.Fatalf("valueAsString(json.Number) = %q, want %q", got, "42.5")
	}
}
