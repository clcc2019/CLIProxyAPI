// Package store — shared helpers for build-tagged backend stubs.
//
// Token-store backends (Postgres, Minio/S3, go-git) are gated behind build
// tags to keep the default binary small. When a backend is not compiled in,
// the corresponding stub returns an error produced by errBackendNotCompiled
// so operators get an actionable message rather than a silent failure or a
// nil-pointer panic.
//
// This file also hosts utility helpers (valueAsString / normalizeAuthID /
// labelFor) that are shared across multiple backends; keeping them in an
// untagged file lets any combination of backends compile without duplicating
// the helpers into each tag-guarded file.
package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// errBackendNotCompiled formats a consistent "this backend was not compiled
// in" error message. The name is the human-facing backend label (e.g.
// "postgres") and tag is the build tag needed to include it (e.g.
// "has_postgres"). Both are required so the message is actionable.
func errBackendNotCompiled(name, tag string) error {
	return fmt.Errorf(
		"%s token store is not compiled in; rebuild with -tags=%s (or the slim Makefile target) to enable it",
		name, tag,
	)
}

// valueAsString normalises an untyped map value into a string when the
// concrete type is either `string` or implements fmt.Stringer. Used by the
// metadata-to-label lookups in multiple backends.
func valueAsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

// labelFor picks the most descriptive label for an auth record from its
// metadata. Multiple backends store auths under label-derived filenames, so
// the lookup must agree across implementations.
func labelFor(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v := strings.TrimSpace(valueAsString(metadata["label"])); v != "" {
		return v
	}
	if v := strings.TrimSpace(valueAsString(metadata["email"])); v != "" {
		return v
	}
	if v := strings.TrimSpace(valueAsString(metadata["project_id"])); v != "" {
		return v
	}
	return ""
}

// normalizeAuthID canonicalises a free-form auth ID to a forward-slash path
// form that's safe to use as a filename or key across operating systems.
func normalizeAuthID(id string) string {
	return filepath.ToSlash(filepath.Clean(id))
}

// jsonEqual reports whether two JSON byte slices represent the same value
// regardless of field ordering or trivial whitespace. Used by multiple
// backends when deciding whether a write would actually change state.
func jsonEqual(a, b []byte) bool {
	var objA any
	var objB any
	if err := json.Unmarshal(a, &objA); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &objB); err != nil {
		return false
	}
	return deepEqualJSON(objA, objB)
}

// deepEqualJSON compares two decoded JSON values for deep equality. Kept
// here (rather than relying on reflect.DeepEqual) because JSON numbers all
// decode to float64 and we want the comparison to ignore that detail —
// reflect.DeepEqual would too but this inlines the type switch so the hot
// paths avoid the reflect overhead.
func deepEqualJSON(a, b any) bool {
	switch valA := a.(type) {
	case map[string]any:
		valB, ok := b.(map[string]any)
		if !ok || len(valA) != len(valB) {
			return false
		}
		for key, subA := range valA {
			subB, ok1 := valB[key]
			if !ok1 || !deepEqualJSON(subA, subB) {
				return false
			}
		}
		return true
	case []any:
		sliceB, ok := b.([]any)
		if !ok || len(valA) != len(sliceB) {
			return false
		}
		for i := range valA {
			if !deepEqualJSON(valA[i], sliceB[i]) {
				return false
			}
		}
		return true
	case float64:
		valB, ok := b.(float64)
		if !ok {
			return false
		}
		return valA == valB
	case string:
		valB, ok := b.(string)
		if !ok {
			return false
		}
		return valA == valB
	case bool:
		valB, ok := b.(bool)
		if !ok {
			return false
		}
		return valA == valB
	case nil:
		return b == nil
	default:
		return false
	}
}
