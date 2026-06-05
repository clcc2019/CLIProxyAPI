// Package common holds helpers shared across the codex translator variants
// (claude, codex, openai). Keeping the tool-name shortening logic in one place
// avoids the risk of the copies drifting apart.
package common

import (
	"strconv"
	"strings"
)

// ShortNameLimit is the maximum length the upstream Codex backend accepts for
// tool names. It is also exported so translator-specific helpers can derive
// additional sizing rules from the same constant.
const ShortNameLimit = 64

// ShortenNameIfNeeded applies the tool-name shortening rule for a single name.
// It preserves the "mcp__" prefix convention when present; otherwise it simply
// truncates to ShortNameLimit.
func ShortenNameIfNeeded(name string) string {
	if len(name) <= ShortNameLimit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > ShortNameLimit {
				return cand[:ShortNameLimit]
			}
			return cand
		}
	}
	return name[:ShortNameLimit]
}

// BuildShortNameMap ensures uniqueness of shortened names within a request.
// It returns a map from original name to (possibly truncated and de-duplicated)
// short name. Iteration order of the input slice determines collision
// resolution order.
func BuildShortNameMap(names []string) map[string]string {
	used := make(map[string]struct{}, len(names))
	m := make(map[string]string, len(names))

	baseCandidate := func(n string) string {
		if len(n) <= ShortNameLimit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > ShortNameLimit {
					cand = cand[:ShortNameLimit]
				}
				return cand
			}
		}
		return n[:ShortNameLimit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := ShortNameLimit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}
