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

// SanitizeCodexInputItemIDs removes encrypted reasoning items whose IDs exceed
// the Codex limit and deterministically shortens other overlong input item IDs.
func SanitizeCodexInputItemIDs(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}

	// Overlong IDs are the exception, not the rule: a well-behaved client sends
	// IDs comfortably under the limit, so the common case is a no-op. Detect
	// that with one scan before allocating the maps and the rebuilt item slice
	// below, which otherwise reserialize the whole input array to produce a
	// payload identical to the one we were given.
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
		if !codexInputItemIDTooLong(id) {
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
			if codexInputItemIDTooLong(id) {
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

// codexInputItemIDsNeedSanitizing reports whether any input item carries an ID
// longer than the Codex limit. Both mutations performed by
// SanitizeCodexInputItemIDs — dropping encrypted reasoning items and shortening
// IDs — are gated on an overlong ID, so this is the exact precondition for the
// function doing any work at all.
func codexInputItemIDsNeedSanitizing(input gjson.Result) bool {
	overlong := false
	input.ForEach(func(_, item gjson.Result) bool {
		if itemID := item.Get("id"); itemID.Type == gjson.String && codexInputItemIDTooLong(itemID.String()) {
			overlong = true
			return false
		}
		return true
	})
	return overlong
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
	if itemID.Type != gjson.String || !codexInputItemIDTooLong(itemID.String()) {
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
