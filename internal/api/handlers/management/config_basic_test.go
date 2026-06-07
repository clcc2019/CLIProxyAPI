package management

import "testing"

func TestNormalizeRoutingStrategy(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		want     string
		wantOK   bool
	}{
		{name: "empty defaults", strategy: " ", want: "round-robin", wantOK: true},
		{name: "round robin canonical", strategy: " Round-Robin ", want: "round-robin", wantOK: true},
		{name: "round robin compact", strategy: "roundrobin", want: "round-robin", wantOK: true},
		{name: "round robin short", strategy: "RR", want: "round-robin", wantOK: true},
		{name: "fill first canonical", strategy: "\tFill-First\r\n", want: "fill-first", wantOK: true},
		{name: "fill first compact", strategy: "fillfirst", want: "fill-first", wantOK: true},
		{name: "fill first short", strategy: "FF", want: "fill-first", wantOK: true},
		{name: "invalid", strategy: "least-latency", want: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeRoutingStrategy(tt.strategy)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("normalizeRoutingStrategy(%q) = %q, %t; want %q, %t", tt.strategy, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func BenchmarkNormalizeRoutingStrategy(b *testing.B) {
	for b.Loop() {
		if got, ok := normalizeRoutingStrategy(" Fill-First "); !ok || got != "fill-first" {
			b.Fatalf("normalizeRoutingStrategy() = %q, %t", got, ok)
		}
	}
}
