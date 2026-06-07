package openai

import "testing"

func TestNormalizeCodexClientReasoningLevel(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "mixed case", raw: " Medium ", want: "medium"},
		{name: "xhigh", raw: "XHIGH", want: "xhigh"},
		{name: "none", raw: "none", want: "none"},
		{name: "unknown", raw: "extreme", want: ""},
		{name: "empty", raw: " ", want: ""},
	}

	for i := range tests {
		if got := normalizeCodexClientReasoningLevel(tests[i].raw); got != tests[i].want {
			t.Fatalf("%s: got %q, want %q", tests[i].name, got, tests[i].want)
		}
	}
}

func BenchmarkNormalizeCodexClientReasoningLevel(b *testing.B) {
	for b.Loop() {
		if got := normalizeCodexClientReasoningLevel(" Medium "); got != "medium" {
			b.Fatalf("normalizeCodexClientReasoningLevel() = %q", got)
		}
	}
}
