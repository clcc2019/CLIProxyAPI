package credentialweight

import "testing"

func TestParseValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  int64
		ok    bool
	}{
		{name: "integer", value: 5, want: 5, ok: true},
		{name: "numeric string", value: " 3 ", want: 3, ok: true},
		{name: "negative normalizes", value: -1, want: 0, ok: true},
		{name: "fraction rejected", value: 1.5, ok: false},
		{name: "too large rejected", value: Max + 1, ok: false},
		{name: "non numeric rejected", value: true, ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseValue(tc.value)
			if tc.ok {
				if err != nil || got != tc.want {
					t.Fatalf("ParseValue(%v) = %d, %v; want %d, nil", tc.value, got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseValue(%v) error = nil, want error", tc.value)
			}
		})
	}
}
