package common

import (
	"bytes"
	"context"
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

func TestPassthroughStreamPayload(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "trims data prefix",
			raw:  "data: {\"ok\":true}\n",
			want: []string{`{"ok":true}`},
		},
		{
			name: "drops done marker",
			raw:  "data: [DONE]",
			want: nil,
		},
		{
			name: "passes raw payload",
			raw:  `{"ok":true}`,
			want: []string{`{"ok":true}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PassthroughStreamPayload(context.Background(), "", nil, nil, []byte(tt.raw), nil)
			assertByteSlices(t, got, tt.want)
		})
	}
}

func TestPassthroughNonStreamPayload(t *testing.T) {
	raw := []byte(`{"ok":true}`)
	got := PassthroughNonStreamPayload(context.Background(), "", nil, nil, raw, nil)
	if string(got) != string(raw) {
		t.Fatalf("unexpected payload: %s", got)
	}
}

func assertByteSlices(t *testing.T, got [][]byte, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d payloads, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("payload %d = %q, want %q", i, got[i], want[i])
		}
	}
}
