package util

import (
	"encoding/json"
	"testing"
)

func TestAppendJSONString(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain", value: "plain", want: `"plain"`},
		{name: "quote and slash", value: "a\"b\\c", want: `"a\"b\\c"`},
		{name: "controls", value: "\x00\x1f\n\r\t\b\f", want: `"\u0000\u001f\n\r\t\b\f"`},
		{name: "unicode", value: "你好 🌍", want: `"你好 🌍"`},
		{name: "invalid UTF-8", value: string([]byte{'a', 0xff, 'b'}), want: `"a\ufffdb"`},
		{name: "html remains literal", value: "<tag>&", want: `"<tag>&"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := AppendJSONString([]byte("prefix:"), test.value)
			if string(got) != "prefix:"+test.want {
				t.Fatalf("AppendJSONString() = %q, want %q", got, "prefix:"+test.want)
			}
			if !json.Valid(got[len("prefix:"):]) {
				t.Fatalf("AppendJSONString() emitted invalid JSON: %q", got)
			}
		})
	}
}
