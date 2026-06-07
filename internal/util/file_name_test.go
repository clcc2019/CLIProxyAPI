package util

import "testing"

func TestHasJSONFileName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "auth.json", want: true},
		{name: "auth.JSON", want: true},
		{name: "/tmp/auth.JsOn", want: true},
		{name: ".json", want: true},
		{name: "auth.json.bak", want: false},
		{name: "json", want: false},
		{name: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasJSONFileName(tt.name)
			if got != tt.want {
				t.Fatalf("HasJSONFileName(%q) = %t, want %t", tt.name, got, tt.want)
			}
		})
	}
}

func BenchmarkHasJSONFileName(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if !HasJSONFileName("/tmp/auth-file.JSON") {
			b.Fatal("expected JSON filename")
		}
	}
}
