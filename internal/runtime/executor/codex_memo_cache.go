package executor

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"hash/maphash"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

const (
	codexFinalUpstreamBodyMemoMaxEntries = 256
	codexFinalUpstreamBodyMemoMaxBytes   = 16 << 20
	codexFinalUpstreamBodyMemoMaxItem    = 1 << 20
	codexPromptResolutionMemoMaxEntries  = 512
	codexPromptResolutionMemoMaxBytes    = 8 << 20
	codexPromptResolutionMemoMaxPayload  = 256 << 10
)

var (
	codexMemoHashSeed                = maphash.MakeSeed()
	globalCodexFinalUpstreamBodyMemo codexFinalUpstreamBodyMemo
	globalCodexPromptResolutionMemo  codexPromptResolutionMemo
	globalCodexPromptResolutionGroup helps.InFlightGroup[codexPromptCacheResolution]
)

// memoList is a FIFO-ordered doubly-linked list of hash keys.
// It lets the memo evict oldest entries one at a time instead of clearing the
// entire cache when a byte budget is exceeded, which avoids cache thrashing
// under concurrent load with distinct-but-similar-sized inputs.
type memoList = list.List

// pushBackHash adds hash to the tail of l. Exposed as a helper so the generic
// code path can read consistently across the two memo types.
func pushBackHash(l *list.List, hash uint64) *list.Element {
	return l.PushBack(hash)
}

// removeElem removes e from l and returns its hash value.
func removeElem(l *list.List, e *list.Element) uint64 {
	if e == nil {
		return 0
	}
	hash, _ := l.Remove(e).(uint64)
	return hash
}

// popFrontHash removes the front element of l and returns its hash and true,
// or (0, false) if l is empty.
func popFrontHash(l *list.List) (uint64, bool) {
	front := l.Front()
	if front == nil {
		return 0, false
	}
	hash, _ := l.Remove(front).(uint64)
	return hash, true
}

type codexFinalUpstreamBodyMemoEntry struct {
	baseModel string
	opts      codexFinalUpstreamBodyOptions
	input     []byte
	output    []byte
	size      int
	elem      *list.Element
}

type codexFinalUpstreamBodyMemo struct {
	mu      sync.RWMutex
	entries map[uint64]*codexFinalUpstreamBodyMemoEntry
	order   *memoList
	bytes   int
}

func (m *codexFinalUpstreamBodyMemo) get(baseModel string, opts codexFinalUpstreamBodyOptions, input []byte) []byte {
	if m == nil || len(input) == 0 {
		return nil
	}
	hash := hashCodexFinalUpstreamBodyMemoKey(baseModel, opts, input)

	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.entries[hash]
	if !ok || entry.baseModel != baseModel || entry.opts != opts || !bytes.Equal(entry.input, input) {
		return nil
	}
	return bytes.Clone(entry.output)
}

func (m *codexFinalUpstreamBodyMemo) set(baseModel string, opts codexFinalUpstreamBodyOptions, input []byte, output []byte) {
	if m == nil || len(input) == 0 || len(output) == 0 {
		return
	}
	size := len(input) + len(output)
	if size > codexFinalUpstreamBodyMemoMaxItem || size > codexFinalUpstreamBodyMemoMaxBytes {
		return
	}
	hash := hashCodexFinalUpstreamBodyMemoKey(baseModel, opts, input)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[uint64]*codexFinalUpstreamBodyMemoEntry, codexFinalUpstreamBodyMemoMaxEntries)
		m.order = list.New()
	}
	if prev, ok := m.entries[hash]; ok {
		m.bytes -= prev.size
		removeElem(m.order, prev.elem)
		delete(m.entries, hash)
	}

	// Evict oldest entries incrementally until the new item fits both budgets.
	for (m.bytes+size > codexFinalUpstreamBodyMemoMaxBytes ||
		m.order.Len() >= codexFinalUpstreamBodyMemoMaxEntries) && m.order.Len() > 0 {
		oldHash, ok := popFrontHash(m.order)
		if !ok {
			break
		}
		if old, ok := m.entries[oldHash]; ok {
			m.bytes -= old.size
			delete(m.entries, oldHash)
		}
	}

	// If a single entry is smaller than the max but still cannot fit (pathological
	// case where bookkeeping diverged), drop it rather than inserting over-budget.
	if m.bytes+size > codexFinalUpstreamBodyMemoMaxBytes {
		return
	}

	entry := &codexFinalUpstreamBodyMemoEntry{
		baseModel: baseModel,
		opts:      opts,
		input:     bytes.Clone(input),
		output:    bytes.Clone(output),
		size:      size,
	}
	entry.elem = pushBackHash(m.order, hash)
	m.entries[hash] = entry
	m.bytes += size
}

type codexPromptResolutionMemoEntry struct {
	from               sdktranslator.Format
	model              string
	scope              string
	payload            []byte
	executionSessionID string
	resolution         codexPromptCacheResolution
	size               int
	elem               *list.Element
}

