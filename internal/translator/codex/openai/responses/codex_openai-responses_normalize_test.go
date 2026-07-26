package responses

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertSystemRoleToDeveloper(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "converts every system role",
			input: `{"input":[{"role":"system","content":"a"},{"role":"user","content":"b"},{"role":"system","content":"c"}]}`,
			want:  `{"input":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
		},
		{
			name:  "leaves non-system roles untouched",
			input: `{"input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`,
			want:  `{"input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`,
		},
		{
			name:  "tolerates items without a role",
			input: `{"input":[{"type":"message"},{"role":"system","content":[{"type":"input_text","text":"hi"}]}]}`,
			want:  `{"input":[{"type":"message"},{"role":"developer","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
		{
			name:  "empty array is a no-op",
			input: `{"input":[]}`,
			want:  `{"input":[]}`,
		},
		{
			name:  "non-array input is a no-op",
			input: `{"input":"notanarray"}`,
			want:  `{"input":"notanarray"}`,
		},
		{
			name:  "missing input is a no-op",
			input: `{"model":"gpt-5"}`,
			want:  `{"model":"gpt-5"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(convertSystemRoleToDeveloper([]byte(tc.input)))
			if got != tc.want {
				t.Errorf("convertSystemRoleToDeveloper()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// Indices are resolved against the array parsed once up front, so a rewrite of
// an early element must not shift the target of a later one.
func TestConvertSystemRoleToDeveloperIndicesStayAlignedAfterRewrite(t *testing.T) {
	// The first rewrite grows the payload ("system" -> "developer"); the second
	// system item must still be located correctly afterwards.
	input := `{"input":[{"role":"system","content":"first"},{"role":"user","content":"middle"},{"role":"system","content":"last"}]}`
	got := convertSystemRoleToDeveloper([]byte(input))

	roles := gjson.GetBytes(got, "input.#.role").Array()
	want := []string{"developer", "user", "developer"}
	if len(roles) != len(want) {
		t.Fatalf("got %d roles, want %d: %s", len(roles), len(want), got)
	}
	for i, role := range roles {
		if role.String() != want[i] {
			t.Errorf("input.%d.role = %q, want %q", i, role.String(), want[i])
		}
	}
	if content := gjson.GetBytes(got, "input.2.content").String(); content != "last" {
		t.Errorf("input.2.content = %q, want %q (content misaligned)", content, "last")
	}
}

func TestNormalizeCodexBuiltinToolTypesAt(t *testing.T) {
	// web_search_preview is a legacy alias the upstream no longer accepts.
	input := `{"tools":[{"type":"web_search_preview"},{"type":"function","name":"keep"}]}`
	got := string(normalizeCodexBuiltinToolTypesAt([]byte(input), "tools"))

	if gjson.Get(got, "tools.0.type").String() != "web_search" {
		t.Errorf("tools.0.type not normalized: %s", got)
	}
	if gjson.Get(got, "tools.1.type").String() != "function" {
		t.Errorf("tools.1.type should be untouched: %s", got)
	}
	if gjson.Get(got, "tools.1.name").String() != "keep" {
		t.Errorf("tools.1.name lost: %s", got)
	}

	for _, path := range []string{"tools", "missing", "tool_choice.tools"} {
		noop := `{"tools":"notanarray"}`
		if got := string(normalizeCodexBuiltinToolTypesAt([]byte(noop), path)); got != noop {
			t.Errorf("normalizeCodexBuiltinToolTypesAt(%q) mutated a non-array: %s", path, got)
		}
	}
}

func benchInput(items int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5","input":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"message","role":"user","content":[{"type":"input_text","text":"%s"}]}`,
			strings.Repeat("lorem ipsum dolor ", 30))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkConvertSystemRoleToDeveloper(b *testing.B) {
	for _, items := range []int{50, 150, 300} {
		body := benchInput(items)
		b.Run(fmt.Sprintf("items=%d", items), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = convertSystemRoleToDeveloper(body)
			}
		})
	}
}
