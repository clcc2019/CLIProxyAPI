package config

import "testing"

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
