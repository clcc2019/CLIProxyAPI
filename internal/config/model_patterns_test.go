package config

import "testing"

func TestMatchModelPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		value   string
		want    bool
	}{
		{name: "exact match", pattern: "gpt-5", value: "GPT-5", want: true},
		{name: "prefix wildcard", pattern: "gpt-*", value: "gpt-5-preview", want: true},
		{name: "suffix wildcard", pattern: "*-mini", value: "gpt-5-mini", want: true},
		{name: "middle segments in order", pattern: "claude*sonnet*4-5", value: "claude-3-5-sonnet-4-5", want: true},
		{name: "middle segments out of order", pattern: "claude*4-5*sonnet", value: "claude-3-5-sonnet-4-5", want: false},
		{name: "consecutive wildcards", pattern: "gpt-**preview", value: "gpt-5-preview", want: true},
		{name: "thinking suffix ignored", pattern: "claude-sonnet-*", value: "claude-sonnet-4-5(high)", want: true},
		{name: "models prefix ignored", pattern: "gpt-*", value: "models/gpt-5", want: true},
		{name: "empty pattern denied", pattern: " ", value: "gpt-5", want: false},
		{name: "empty value denied", pattern: "gpt-*", value: " ", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchModelPattern(tt.pattern, tt.value)
			if got != tt.want {
				t.Fatalf("MatchModelPattern(%q, %q) = %t, want %t", tt.pattern, tt.value, got, tt.want)
			}
		})
	}
}

func TestIsModelAllowed(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		allowed  []string
		excluded []string
		want     bool
	}{
		{
			name:     "allow all when lists empty",
			model:    "gpt-5",
			allowed:  nil,
			excluded: nil,
			want:     true,
		},
		{
			name:     "allowed list supports wildcard and case-insensitive match",
			model:    "GPT-5-Preview",
			allowed:  []string{"gpt-5-*"},
			excluded: nil,
			want:     true,
		},
		{
			name:     "excluded list wins over allow list",
			model:    "gpt-5-mini",
			allowed:  []string{"gpt-5-*"},
			excluded: []string{"*-mini"},
			want:     false,
		},
		{
			name:     "codex models prefix is canonicalized",
			model:    "models/gpt-5",
			allowed:  []string{"gpt-*"},
			excluded: nil,
			want:     true,
		},
		{
			name:     "thinking suffix is ignored",
			model:    "claude-sonnet-4-5(high)",
			allowed:  []string{"claude-sonnet-*"},
			excluded: nil,
			want:     true,
		},
		{
			name:     "non matching allow list denies",
			model:    "gpt-4o",
			allowed:  []string{"gpt-5-*"},
			excluded: nil,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsModelAllowed(tt.model, tt.allowed, tt.excluded)
			if got != tt.want {
				t.Fatalf("IsModelAllowed(%q, %v, %v) = %t, want %t", tt.model, tt.allowed, tt.excluded, got, tt.want)
			}
		})
	}
}

func BenchmarkMatchModelPatternWildcard(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if !MatchModelPattern("claude*sonnet*4-5", "models/claude-3-5-sonnet-4-5(high)") {
			b.Fatal("expected wildcard pattern to match")
		}
	}
}
