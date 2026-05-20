package executor

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexFinalUpstreamBodyMemoSkipsOversizedEntry(t *testing.T) {
	memo := &codexFinalUpstreamBodyMemo{}
	opts := codexFinalUpstreamBodyOptions{requestKind: codexFinalUpstreamResponses, streamMode: codexStreamFieldTrue}
	input := bytes.Repeat([]byte("x"), codexFinalUpstreamBodyMemoMaxItem+1)

	memo.set("gpt-5", opts, input, []byte(`{"ok":true}`))

	if got := memo.get("gpt-5", opts, input); got != nil {
		t.Fatalf("oversized memo entry was cached: %d bytes", len(got))
	}
	if memo.bytes != 0 || len(memo.entries) != 0 || memo.orderLen() != 0 {
		t.Fatalf("memo retained oversized entry: bytes=%d entries=%d order=%d", memo.bytes, len(memo.entries), memo.orderLen())
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
	if memo.bytes != 0 || len(memo.entries) != 0 || memo.orderLen() != 0 {
		t.Fatalf("memo retained oversized payload: bytes=%d entries=%d order=%d", memo.bytes, len(memo.entries), memo.orderLen())
	}
}

// TestCodexFinalUpstreamBodyMemoIncrementalEviction verifies that adding entries
// past the byte budget evicts the oldest entries rather than clearing the whole
// cache. Without incremental eviction, a single overflow wiped every cached
// normalization, degrading hit rate under sustained load.
func TestCodexFinalUpstreamBodyMemoIncrementalEviction(t *testing.T) {
	memo := &codexFinalUpstreamBodyMemo{}
	opts := codexFinalUpstreamBodyOptions{requestKind: codexFinalUpstreamResponses, streamMode: codexStreamFieldTrue}

	// Use per-entry size close to (but below) the per-item cap so that the byte
	// budget runs out before the entry count does.
	itemSize := codexFinalUpstreamBodyMemoMaxItem - 64
	payload := []byte(`{"ok":true}`)
	// Enough iterations to overflow the byte budget several times over.
	totalEntries := (codexFinalUpstreamBodyMemoMaxBytes / itemSize) + 4

	var lastInput []byte
	var firstInput []byte
	for i := 0; i < totalEntries; i++ {
		in := bytes.Repeat([]byte{byte('a' + (i % 26))}, itemSize-len(payload))
		// Append the index so each input is unique even though the fill byte
		// repeats every 26 iterations.
		in = append(in, byte(i>>8), byte(i&0xff))
		if i == 0 {
			firstInput = bytes.Clone(in)
		}
		if i == totalEntries-1 {
			lastInput = bytes.Clone(in)
		}
		memo.set("gpt-5", opts, in, payload)
	}

	if got := memo.get("gpt-5", opts, lastInput); got == nil {
		t.Fatal("most recent entry was evicted — incremental eviction broken")
	}
	if got := memo.get("gpt-5", opts, firstInput); got != nil {
		t.Fatal("expected the oldest entry to be evicted once the byte budget was exceeded")
	}
	if memo.orderLen() == 0 {
		t.Fatal("all entries were cleared; expected incremental eviction to keep the most recent ones")
	}
	if memo.bytes > codexFinalUpstreamBodyMemoMaxBytes {
		t.Fatalf("memo bytes=%d exceeded budget=%d", memo.bytes, codexFinalUpstreamBodyMemoMaxBytes)
	}
}
