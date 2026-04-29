package common

import (
	"bytes"
	"testing"
)

func TestForEachSSEDataLine(t *testing.T) {
	raw := []byte("event: message\r\n" +
		"data: {\"type\":\"message_start\"}\r\n" +
		": keepalive\n" +
		"data:   {\"type\":\"message_delta\"}  \n")

	var got [][]byte
	ForEachSSEDataLine(raw, func(data []byte) bool {
		got = append(got, bytes.Clone(data))
		return true
	})

	want := [][]byte{
		[]byte(`{"type":"message_start"}`),
		[]byte(`{"type":"message_delta"}`),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d data lines, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("data line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestForEachSSEDataLineStopsWhenCallbackReturnsFalse(t *testing.T) {
	raw := []byte("data: one\ndata: two\n")
	count := 0

	ForEachSSEDataLine(raw, func(data []byte) bool {
		count++
		return false
	})

	if count != 1 {
		t.Fatalf("callback count = %d, want 1", count)
	}
}
