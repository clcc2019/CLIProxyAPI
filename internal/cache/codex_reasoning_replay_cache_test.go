package cache

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"testing"
	"time"
)

func validCodexReasoningReplayEncryptedContentForTest(seed byte) string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = seed + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func TestCodexReasoningReplayCacheRejectsInvalidItems(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)

	if CacheCodexReasoningReplayItem("gpt-5.4", "session", []byte(`{"type":"reasoning","encrypted_content":"bad","summary":[]}`)) {
		t.Fatal("invalid encrypted_content should not be cached")
	}
	if _, ok := GetCodexReasoningReplayItem("gpt-5.4", "session"); ok {
		t.Fatal("invalid item was cached")
	}
}

func TestCodexReasoningReplayCacheScopesByModelAndSession(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)

	encryptedContent := validCodexReasoningReplayEncryptedContentForTest(7)
	if !CacheCodexReasoningReplayItem("gpt-5.4", "session-a", []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+encryptedContent+`"}`)) {
		t.Fatal("valid item was not cached")
	}

	if _, ok := GetCodexReasoningReplayItem("gpt-5.5", "session-a"); ok {
		t.Fatal("cache should not hit across models")
	}
	if _, ok := GetCodexReasoningReplayItem("gpt-5.4", "session-b"); ok {
		t.Fatal("cache should not hit across sessions")
	}

	item, ok := GetCodexReasoningReplayItem("gpt-5.4", "session-a")
	if !ok {
		t.Fatal("cache miss for original model and session")
	}
	if string(item) != `{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+encryptedContent+`"}` {
		t.Fatalf("normalized item = %s", string(item))
	}
}

func TestCodexReasoningReplayCacheBatchEvictsWhenFull(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)

	encryptedContent := validCodexReasoningReplayEncryptedContentForTest(9)
	item := []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + encryptedContent + `"}`)
	for i := 0; i <= CodexReasoningReplayCacheMaxEntries; i++ {
		if !CacheCodexReasoningReplayItem("gpt-5.4", fmt.Sprintf("session-%d", i), item) {
			t.Fatalf("cache insert %d failed", i)
		}
	}

	codexReasoningReplayMu.Lock()
	gotLen := len(codexReasoningReplayEntries)
	codexReasoningReplayMu.Unlock()
	if gotLen >= CodexReasoningReplayCacheMaxEntries {
		t.Fatalf("cache entries = %d, want batch eviction below max %d", gotLen, CodexReasoningReplayCacheMaxEntries)
	}
}

// Eviction must remove exactly the N oldest entries by Timestamp — the heap
// rewrite is only correct if it picks the same victims a full sort would.
func TestEvictOldestCodexReasoningReplayEntriesRemovesOldest(t *testing.T) {
	codexReasoningReplayMu.Lock()
	defer codexReasoningReplayMu.Unlock()
	codexReasoningReplayEntries = make(map[string]codexReasoningReplayEntry)

	base := time.Now()
	const total = 50
	for i := 0; i < total; i++ {
		codexReasoningReplayEntries[strconv.Itoa(i)] = codexReasoningReplayEntry{
			Items:     [][]byte{[]byte(`{"type":"reasoning"}`)},
			Timestamp: base.Add(time.Duration(i) * time.Second),
		}
	}

	const evict = 20
	evictOldestCodexReasoningReplayEntries(evict)

	if got := len(codexReasoningReplayEntries); got != total-evict {
		t.Fatalf("entry count = %d, want %d", got, total-evict)
	}
	// Keys 0..evict-1 are the oldest and must be gone; evict..total-1 must stay.
	for i := 0; i < total; i++ {
		_, present := codexReasoningReplayEntries[strconv.Itoa(i)]
		if wantPresent := i >= evict; present != wantPresent {
			t.Errorf("key %d present=%v, want %v", i, present, wantPresent)
		}
	}
}

func TestEvictOldestCodexReasoningReplayEntriesBounds(t *testing.T) {
	codexReasoningReplayMu.Lock()
	defer codexReasoningReplayMu.Unlock()

	// Evicting more than the cache holds must drain it, not panic.
	codexReasoningReplayEntries = map[string]codexReasoningReplayEntry{
		"a": {Timestamp: time.Now()},
		"b": {Timestamp: time.Now()},
	}
	evictOldestCodexReasoningReplayEntries(10)
	if got := len(codexReasoningReplayEntries); got != 0 {
		t.Errorf("over-eviction left %d entries, want 0", got)
	}

	// Non-positive counts and an empty cache are no-ops.
	codexReasoningReplayEntries = map[string]codexReasoningReplayEntry{"a": {Timestamp: time.Now()}}
	evictOldestCodexReasoningReplayEntries(0)
	evictOldestCodexReasoningReplayEntries(-1)
	if got := len(codexReasoningReplayEntries); got != 1 {
		t.Errorf("no-op eviction changed cache to %d entries, want 1", got)
	}
	codexReasoningReplayEntries = make(map[string]codexReasoningReplayEntry)
	evictOldestCodexReasoningReplayEntries(5)
}

func BenchmarkEvictOldestCodexReasoningReplayEntries(b *testing.B) {
	base := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		codexReasoningReplayEntries = make(map[string]codexReasoningReplayEntry, CodexReasoningReplayCacheMaxEntries)
		for j := 0; j <= CodexReasoningReplayCacheMaxEntries; j++ {
			codexReasoningReplayEntries[strconv.Itoa(j)] = codexReasoningReplayEntry{
				Timestamp: base.Add(time.Duration(j) * time.Millisecond),
			}
		}
		b.StartTimer()
		evictOldestCodexReasoningReplayEntries(CodexReasoningReplayCacheEvictBatchSize)
	}
}