type codexPromptResolutionMemo struct {
	mu      sync.RWMutex
	entries map[uint64]*codexPromptResolutionMemoEntry
	order   *memoList
	bytes   int
}

func (m *codexPromptResolutionMemo) get(from sdktranslator.Format, model string, scope string, executionSessionID string, payload []byte) (codexPromptCacheResolution, bool) {
	if m == nil {
		return codexPromptCacheResolution{}, false
	}
	hash := hashCodexPromptResolutionMemoKey(from, model, scope, executionSessionID, payload)

	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.entries[hash]
	if !ok ||
		entry.from != from ||
		entry.model != model ||
		entry.scope != scope ||
		entry.executionSessionID != executionSessionID ||
		!bytes.Equal(entry.payload, payload) {
		return codexPromptCacheResolution{}, false
	}
	return entry.resolution, true
}

func (m *codexPromptResolutionMemo) set(from sdktranslator.Format, model string, scope string, executionSessionID string, payload []byte, resolution codexPromptCacheResolution) {
	if m == nil {
		return
	}
	size := len(payload)
	if size > codexPromptResolutionMemoMaxPayload || size > codexPromptResolutionMemoMaxBytes {
		return
	}
	hash := hashCodexPromptResolutionMemoKey(from, model, scope, executionSessionID, payload)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[uint64]*codexPromptResolutionMemoEntry, codexPromptResolutionMemoMaxEntries)
		m.order = list.New()
	}
	if prev, ok := m.entries[hash]; ok {
		m.bytes -= prev.size
		removeElem(m.order, prev.elem)
		delete(m.entries, hash)
	}

	for (m.bytes+size > codexPromptResolutionMemoMaxBytes ||
		m.order.Len() >= codexPromptResolutionMemoMaxEntries) && m.order.Len() > 0 {
		oldHash, ok := popFrontHash(m.order)
		if !ok {
			break
		}
		if old, ok := m.entries[oldHash]; ok {
			m.bytes -= old.size
			delete(m.entries, oldHash)
		}
	}

	if m.bytes+size > codexPromptResolutionMemoMaxBytes {
		return
	}

	entry := &codexPromptResolutionMemoEntry{
		from:               from,
		model:              model,
		scope:              scope,
		payload:            bytes.Clone(payload),
		executionSessionID: executionSessionID,
		resolution:         resolution,
		size:               size,
	}
	entry.elem = pushBackHash(m.order, hash)
	m.entries[hash] = entry
	m.bytes += size
}

// orderLen returns the current number of tracked insertion-order elements.
// Exported for tests to assert post-condition invariants without exposing the
// underlying list implementation.
func (m *codexFinalUpstreamBodyMemo) orderLen() int {
	if m == nil || m.order == nil {
		return 0
	}
	return m.order.Len()
}

func (m *codexPromptResolutionMemo) orderLen() int {
	if m == nil || m.order == nil {
		return 0
	}
	return m.order.Len()
}

func normalizeCodexFinalUpstreamBody(body []byte, baseModel string, auth *cliproxyauth.Auth, opts codexFinalUpstreamBodyOptions) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	if cached := globalCodexFinalUpstreamBodyMemo.get(baseModel, opts, body); cached != nil {
		return cached
	}

	// The memo cache on its own is sufficient: each caller gets an independent
	// clone (see memo.get) so concurrent callers with the same input may
	// redundantly recompute once before the memo warms up, but avoiding the
	// singleflight channel/goroutine hop keeps per-request latency lower for
	// the common case where each request's body is unique.
	out := normalizeCodexFinalUpstreamBodyUncached(body, baseModel, auth, opts)
	globalCodexFinalUpstreamBodyMemo.set(baseModel, opts, body, out)
	return out
}

func hashCodexFinalUpstreamBodyMemoKey(baseModel string, opts codexFinalUpstreamBodyOptions, input []byte) uint64 {
	var h maphash.Hash
	h.SetSeed(codexMemoHashSeed)
	_, _ = h.WriteString(baseModel)
	_, _ = h.Write([]byte{byte(opts.requestKind), byte(opts.streamMode), boolToByte(opts.preservePreviousResponseID)})
	_, _ = h.Write(input)
	return h.Sum64()
}

func hashCodexPromptResolutionMemoKey(from sdktranslator.Format, model string, scope string, executionSessionID string, payload []byte) uint64 {
	var h maphash.Hash
	h.SetSeed(codexMemoHashSeed)
	_, _ = h.WriteString(string(from))
	_, _ = h.WriteString(model)
	_, _ = h.WriteString(scope)
	_, _ = h.WriteString(executionSessionID)
	if len(payload) > 0 {
		_, _ = h.Write(payload)
	}
	return h.Sum64()
}

func boolToByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func promptResolutionMemoInflightKey(from sdktranslator.Format, model string, scope string, executionSessionID string, payload []byte) string {
	hash := hashCodexPromptResolutionMemoKey(from, model, scope, executionSessionID, payload)
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, hash)
	return string(from) + "|" + model + "|" + scope + "|" + executionSessionID + "|" + string(buf)
}
