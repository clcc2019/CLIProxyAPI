package executor

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestCodexFinalUpstreamBodyMemoSkipsOversizedEntry(t *testing.T) {
	memo := &codexFinalUpstreamBodyMemo{}
	opts := codexFinalUpstreamBodyOptions{requestKind: codexFinalUpstreamResponses, streamMode: codexStreamFieldTrue}
	input := bytes.Repeat([]byte("x"), codexFinalUpstreamBodyMemoMaxItem+1)

	memo.set("gpt-5", opts, input, []byte(`{"ok":true}`))

	if got := memo.get("gpt-5", opts, input); got != nil {
		t.Fatalf("oversized memo entry was cached: %d bytes", len(got))
	}
	if memo.bytes != 0 || len(memo.entries) != 0 || len(memo.order) != 0 {
		t.Fatalf("memo retained oversized entry: bytes=%d entries=%d order=%d", memo.bytes, len(memo.entries), len(memo.order))
	}
}

func TestCodexPromptResolutionMemoSkipsOversizedPayload(t *testing.T) {
	memo := &codexPromptResolutionMemo{}
	payload := bytes.Repeat([]byte("x"), codexPromptResolutionMemoMaxPayload+1)
	resolution := codexPromptCacheResolution{cache: helps.CodexCache{ID: "cache-id"}}

	memo.set(sdktranslator.FormatOpenAI, "gpt-5", "scope", "session", payload, resolution)

	if _, ok := memo.get(sdktranslator.FormatOpenAI, "gpt-5", "scope", "session", payload); ok {
		t.Fatal("oversized prompt resolution payload was cached")
	}
	if memo.bytes != 0 || len(memo.entries) != 0 || len(memo.order) != 0 {
		t.Fatalf("memo retained oversized payload: bytes=%d entries=%d order=%d", memo.bytes, len(memo.entries), len(memo.order))
	}
}
