package util

import "unicode/utf8"

// AppendJSONString appends value to dst as an RFC 8259 JSON string literal,
// including the surrounding quotes. It escapes only the characters JSON
// requires and deliberately leaves HTML characters (<, >, &) unescaped to
// match tidwall/sjson's output.
func AppendJSONString(dst []byte, value string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char >= utf8.RuneSelf {
			runeValue, size := utf8.DecodeRuneInString(value[index:])
			if runeValue != utf8.RuneError || size > 1 {
				index += size - 1
				continue
			}
			if index > start {
				dst = append(dst, value[start:index]...)
			}
			dst = append(dst, `\ufffd`...)
			start = index + 1
			continue
		}
		if char >= 0x20 && char != '"' && char != '\\' {
			continue
		}
		if index > start {
			dst = append(dst, value[start:index]...)
		}
		switch char {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hex[char>>4], hex[char&0x0f])
		}
		start = index + 1
	}
	if start < len(value) {
		dst = append(dst, value[start:]...)
	}
	return append(dst, '"')
}
