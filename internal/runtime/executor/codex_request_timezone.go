package executor

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var codexEnvironmentFieldStart = regexp.MustCompile(`^<([A-Za-z_][A-Za-z0-9_.:-]*)(?:[ \t\r\n]+(?:[^<>"']|"[^"]*"|'[^']*')*)?[ \t\r\n]*/?>`)

func findCodexEnvironmentFieldEnd(text, name string) (start, end int) {
	prefix := "</" + name
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], prefix)
		if index < 0 {
			break
		}
		start = offset + index
		offset = start + len(prefix)
		rest := strings.TrimLeft(text[offset:], " \t\r\n")
		if strings.HasPrefix(rest, ">") {
			return start, len(text) - len(rest) + 1
		}
	}
	return -1, -1
}

func rewriteCodexEnvironmentTimezone(text, timezone string) string {
	trimmed := strings.TrimSpace(text)
	root := codexEnvironmentFieldStart.FindStringSubmatchIndex(trimmed)
	if root == nil || trimmed[root[2]:root[3]] != "environment_context" {
		return text
	}
	rootCloseStart, rootCloseEnd := findCodexEnvironmentFieldEnd(trimmed[root[1]:], "environment_context")
	if strings.HasSuffix(trimmed[:root[1]], "/>") || rootCloseEnd != len(trimmed)-root[1] {
		return text
	}
	content := trimmed[root[1] : root[1]+rootCloseStart]
	valueStart, valueEnd := -1, -1
	previous := ""
	for offset := 0; offset < len(content); {
		rest := strings.TrimLeftFunc(content[offset:], unicode.IsSpace)
		offset = len(content) - len(rest)
		if rest == "" {
			break
		}
		field := codexEnvironmentFieldStart.FindStringSubmatchIndex(rest)
		if field == nil {
			return text
		}
		name := rest[field[2]:field[3]]
		if name == "environment_context" {
			return text
		}
		selfClosing := strings.HasSuffix(rest[:field[1]], "/>")
		if name == "timezone" && (valueStart >= 0 || selfClosing) {
			return text
		}
		offset += field[1]
		if selfClosing {
			continue
		}
		end, closeEnd := findCodexEnvironmentFieldEnd(content[offset:], name)
		if end < 0 {
			return text
		}
		if name == "timezone" {
			value := content[offset : offset+end]
			previous = strings.TrimSpace(value)
			if previous == "" || strings.Contains(value, "<") {
				return text
			}
			valueStart = offset + len(value) - len(strings.TrimLeftFunc(value, unicode.IsSpace))
			valueEnd = offset + len(strings.TrimRightFunc(value, unicode.IsSpace))
		}
		offset += closeEnd
	}
	if valueStart < 0 || previous == timezone {
		return text
	}
	base := len(text) - len(strings.TrimLeftFunc(text, unicode.IsSpace)) + root[1]
	return text[:base+valueStart] + timezone + text[base+valueEnd:]
}

func normalizeCodexRequestTimezone(body []byte, auth *cliproxyauth.Auth) []byte {
	if len(body) == 0 || auth == nil {
		return body
	}
	timezone := auth.OpenAIRequestTimezone()
	rewriteText := func(path string, value gjson.Result) {
		if value.Type != gjson.String {
			return
		}
		original := value.String()
		updated := rewriteCodexEnvironmentTimezone(original, timezone)
		if updated == original {
			return
		}
		if rewritten, err := sjson.SetBytes(body, path, updated); err == nil {
			body = rewritten
		}
	}

	// Read both collections in one JSON scan. The paths and array indexes remain
	// stable because the normalizer only changes string values, so the snapshot
	// can safely be applied back to body through sjson below.
	values := gjson.GetManyBytes(body, "input", "tools")
	input, tools := values[0], values[1]
	if input.IsArray() {
		for inputIndex, item := range input.Array() {
			if item.Get("role").String() != "user" {
				continue
			}
			content := item.Get("content")
			path := "input." + strconv.Itoa(inputIndex) + ".content"
			if content.Type == gjson.String {
				rewriteText(path, content)
			} else if content.IsArray() {
				for contentIndex, part := range content.Array() {
					if part.Get("type").String() == "input_text" {
						rewriteText(path+"."+strconv.Itoa(contentIndex)+".text", part.Get("text"))
					}
				}
			}
		}
	}

	if tools.IsArray() {
		for index, tool := range tools.Array() {
			kind := tool.Get("type").String()
			if kind != "web_search" && !strings.HasPrefix(kind, "web_search_") {
				continue
			}
			value := tool.Get("user_location.timezone")
			if value.Type == gjson.String && value.String() != timezone {
				if rewritten, err := sjson.SetBytes(body, "tools."+strconv.Itoa(index)+".user_location.timezone", timezone); err == nil {
					body = rewritten
				}
			}
		}
	}
	return body
}
