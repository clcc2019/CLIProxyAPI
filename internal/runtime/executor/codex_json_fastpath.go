package executor

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

func codexSetPromptCacheKey(body []byte, cacheID string) []byte {
	cacheID = strings.TrimSpace(cacheID)
	if len(body) == 0 || cacheID == "" {
		return body
	}

	existing := gjson.GetBytes(body, "prompt_cache_key")
	if existing.Exists() && existing.Type == gjson.String && existing.String() == cacheID {
		return body
	}
	if !existing.Exists() {
		if updated, ok := codexAppendTopLevelStringField(body, "prompt_cache_key", cacheID); ok {
			return updated
		}
	}

	updated, err := helps.SetJSONBytes(body, "prompt_cache_key", cacheID)
	if err != nil {
		return body
	}
	return updated
}

func codexAppendTopLevelStringField(body []byte, field string, value string) ([]byte, bool) {
	trimmed, suffix, hasFields, ok := codexPrepareTopLevelObjectAppend(body)
	if !ok {
		return nil, false
	}

	buf := make([]byte, 0, len(body)+len(field)+len(value)+8)
	buf = append(buf, trimmed[:len(trimmed)-1]...)
	if hasFields {
		buf = append(buf, ',')
	}
	buf = strconv.AppendQuote(buf, field)
	buf = append(buf, ':')
	buf = strconv.AppendQuote(buf, value)
	buf = append(buf, '}')
	buf = append(buf, suffix...)
	return buf, true
}

func codexAppendTopLevelSingleStringObjectField(body []byte, field string, key string, value string) ([]byte, bool) {
	trimmed, suffix, hasFields, ok := codexPrepareTopLevelObjectAppend(body)
	if !ok {
		return nil, false
	}

	buf := make([]byte, 0, len(body)+len(field)+len(key)+len(value)+12)
	buf = append(buf, trimmed[:len(trimmed)-1]...)
	if hasFields {
		buf = append(buf, ',')
	}
	buf = strconv.AppendQuote(buf, field)
	buf = append(buf, ':', '{')
	buf = strconv.AppendQuote(buf, key)
	buf = append(buf, ':')
	buf = strconv.AppendQuote(buf, value)
	buf = append(buf, '}', '}')
	buf = append(buf, suffix...)
	return buf, true
}

func codexAppendTopLevelRawField(body []byte, field string, rawValue []byte) ([]byte, bool) {
	return codexAppendTopLevelRawFields(body, []codexTopLevelRawField{{field: field, rawValue: rawValue}})
}

type codexTopLevelRawField struct {
	field    string
	rawValue []byte
}

func codexAppendTopLevelRawFields(body []byte, fields []codexTopLevelRawField) ([]byte, bool) {
	if len(fields) == 0 {
		return nil, false
	}
	trimmed, suffix, hasFields, ok := codexPrepareTopLevelObjectAppend(body)
	if !ok {
		return nil, false
	}

	extra := 0
	fieldCount := 0
	for _, entry := range fields {
		if entry.field == "" || len(entry.rawValue) == 0 {
			continue
		}
		fieldCount++
		extra += len(entry.field) + len(entry.rawValue) + 4
	}
	if fieldCount == 0 {
		return nil, false
	}

	buf := make([]byte, 0, len(body)+extra)
	buf = append(buf, trimmed[:len(trimmed)-1]...)
	needComma := hasFields
	for _, entry := range fields {
		if entry.field == "" || len(entry.rawValue) == 0 {
			continue
		}
		if needComma {
			buf = append(buf, ',')
		}
		buf = strconv.AppendQuote(buf, entry.field)
		buf = append(buf, ':')
		buf = append(buf, entry.rawValue...)
		needComma = true
	}
	buf = append(buf, '}')
	buf = append(buf, suffix...)
	return buf, true
}

func codexPrepareTopLevelObjectAppend(body []byte) (trimmed []byte, suffix []byte, hasFields bool, ok bool) {
	trimmed = bytes.TrimRight(body, " \t\r\n")
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return nil, nil, false, false
	}
	inner := bytes.TrimSpace(trimmed[1 : len(trimmed)-1])
	return trimmed, body[len(trimmed):], len(inner) > 0, true
}
