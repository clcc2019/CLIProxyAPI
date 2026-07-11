package executor

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestMapCodexWebsocketReadErrorMessageTooBig(t *testing.T) {
	upstreamErr := &websocket.CloseError{Code: websocket.CloseMessageTooBig, Text: "frame exceeds read limit"}

	mapped := mapCodexWebsocketReadError(upstreamErr)

	statusProvider, ok := mapped.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("mapped error %T does not expose StatusCode()", mapped)
	}
	if got := statusProvider.StatusCode(); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}
	if got := gjson.Get(mapped.Error(), "error.code").String(); got != "message_too_big" {
		t.Fatalf("error.code = %q, want message_too_big; error=%s", got, mapped)
	}
}

func TestMapCodexWebsocketReadErrorPreservesOtherErrors(t *testing.T) {
	upstreamErr := errors.New("connection reset")

	mapped := mapCodexWebsocketReadError(upstreamErr)

	if !errors.Is(mapped, upstreamErr) {
		t.Fatalf("mapped error = %v, want original %v", mapped, upstreamErr)
	}
}
