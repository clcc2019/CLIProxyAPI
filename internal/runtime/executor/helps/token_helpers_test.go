package helps

import "testing"

func TestTokenizerForModelAcceptsMixedCaseFamilies(t *testing.T) {
	tests := []string{
		" GPT-5.1-CODEX ",
		"GPT-4.1-MINI",
		"gpt-4o-mini",
		"O3-mini",
		"unknown-model",
		"",
	}

	for i := range tests {
		enc, err := TokenizerForModel(tests[i])
		if err != nil {
			t.Fatalf("TokenizerForModel(%q) error = %v", tests[i], err)
		}
		if enc == nil {
			t.Fatalf("TokenizerForModel(%q) returned nil encoder", tests[i])
		}
	}
}

func TestHasOpenAITokenizerModelPrefix(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		prefix string
		want   bool
	}{
		{name: "mixed case", model: "GPT-4.1-MINI", prefix: "gpt-4.1", want: true},
		{name: "exact", model: "gpt-5", prefix: "gpt-5", want: true},
		{name: "short", model: "gpt", prefix: "gpt-4", want: false},
		{name: "different", model: "claude-sonnet", prefix: "gpt-4", want: false},
	}

	for i := range tests {
		if got := hasOpenAITokenizerModelPrefix(tests[i].model, tests[i].prefix); got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func BenchmarkHasOpenAITokenizerModelPrefix(b *testing.B) {
	for b.Loop() {
		if !hasOpenAITokenizerModelPrefix("GPT-4.1-MINI", "gpt-4.1") {
			b.Fatal("expected tokenizer model prefix")
		}
	}
}
