package openai

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Compare the optimized parser against the compatibility path for valid JSON.
func FuzzWebsocketPayloadEventTypeValue(f *testing.F) {
	for _, payload := range []string{
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":" response.create "}`,
		`{"ty\u0070e":"response.create","type":"response.append"}`,
		`{"type":"response.output_text.\u0064elta"}`,
		`{"item":{"type":"ignored"},"type":"response.completed"}`,
		`{"type":null}`,
		`{"type":42}`,
		`{"type":{"nested":true}}`,
		`[]`,
	} {
		f.Add([]byte(payload))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if !gjson.ValidBytes(payload) {
			return
		}
		want := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
		if got := websocketPayloadEventTypeValue(payload); got != want {
			t.Fatalf("type = %q, want %q; payload=%s", got, want, payload)
		}
	})
}
