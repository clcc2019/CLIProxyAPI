package util

import (
	"unsafe"

	"github.com/tidwall/gjson"
)

// GetGJSONBytesNoCopy parses a byte payload without creating an intermediate
// string. Callers must not retain the result after the payload is reused or
// mutated.
func GetGJSONBytesNoCopy(data []byte, path string) gjson.Result {
	if len(data) == 0 {
		return gjson.Result{}
	}
	return gjson.Get(unsafe.String(unsafe.SliceData(data), len(data)), path)
}
