package helps

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// zeroWidthSpace is the Unicode zero-width space character used for obfuscation.
const zeroWidthSpace = "\u200B"

const sensitiveWordMatcherCacheMaxEntries = 64

// SensitiveWordMatcher holds the compiled regex for matching sensitive words.
type SensitiveWordMatcher struct {
	regex *regexp.Regexp
}

var sensitiveWordMatcherCache = struct {
	sync.Mutex
	entries map[string]*SensitiveWordMatcher
	order   []string
}{
	entries: make(map[string]*SensitiveWordMatcher),
}

// BuildSensitiveWordMatcher compiles a regex from the word list.
// Words are sorted by length (longest first) for proper matching.
func BuildSensitiveWordMatcher(words []string) *SensitiveWordMatcher {
	if len(words) == 0 {
		return nil
	}

	cacheKey, ok := sensitiveWordMatcherCacheKey(words)
	if !ok {
		return nil
	}
	sensitiveWordMatcherCache.Lock()
	if matcher := sensitiveWordMatcherCache.entries[cacheKey]; matcher != nil {
		sensitiveWordMatcherCache.Unlock()
		return matcher
	}
	sensitiveWordMatcherCache.Unlock()

	// Filter and normalize words
	var validWords []string
	for _, w := range words {
		w = strings.TrimSpace(w)
		if utf8.RuneCountInString(w) >= 2 && !strings.Contains(w, zeroWidthSpace) {
			validWords = append(validWords, w)
		}
	}

	if len(validWords) == 0 {
		return nil
	}

	// Sort by length (longest first) for proper matching
	sort.Slice(validWords, func(i, j int) bool {
		return len(validWords[i]) > len(validWords[j])
	})

	// Escape and join
	escaped := make([]string, len(validWords))
	for i, w := range validWords {
		escaped[i] = regexp.QuoteMeta(w)
	}

	pattern := "(?i)" + strings.Join(escaped, "|")
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}

	matcher := &SensitiveWordMatcher{regex: re}
	sensitiveWordMatcherCache.Lock()
	if existing := sensitiveWordMatcherCache.entries[cacheKey]; existing != nil {
		sensitiveWordMatcherCache.Unlock()
		return existing
	}
	if len(sensitiveWordMatcherCache.order) >= sensitiveWordMatcherCacheMaxEntries {
		oldest := sensitiveWordMatcherCache.order[0]
		delete(sensitiveWordMatcherCache.entries, oldest)
		copy(sensitiveWordMatcherCache.order, sensitiveWordMatcherCache.order[1:])
		sensitiveWordMatcherCache.order = sensitiveWordMatcherCache.order[:len(sensitiveWordMatcherCache.order)-1]
	}
	sensitiveWordMatcherCache.entries[cacheKey] = matcher
	sensitiveWordMatcherCache.order = append(sensitiveWordMatcherCache.order, cacheKey)
	sensitiveWordMatcherCache.Unlock()
	return matcher
}

func sensitiveWordMatcherCacheKey(words []string) (string, bool) {
	var builder strings.Builder
	for _, w := range words {
		w = strings.TrimSpace(w)
		if utf8.RuneCountInString(w) < 2 || strings.Contains(w, zeroWidthSpace) {
			continue
		}
		builder.WriteString(strconv.Itoa(len(w)))
		builder.WriteByte(':')
		builder.WriteString(w)
	}
	if builder.Len() == 0 {
		return "", false
	}
	return builder.String(), true
}

// obfuscateWord inserts a zero-width space after the first grapheme.
func obfuscateWord(word string) string {
	if strings.Contains(word, zeroWidthSpace) {
		return word
	}

	// Get first rune
	r, size := utf8.DecodeRuneInString(word)
	if r == utf8.RuneError || size >= len(word) {
		return word
	}

	return string(r) + zeroWidthSpace + word[size:]
}

// obfuscateText replaces all sensitive words in the text.
func (m *SensitiveWordMatcher) obfuscateText(text string) string {
	if m == nil || m.regex == nil {
		return text
	}
	return m.regex.ReplaceAllStringFunc(text, obfuscateWord)
}

// ObfuscateSensitiveWords processes the payload and obfuscates sensitive words
// in system blocks and message content.
func ObfuscateSensitiveWords(payload []byte, matcher *SensitiveWordMatcher) []byte {
	if matcher == nil || matcher.regex == nil {
		return payload
	}

	// Obfuscate in system blocks
	payload = obfuscateSystemBlocks(payload, matcher)

	// Obfuscate in messages
	payload = obfuscateMessages(payload, matcher)

	return payload
}

// obfuscateSystemBlocks obfuscates sensitive words in system blocks.
func obfuscateSystemBlocks(payload []byte, matcher *SensitiveWordMatcher) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		modified := false
		system.ForEach(func(key, value gjson.Result) bool {
			if value.Get("type").String() == "text" {
				text := value.Get("text").String()
				obfuscated := matcher.obfuscateText(text)
				if obfuscated != text {
					path := "system." + key.String() + ".text"
					payload, _ = sjson.SetBytes(payload, path, obfuscated)
					modified = true
				}
			}
			return true
		})
		if modified {
			return payload
		}
	} else if system.Type == gjson.String {
		text := system.String()
		obfuscated := matcher.obfuscateText(text)
		if obfuscated != text {
			payload, _ = sjson.SetBytes(payload, "system", obfuscated)
		}
	}

	return payload
}

// obfuscateMessages obfuscates sensitive words in message content.
func obfuscateMessages(payload []byte, matcher *SensitiveWordMatcher) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	messages.ForEach(func(msgKey, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.Exists() {
			return true
		}

		msgPath := "messages." + msgKey.String()

		if content.Type == gjson.String {
			// Simple string content
			text := content.String()
			obfuscated := matcher.obfuscateText(text)
			if obfuscated != text {
				payload, _ = sjson.SetBytes(payload, msgPath+".content", obfuscated)
			}
		} else if content.IsArray() {
			// Array of content blocks
			content.ForEach(func(blockKey, block gjson.Result) bool {
				if block.Get("type").String() == "text" {
					text := block.Get("text").String()
					obfuscated := matcher.obfuscateText(text)
					if obfuscated != text {
						path := msgPath + ".content." + blockKey.String() + ".text"
						payload, _ = sjson.SetBytes(payload, path, obfuscated)
					}
				}
				return true
			})
		}

		return true
	})

	return payload
}
