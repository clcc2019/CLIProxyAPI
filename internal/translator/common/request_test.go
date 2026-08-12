package common

import "testing"

func TestRequestModelName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		original   string
		translated string
		want       string
	}{
		{name: "original wins", original: `{"model":"client-alias"}`, translated: `{"model":"internal-model"}`, want: "client-alias"},
		{name: "translated top level", original: `{"input":"hello"}`, translated: `{"model":"internal-model"}`, want: "internal-model"},
		{name: "translated nested request", original: `{`, translated: `{"request":{"model":"nested-model"}}`, want: "nested-model"},
		{name: "blank model is skipped", original: `{"model":"   "}`, translated: `{"model":"fallback-model"}`, want: "fallback-model"},
		{name: "non string model is skipped", original: `{"model":123}`, translated: `{"request":{"model":"nested-model"}}`, want: "nested-model"},
		{name: "missing", original: `{"input":"hello"}`, translated: `{"stream":true}`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequestModelName([]byte(tt.original), []byte(tt.translated)); got != tt.want {
				t.Fatalf("RequestModelName() = %q, want %q", got, tt.want)
			}
		})
	}
}
