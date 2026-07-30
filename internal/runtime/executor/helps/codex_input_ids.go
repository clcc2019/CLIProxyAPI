package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexInputItemIDLimit = 64

// SanitizeCodexInputItemIDs removes legacy item IDs that the official Codex
// client no longer sends, removes encrypted reasoning items whose valid IDs
// exceed the Codex limit, and deterministically shortens other overlong valid
// input item IDs.
func SanitizeCodexInputItemIDs(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}

	// Invalid and overlong IDs are exceptional, so detect them with one scan
	// before allocating maps and rebuilding the input array. This preserves the
	// allocation-free no-op path for the normal case.
	if !codexInputItemIDsNeedSanitizing(input) {
		return body
	}

	items := input.Array()
	occupied := make(map[string]struct{}, len(items))
	for _, item := range items {
		if shouldDropCodexEncryptedReasoningItem(item) {
			continue
		}
		itemID := item.Get("id")
		if itemID.Type != gjson.String {
			continue
		}
		id := itemID.String()
		if codexInputItemIDHasPrefix(id) && !codexInputItemIDTooLong(id) {
			occupied[id] = struct{}{}
		}
	}

	mapped := make(map[string]string, len(items))
	rebuilt := make([]string, 0, len(items))
	changed := false
	for _, item := range items {
		if shouldDropCodexEncryptedReasoningItem(item) {
			changed = true
			continue
		}

		raw := item.Raw
		itemID := item.Get("id")
		if itemID.Type == gjson.String {
			id := itemID.String()
			if !codexInputItemIDHasPrefix(id) {
				next, err := sjson.DeleteBytes([]byte(raw), "id")
				if err == nil {
					raw = string(next)
					changed = true
				}
			} else if codexInputItemIDTooLong(id) {
				shortened, ok := mapped[id]
				if !ok {
					shortened = shortenCodexInputItemID(id)
					for attempt := 1; ; attempt++ {
						if _, exists := occupied[shortened]; !exists {
							break
						}
						shortened = shortenCodexInputItemIDWithAttempt(id, attempt)
					}
					mapped[id] = shortened
					occupied[shortened] = struct{}{}
				}

				next, err := sjson.SetBytes([]byte(raw), "id", shortened)
				if err == nil {
					raw = string(next)
					changed = true
				}
			}
		}
		rebuilt = append(rebuilt, raw)
	}
	if !changed {
		return body
	}

	updated, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if err != nil {
		return body
	}
	return updated
}

// codexInputItemIDsNeedSanitizing reports whether an input item carries an
// empty, legacy/unprefixed, or overlong ID.
func codexInputItemIDsNeedSanitizing(input gjson.Result) bool {
	needsSanitizing := false
	input.ForEach(func(_, item gjson.Result) bool {
		if itemID := item.Get("id"); itemID.Type == gjson.String && (!codexInputItemIDHasPrefix(itemID.String()) || codexInputItemIDTooLong(itemID.String())) {
			needsSanitizing = true
			return false
		}
		return true
	})
	return needsSanitizing
}

// codexInputItemIDHasPrefix reports the compatibility rule used by codex-rs:
// an outbound response item ID needs a non-empty prefix and suffix separated by
// an underscore. Deserialization remains permissive; only the upstream request
// is normalized.
func codexInputItemIDHasPrefix(id string) bool {
	prefix, suffix, found := strings.Cut(id, "_")
	return found && prefix != "" && suffix != ""
}

// codexInputItemIDTooLong reports whether id exceeds the Codex input item ID
// limit, measured in runes. utf8.RuneCountInString counts in place, whereas
// len([]rune(id)) allocates a rune slice per ID purely to read its length.
func codexInputItemIDTooLong(id string) bool {
	return utf8.RuneCountInString(id) > codexInputItemIDLimit
}

func shouldDropCodexEncryptedReasoningItem(item gjson.Result) bool {
	if item.Get("type").String() != "reasoning" {
		return false
	}
	itemID := item.Get("id")
	if itemID.Type != gjson.String || !codexInputItemIDHasPrefix(itemID.String()) || !codexInputItemIDTooLong(itemID.String()) {
		return false
	}
	encryptedContent := item.Get("encrypted_content")
	return encryptedContent.Type == gjson.String && encryptedContent.String() != ""
}

func shortenCodexInputItemID(id string) string {
	return shortenCodexInputItemIDWithAttempt(id, 0)
}

func shortenCodexInputItemIDWithAttempt(id string, attempt int) string {
	runes := []rune(id)
	if len(runes) <= codexInputItemIDLimit {
		return id
	}

	hashInput := id
	if attempt > 0 {
		hashInput += "\x00" + strconv.Itoa(attempt)
	}
	sum := sha256.Sum256([]byte(hashInput))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLength := codexInputItemIDLimit - len(suffix)
	return string(runes[:prefixLength]) + suffix
}
