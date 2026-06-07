package helps

import "testing"

func TestShouldCloak(t *testing.T) {
	tests := []struct {
		name      string
		cloakMode string
		userAgent string
		want      bool
	}{
		{name: "always mixed case", cloakMode: "AlWaYs", userAgent: "claude-cli/1.0", want: true},
		{name: "never mixed case", cloakMode: "NeVeR", userAgent: "curl/8.0", want: false},
		{name: "no trim preserves auto fallback", cloakMode: " NeVeR ", userAgent: "curl/8.0", want: true},
		{name: "auto claude cli", cloakMode: "auto", userAgent: "claude-cli/1.0", want: false},
		{name: "auto other client", cloakMode: "auto", userAgent: "curl/8.0", want: true},
		{name: "empty other client", cloakMode: "", userAgent: "curl/8.0", want: true},
	}

	for i := range tests {
		if got := ShouldCloak(tests[i].cloakMode, tests[i].userAgent); got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func BenchmarkShouldCloak(b *testing.B) {
	for b.Loop() {
		if !ShouldCloak("AlWaYs", "claude-cli/1.0") {
			b.Fatal("expected cloak")
		}
	}
}
