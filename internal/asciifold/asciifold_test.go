package asciifold

import "testing"

func TestContains(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		needle string
		want   bool
	}{
		{name: "empty needle", value: "abc", needle: "", want: true},
		{name: "mixed case", value: "Text/Event-Stream; charset=utf-8", needle: "text/event-stream", want: true},
		{name: "missing", value: "/v1/images", needle: "/images/edits", want: false},
		{name: "needle longer", value: "abc", needle: "abcd", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Contains(tt.value, tt.needle); got != tt.want {
				t.Fatalf("Contains(%q, %q) = %v, want %v", tt.value, tt.needle, got, tt.want)
			}
			if got := ContainsBytes([]byte(tt.value), tt.needle); got != tt.want {
				t.Fatalf("ContainsBytes(%q, %q) = %v, want %v", tt.value, tt.needle, got, tt.want)
			}
		})
	}
}

func TestIndex(t *testing.T) {
	if got := Index("TRY AGAIN AT May 1st", "try again at "); got != 0 {
		t.Fatalf("Index() = %d, want 0", got)
	}
	if got := Index("prefix TRY AGAIN AT May 1st", "try again at "); got != len("prefix ") {
		t.Fatalf("Index() = %d, want %d", got, len("prefix "))
	}
	if got := IndexBytes([]byte("prefix <TITLE>ok</title>"), "<title"); got != len("prefix ") {
		t.Fatalf("IndexBytes() = %d, want %d", got, len("prefix "))
	}
	if got := Index("abc", "missing"); got != -1 {
		t.Fatalf("Index() = %d, want -1", got)
	}
}

func TestPrefixSuffixEqual(t *testing.T) {
	if !HasPrefix("/v1/Images/Edits", "/v1/images") {
		t.Fatal("expected mixed-case prefix")
	}
	if !HasPrefixBytes([]byte("<!DOCTYPE html>"), "<!doctype html") {
		t.Fatal("expected mixed-case byte prefix")
	}
	if !HasSuffix("HTTP2: Stream Closed", "stream closed") {
		t.Fatal("expected mixed-case suffix")
	}
	if !Equal("Set-Cookie", "set-cookie") {
		t.Fatal("expected mixed-case equality")
	}
	if HasSuffix("abc", "zabc") {
		t.Fatal("unexpected suffix match")
	}
}
