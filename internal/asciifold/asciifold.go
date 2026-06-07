package asciifold

func Contains(s, substr string) bool {
	if substr == "" {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	first := lower(substr[0])
	limit := len(s) - len(substr)
	for i := 0; i <= limit; i++ {
		if lower(s[i]) != first {
			continue
		}
		if equalAt(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func Index(s, substr string) int {
	if substr == "" {
		return 0
	}
	if len(substr) > len(s) {
		return -1
	}
	first := lower(substr[0])
	limit := len(s) - len(substr)
	for i := 0; i <= limit; i++ {
		if lower(s[i]) != first {
			continue
		}
		if equalAt(s[i:i+len(substr)], substr) {
			return i
		}
	}
	return -1
}

func ContainsBytes(s []byte, substr string) bool {
	if substr == "" {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	first := lower(substr[0])
	limit := len(s) - len(substr)
	for i := 0; i <= limit; i++ {
		if lower(s[i]) != first {
			continue
		}
		if equalBytesAt(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func IndexBytes(s []byte, substr string) int {
	if substr == "" {
		return 0
	}
	if len(substr) > len(s) {
		return -1
	}
	first := lower(substr[0])
	limit := len(s) - len(substr)
	for i := 0; i <= limit; i++ {
		if lower(s[i]) != first {
			continue
		}
		if equalBytesAt(s[i:i+len(substr)], substr) {
			return i
		}
	}
	return -1
}

func HasPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return equalAt(s[:len(prefix)], prefix)
}

func HasSuffix(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return equalAt(s[len(s)-len(suffix):], suffix)
}

func HasPrefixBytes(s []byte, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return equalBytesAt(s[:len(prefix)], prefix)
}

func Equal(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return equalAt(a, b)
}

func equalAt(s, substr string) bool {
	for i := 0; i < len(substr); i++ {
		if lower(s[i]) != lower(substr[i]) {
			return false
		}
	}
	return true
}

func equalBytesAt(s []byte, substr string) bool {
	for i := 0; i < len(substr); i++ {
		if lower(s[i]) != lower(substr[i]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
