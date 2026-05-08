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
	t.Run("trims data prefix", func(t *testing.T) {
		got := PassthroughStreamPayload(context.Background(), "", nil, nil, []byte("data: {\"ok\":true}\n"), nil)
		if len(got) != 1 || string(got[0]) != `{"ok":true}` {
			t.Fatalf("unexpected payload: %#v", got)
		}
	})

	t.Run("drops done marker", func(t *testing.T) {
		got := PassthroughStreamPayload(context.Background(), "", nil, nil, []byte("data: [DONE]"), nil)
		if len(got) != 0 {
			t.Fatalf("expected done marker to be dropped, got %#v", got)
		}
	})

	t.Run("passes raw payload", func(t *testing.T) {
		got := PassthroughStreamPayload(context.Background(), "", nil, nil, []byte(`{"ok":true}`), nil)
		if len(got) != 1 || string(got[0]) != `{"ok":true}` {
			t.Fatalf("unexpected payload: %#v", got)
		}
	})
}

func TestPassthroughNonStreamPayload(t *testing.T) {
	raw := []byte(`{"ok":true}`)
	got := PassthroughNonStreamPayload(context.Background(), "", nil, nil, raw, nil)
	if string(got) != string(raw) {
		t.Fatalf("unexpected payload: %s", got)
	}
}
