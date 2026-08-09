package executor

import (
	"bytes"
	"testing"
)

func TestNormalizeCodexInstructions(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "missing", body: `{"model":"gpt-5"}`, want: `{"model":"gpt-5","instructions":""}`},
		{name: "missing empty object", body: `{}`, want: `{"instructions":""}`},
		{name: "missing trims surrounding whitespace", body: " \n{\"model\":\"gpt-5\"}\t", want: `{"model":"gpt-5","instructions":""}`},
		{name: "null", body: `{"instructions":null,"model":"gpt-5"}`, want: `{"instructions":"","model":"gpt-5"}`},
		{name: "empty string", body: `{"instructions":"","model":"gpt-5"}`, want: `{"instructions":"","model":"gpt-5"}`},
		{name: "existing", body: `{"instructions":"help","model":"gpt-5"}`, want: `{"instructions":"help","model":"gpt-5"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(tt.body)
			before := append([]byte(nil), input...)
			got := normalizeCodexInstructions(input)
			if string(got) != tt.want {
				t.Fatalf("normalizeCodexInstructions() = %s, want %s", got, tt.want)
			}
			if !bytes.Equal(input, before) {
				t.Fatalf("normalizeCodexInstructions mutated input: got %s, want %s", input, before)
			}
		})
	}
}

func BenchmarkNormalizeCodexInstructionsMissing(b *testing.B) {
	body := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"hello"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		result := normalizeCodexInstructions(body)
		if len(result) == 0 {
			b.Fatal("empty result")
		}
	}
}
