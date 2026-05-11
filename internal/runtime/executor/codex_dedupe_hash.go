package executor

import (
	"encoding/binary"
	"encoding/hex"
	"hash/maphash"
	"net/http"
)

// codexResponseDedupeHashLen is the number of hex characters retained from a
// 64-bit maphash output when used as a dedupe-key fragment. 16 hex chars gives
// ~2^64 hash space which is plenty for the short-lived, in-process dedupe
// window (far shorter than the time needed for a meaningful collision).
const codexResponseDedupeHashLen = 16

// codexShortHashSeed keeps shortHash*/hashCodexDedupeHeaders stable for the
// lifetime of the process. The dedupe cache never persists across restarts, so
// re-seeding on each boot is harmless and avoids making the seed a cross-run
// invariant that SHA-256 would otherwise enforce at a much higher CPU cost.
var codexShortHashSeed = maphash.MakeSeed()

// hashCodexDedupeHeaders produces a stable hash over the dedupe-relevant
// subset of a request's headers. Returns the sentinel "none" when no relevant
// headers are present, so dedupe keys generated from totally absent header
// sets stay equal across requests.
func hashCodexDedupeHeaders(headers http.Header) string {
	if len(headers) == 0 {
		return "none"
	}

	var h maphash.Hash
	h.SetSeed(codexShortHashSeed)
	wrote := false
	for _, key := range codexDedupeRelevantHeaders {
		values := codexDedupeHeaderValues(headers, key)
		if len(values) == 0 {
			continue
		}
		writeCodexHeaderValues(&h, key, values)
		wrote = true
	}
	if !wrote {
		return "none"
	}
	return codexMaphashHex(&h)
}

// codexDedupeHeaderValues returns the canonical + raw-key fallback so callers
// do not have to pre-canonicalise keys at lookup sites.
func codexDedupeHeaderValues(headers http.Header, key string) []string {
	if len(headers) == 0 {
		return nil
	}
	if values := headers.Values(key); len(values) > 0 {
		return values
	}
	return headers[key]
}

// writeCodexHeaderValues streams a canonical "Key=Value\n" (or
// "Key=V1\x00V2\n" for multi-valued headers) fragment through the hasher
// without allocating a fresh []byte per string. maphash.Hash implements
// io.StringWriter so each WriteString writes directly without going through a
// temporary byte slice.
func writeCodexHeaderValues(h *maphash.Hash, key string, values []string) {
	if h == nil || key == "" || len(values) == 0 {
		return
	}

	var sep [1]byte
	_, _ = h.WriteString(key)
	sep[0] = '='
	_, _ = h.Write(sep[:])
	if len(values) == 1 {
		_, _ = h.WriteString(values[0])
		sep[0] = '\n'
		_, _ = h.Write(sep[:])
		return
	}
	for i := range values {
		if i > 0 {
			sep[0] = 0
			_, _ = h.Write(sep[:])
		}
		_, _ = h.WriteString(values[i])
	}
	sep[0] = '\n'
	_, _ = h.Write(sep[:])
}

// codexMaphashHex returns the lower-case hex encoding of h.Sum64 truncated to
// codexResponseDedupeHashLen. The output width matches the legacy
// SHA-256-truncated-to-16-hex format so downstream key composition (e.g.
// "codex|scope|POST|url|promptCacheID|bodyHash|headersHash") keeps the same
// shape.
func codexMaphashHex(h *maphash.Hash) string {
	var sum [8]byte
	binary.BigEndian.PutUint64(sum[:], h.Sum64())
	var encoded [16]byte
	hex.Encode(encoded[:], sum[:])
	return string(encoded[:codexResponseDedupeHashLen])
}

func shortHashString(value string) string {
	var h maphash.Hash
	h.SetSeed(codexShortHashSeed)
	_, _ = h.WriteString(value)
	return codexMaphashHex(&h)
}

func shortHashBytes(value []byte) string {
	var h maphash.Hash
	h.SetSeed(codexShortHashSeed)
	_, _ = h.Write(value)
	return codexMaphashHex(&h)
}
