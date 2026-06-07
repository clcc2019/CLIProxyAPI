package util

import "testing"

func TestIsWebSearchTool(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		toolType string
		want     bool
	}{
		{name: "name exact", toolName: "web_search", want: true},
		{name: "name mixed case with spaces", toolName: " Web_Search ", want: true},
		{name: "type exact", toolType: "web_search_20250305", want: true},
		{name: "type mixed case with spaces", toolType: " Web_Search_20260209 ", want: true},
		{name: "type preview prefix", toolType: "web_search_preview_2025_03_11", want: true},
		{name: "short type", toolType: "web", want: false},
		{name: "different tool", toolName: "browser_search", toolType: "computer_use", want: false},
	}

	for i := range tests {
		if got := IsWebSearchTool(tests[i].toolName, tests[i].toolType); got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func TestHasPrefixEqualFold(t *testing.T) {
	if !hasPrefixEqualFold("Web_Search_20250305", "web_search") {
		t.Fatal("expected mixed-case prefix to match")
	}
	if hasPrefixEqualFold("web", "web_search") {
		t.Fatal("expected short value not to match")
	}
}

func BenchmarkIsWebSearchTool(b *testing.B) {
	for b.Loop() {
		if !IsWebSearchTool("", " Web_Search_20250305 ") {
			b.Fatal("expected web search tool")
		}
	}
}
