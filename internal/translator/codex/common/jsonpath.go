package common

import "strconv"

// IndexedPath composes an sjson/gjson path of the form "<prefix>.<idx>.<suffix>"
// without the allocation overhead of fmt.Sprintf. Hot translator paths build one
// path per array element, so keeping this on a single append-based allocation
// matters: fmt.Sprintf costs a format parse plus an interface boxing per call.
//
// An empty prefix or suffix is elided along with its separator, so
// IndexedPath("input", 3, "role") yields "input.3.role" and
// IndexedPath("", 3, "") yields "3".
func IndexedPath(prefix string, idx int, suffix string) string {
	// Capacity: prefix + '.' + up to 20 digits + '.' + suffix.
	buf := make([]byte, 0, len(prefix)+len(suffix)+22)
	if prefix != "" {
		buf = append(buf, prefix...)
		buf = append(buf, '.')
	}
	buf = strconv.AppendInt(buf, int64(idx), 10)
	if suffix != "" {
		buf = append(buf, '.')
		buf = append(buf, suffix...)
	}
	return string(buf)
}
