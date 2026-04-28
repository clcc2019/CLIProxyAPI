package executor

import (
	"bytes"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type codexStreamFieldMode uint8

const (
	codexStreamFieldKeep codexStreamFieldMode = iota
	codexStreamFieldTrue
	codexStreamFieldFalse
	codexStreamFieldDelete
)

type codexFinalUpstreamRequestKind uint8

const (
	codexFinalUpstreamResponses codexFinalUpstreamRequestKind = iota
	codexFinalUpstreamCompact
)

type codexFinalUpstreamBodyOptions struct {
	requestKind                codexFinalUpstreamRequestKind
	streamMode                 codexStreamFieldMode
	preservePreviousResponseID bool
}

func codexFinalUpstreamRequestKindForURL(rawURL string) codexFinalUpstreamRequestKind {
	trimmed := strings.TrimSpace(rawURL)
	path := trimmed
	if parsed, err := url.Parse(trimmed); err == nil && strings.TrimSpace(parsed.Path) != "" {
		path = parsed.Path
	}
	path = strings.TrimSuffix(strings.TrimSpace(path), "/")
	if strings.HasSuffix(path, "/responses/compact") {
		return codexFinalUpstreamCompact
	}
	return codexFinalUpstreamResponses
}

var codexAllowedResponsesFinalUpstreamFields = map[string]struct{}{
	"model":               {},
	"instructions":        {},
	"input":               {},
	"tools":               {},
	"tool_choice":         {},
	"parallel_tool_calls": {},
	"reasoning":           {},
	"store":               {},
	"stream":              {},
	"include":             {},
	"service_tier":        {},
	"prompt_cache_key":    {},
	"text":                {},
	"client_metadata":     {},
}

var codexAllowedCompactFinalUpstreamFields = map[string]struct{}{
	"model":               {},
	"instructions":        {},
	"input":               {},
	"tools":               {},
	"parallel_tool_calls": {},
	"reasoning":           {},
	"text":                {},
}

func codexEnsureFinalUpstreamBodyDefaults(body []byte, opts codexFinalUpstreamBodyOptions) []byte {
	appendFields := make([]codexTopLevelRawField, 0, 4)
	edits := make([]helps.JSONEdit, 0, 4)
	addDefault := func(field string, rawValue []byte) {
		current := gjson.GetBytes(body, field)
		if !current.Exists() {
			appendFields = append(appendFields, codexTopLevelRawField{field: field, rawValue: rawValue})
			return
		}
		if current.Type == gjson.Null {
			edits = append(edits, helps.SetRawJSONEdit(field, rawValue))
		}
	}

	switch opts.requestKind {
	case codexFinalUpstreamCompact:
		addDefault("tools", []byte("[]"))
		addDefault("parallel_tool_calls", []byte("true"))
	default:
		addDefault("tools", []byte("[]"))
		addDefault("tool_choice", []byte(`"auto"`))
		addDefault("parallel_tool_calls", []byte("true"))
		addDefault("include", []byte("[]"))
	}
	if len(appendFields) > 0 {
		if updated, ok := codexAppendTopLevelRawFields(body, appendFields); ok {
			body = updated
		} else {
			for _, entry := range appendFields {
				edits = append(edits, helps.SetRawJSONEdit(entry.field, entry.rawValue))
			}
		}
	}
	if len(edits) == 0 {
		return body
	}
	return helps.EditJSONBytes(body, edits...)
}

func pruneCodexFinalUpstreamBody(body []byte, opts codexFinalUpstreamBodyOptions) []byte {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body
	}

	allowedFields := codexAllowedResponsesFinalUpstreamFields
	if opts.requestKind == codexFinalUpstreamCompact {
		allowedFields = codexAllowedCompactFinalUpstreamFields
	}

	edits := make([]helps.JSONEdit, 0, 8)
	root.ForEach(func(key, _ gjson.Result) bool {
		field := strings.TrimSpace(key.String())
		if field == "" {
			return true
		}
		if field == "previous_response_id" && opts.preservePreviousResponseID {
			return true
		}
		if _, ok := allowedFields[field]; ok {
			return true
		}
		edits = append(edits, helps.DeleteJSONEdit(field))
		return true
	})

	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() || instructions.Type == gjson.Null || (instructions.Type == gjson.String && instructions.String() == "") {
		edits = append(edits, helps.DeleteJSONEdit("instructions"))
	}
	if len(edits) == 0 {
		return body
	}
	return helps.EditJSONBytes(body, edits...)
}

func normalizeCodexFinalUpstreamBodyUncached(body []byte, baseModel string, auth *cliproxyauth.Auth, opts codexFinalUpstreamBodyOptions) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}

	body = codexEnsureFinalUpstreamBodyDefaults(body, opts)

	edits := make([]helps.JSONEdit, 0, 3)
	if model := gjson.GetBytes(body, "model"); !model.Exists() || model.Type != gjson.String || model.String() != baseModel {
		edits = append(edits, helps.SetJSONEdit("model", baseModel))
	}
	if opts.requestKind == codexFinalUpstreamResponses {
		store := gjson.GetBytes(body, "store")
		if !store.Exists() {
			if updated, ok := codexAppendTopLevelRawField(body, "store", []byte("false")); ok {
				body = updated
			} else {
				edits = append(edits, helps.SetRawJSONEdit("store", []byte("false")))
			}
		} else if store.Type != gjson.False {
			edits = append(edits, helps.SetRawJSONEdit("store", []byte("false")))
		}
	}
	switch opts.streamMode {
	case codexStreamFieldTrue:
		if stream := gjson.GetBytes(body, "stream"); !stream.Exists() {
			if updated, ok := codexAppendTopLevelRawField(body, "stream", []byte("true")); ok {
				body = updated
			} else {
				edits = append(edits, helps.SetRawJSONEdit("stream", []byte("true")))
			}
		} else if stream.Type != gjson.True {
			edits = append(edits, helps.SetRawJSONEdit("stream", []byte("true")))
		}
	case codexStreamFieldFalse:
		if stream := gjson.GetBytes(body, "stream"); !stream.Exists() {
			if updated, ok := codexAppendTopLevelRawField(body, "stream", []byte("false")); ok {
				body = updated
			} else {
				edits = append(edits, helps.SetRawJSONEdit("stream", []byte("false")))
			}
		} else if stream.Type != gjson.False {
			edits = append(edits, helps.SetRawJSONEdit("stream", []byte("false")))
		}
	case codexStreamFieldDelete:
		edits = append(edits, helps.DeleteJSONEdit("stream"))
	}

	if len(edits) > 0 {
		body = helps.EditJSONBytes(body, edits...)
	}
	body = pruneCodexFinalUpstreamBody(body, opts)
	return body
}
