package executor

import (
	"bytes"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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

// codexFinalUpstreamRequestKindForURL classifies the request kind from the
// target URL. It avoids url.Parse because we only need a cheap suffix match
// on the path portion of the URL; parsing a full net/url is overkill in the
// per-request hot path.
func codexFinalUpstreamRequestKindForURL(rawURL string) codexFinalUpstreamRequestKind {
	path := strings.TrimSpace(rawURL)
	// Drop query/fragment without allocating a parsed URL.
	if idx := strings.IndexAny(path, "?#"); idx >= 0 {
		path = path[:idx]
	}
	// Trim any trailing slashes to make the suffix check robust.
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	if strings.HasSuffix(path, "/responses/compact") {
		return codexFinalUpstreamCompact
	}
	return codexFinalUpstreamResponses
}

var codexAllowedResponsesFinalUpstreamFields = map[string]struct{}{
	"model":                  {},
	"instructions":           {},
	"input":                  {},
	"tools":                  {},
	"tool_choice":            {},
	"parallel_tool_calls":    {},
	"reasoning":              {},
	"store":                  {},
	"stream":                 {},
	"include":                {},
	"service_tier":           {},
	"prompt_cache_key":       {},
	"prompt_cache_retention": {},
	"text":                   {},
	"client_metadata":        {},
}

var codexAllowedCompactFinalUpstreamFields = map[string]struct{}{
	"model":               {},
	"instructions":        {},
	"input":               {},
	"tools":               {},
	"parallel_tool_calls": {},
	"reasoning":           {},
	"service_tier":        {},
	"prompt_cache_key":    {},
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

// codexFinalUpstreamScanFields lists the fields read by
// normalizeCodexFinalUpstreamBodyUncached after tools have been normalized, in
// the precise order consumed below. Batching them through GetManyBytes costs
// one payload parse instead of four sequential scans.
var codexFinalUpstreamScanFields = []string{
	"model",        // idx 0
	"store",        // idx 1 (only inspected on the /responses kind)
	"instructions", // idx 2
	"stream",       // idx 3
}

func normalizeCodexFinalUpstreamBodyUncached(body []byte, baseModel string, auth *cliproxyauth.Auth, opts codexFinalUpstreamBodyOptions) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}

	body = codexEnsureFinalUpstreamBodyDefaults(body, opts)
	body = normalizeCodexFinalUpstreamTools(body)

	// Resolve all four inspected fields in a single payload traversal so
	// downstream branches can reuse the decoded Result values rather than
	// re-parsing the body once per field.
	scanned := gjson.GetManyBytes(body, codexFinalUpstreamScanFields...)
	model := scanned[0]
	store := scanned[1]
	instructions := scanned[2]
	stream := scanned[3]

	edits := make([]helps.JSONEdit, 0, 3)
	if !model.Exists() || model.Type != gjson.String || model.String() != baseModel {
		edits = append(edits, helps.SetJSONEdit("model", baseModel))
	}
	if opts.requestKind == codexFinalUpstreamResponses {
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
	if !instructions.Exists() || instructions.Type == gjson.Null || (instructions.Type == gjson.String && strings.TrimSpace(instructions.String()) == "") {
		instructionText := codexDefaultInstructionsFromBody(body)
		if !instructions.Exists() {
			if updated, ok := codexAppendTopLevelStringField(body, "instructions", instructionText); ok {
				body = updated
			} else {
				edits = append(edits, helps.SetJSONEdit("instructions", instructionText))
			}
		} else {
			edits = append(edits, helps.SetJSONEdit("instructions", instructionText))
		}
	}
	switch opts.streamMode {
	case codexStreamFieldTrue:
		if !stream.Exists() {
			if updated, ok := codexAppendTopLevelRawField(body, "stream", []byte("true")); ok {
				body = updated
			} else {
				edits = append(edits, helps.SetRawJSONEdit("stream", []byte("true")))
			}
		} else if stream.Type != gjson.True {
			edits = append(edits, helps.SetRawJSONEdit("stream", []byte("true")))
		}
	case codexStreamFieldFalse:
		if !stream.Exists() {
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

func normalizeCodexFinalUpstreamTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		if rawTools, ok := normalizeCodexFinalUpstreamToolsArray(tools); ok {
			if updated, err := helps.SetRawJSONBytes(body, "tools", rawTools); err == nil {
				body = updated
			}
		}
	}

	toolChoice := gjson.GetBytes(body, "tool_choice")
	if toolChoice.Exists() {
		if rawChoice, ok := normalizeCodexFinalUpstreamToolChoice(toolChoice); ok {
			if updated, err := helps.SetRawJSONBytes(body, "tool_choice", rawChoice); err == nil {
				body = updated
			}
		}
	}
	return body
}

func normalizeCodexFinalUpstreamToolsArray(tools gjson.Result) ([]byte, bool) {
	if !tools.IsArray() {
		return nil, false
	}

	items := make([][]byte, 0, len(tools.Array()))
	tools.ForEach(func(_, tool gjson.Result) bool {
		if rawTool, keep := normalizeCodexFinalUpstreamTool(tool); keep {
			items = append(items, rawTool)
		}
		return true
	})

	return codexRawJSONArray(items), true
}

func normalizeCodexFinalUpstreamTool(tool gjson.Result) ([]byte, bool) {
	if !tool.IsObject() {
		return nil, false
	}

	toolType := normalizeCodexFinalUpstreamToolType(tool.Get("type").String())
	if toolType == "" {
		if strings.TrimSpace(tool.Get("name").String()) == "" && !tool.Get("input_schema").Exists() && !tool.Get("parameters").Exists() {
			return nil, false
		}
		toolType = "function"
	}

	switch toolType {
	case "function":
		return normalizeCodexFinalUpstreamFunctionTool(tool)
	case "web_search":
		return normalizeCodexFinalUpstreamWebSearchTool(tool), true
	case "image_generation":
		raw := []byte(tool.Raw)
		raw, _ = helps.SetJSONBytes(raw, "type", "image_generation")
		return raw, true
	default:
		if strings.TrimSpace(tool.Get("name").String()) == "" {
			return nil, false
		}
		return normalizeCodexFinalUpstreamFunctionTool(tool)
	}
}

func normalizeCodexFinalUpstreamFunctionTool(tool gjson.Result) ([]byte, bool) {
	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		return nil, false
	}

	raw := []byte(tool.Raw)
	raw, _ = helps.SetJSONBytes(raw, "type", "function")
	raw, _ = helps.SetJSONBytes(raw, "name", name)
	parameters := tool.Get("parameters")
	if !parameters.Exists() || parameters.Type == gjson.Null {
		if inputSchema := tool.Get("input_schema"); inputSchema.Exists() && inputSchema.Type != gjson.Null {
			raw, _ = helps.SetRawJSONBytes(raw, "parameters", []byte(inputSchema.Raw))
		} else {
			raw, _ = helps.SetRawJSONBytes(raw, "parameters", []byte(`{"type":"object","properties":{}}`))
		}
	}
	if !tool.Get("strict").Exists() || tool.Get("strict").Type == gjson.Null {
		raw, _ = helps.SetJSONBytes(raw, "strict", false)
	}
	raw, _ = helps.DeleteJSONBytes(raw, "input_schema")
	raw, _ = helps.DeleteJSONBytes(raw, "parameters.$schema")
	raw, _ = helps.DeleteJSONBytes(raw, "cache_control")
	raw, _ = helps.DeleteJSONBytes(raw, "defer_loading")
	return raw, true
}

func normalizeCodexFinalUpstreamWebSearchTool(tool gjson.Result) []byte {
	raw := []byte(tool.Raw)
	raw, _ = helps.SetJSONBytes(raw, "type", "web_search")
	if allowedDomains := tool.Get("allowed_domains"); allowedDomains.Exists() && allowedDomains.IsArray() && !tool.Get("filters.allowed_domains").Exists() {
		raw, _ = helps.SetRawJSONBytes(raw, "filters.allowed_domains", []byte(allowedDomains.Raw))
	}
	raw, _ = helps.DeleteJSONBytes(raw, "name")
	raw, _ = helps.DeleteJSONBytes(raw, "input_schema")
	raw, _ = helps.DeleteJSONBytes(raw, "allowed_domains")
	raw, _ = helps.DeleteJSONBytes(raw, "blocked_domains")
	raw, _ = helps.DeleteJSONBytes(raw, "cache_control")
	raw, _ = helps.DeleteJSONBytes(raw, "defer_loading")
	return raw
}

func normalizeCodexFinalUpstreamToolChoice(toolChoice gjson.Result) ([]byte, bool) {
	if toolChoice.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
		case "auto", "":
			return []byte(`"auto"`), true
		case "none":
			return []byte(`"none"`), true
		case "required", "any":
			return []byte(`"required"`), true
		case "null":
			return []byte(`"auto"`), true
		}
		return []byte(`"auto"`), true
	}
	if !toolChoice.IsObject() {
		return nil, false
	}

	choiceType := strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String()))
	switch choiceType {
	case "", "null", "auto":
		return []byte(`"auto"`), true
	case "none":
		return []byte(`"none"`), true
	case "any", "required":
		return []byte(`"required"`), true
	case "tool", "function":
		name := strings.TrimSpace(toolChoice.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(toolChoice.Get("function.name").String())
		}
		if name == "" {
			return []byte(`"auto"`), true
		}
		raw := []byte(`{"type":"function"}`)
		raw, _ = helps.SetJSONBytes(raw, "name", name)
		return raw, true
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_20250305", "web_search_20260209":
		return []byte(`{"type":"web_search"}`), true
	case "image_generation":
		return []byte(`{"type":"image_generation"}`), true
	case "allowed_tools":
		raw := []byte(toolChoice.Raw)
		tools := toolChoice.Get("tools")
		if tools.IsArray() {
			if rawTools, ok := normalizeCodexFinalUpstreamToolsArray(tools); ok {
				raw, _ = helps.SetRawJSONBytes(raw, "tools", rawTools)
			}
		}
		return raw, true
	default:
		return []byte(`"auto"`), true
	}
}

func normalizeCodexFinalUpstreamToolType(toolType string) string {
	switch strings.ToLower(strings.TrimSpace(toolType)) {
	case "", "none", "null":
		return ""
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_20250305", "web_search_20260209":
		return "web_search"
	default:
		return strings.ToLower(strings.TrimSpace(toolType))
	}
}

func codexRawJSONArray(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var buf bytes.Buffer
	buf.Grow(len(items) * 32)
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(bytes.TrimSpace(item))
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func codexDefaultInstructionsFromBody(body []byte) string {
	if instructions := collectCodexInputInstructionText(gjson.GetBytes(body, "input")); instructions != "" {
		return instructions
	}
	return "You are a helpful assistant."
}

func collectCodexInputInstructionText(input gjson.Result) string {
	if !input.IsArray() {
		return ""
	}

	var parts []string
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "message" {
			return true
		}
		role := item.Get("role").String()
		if role != "developer" && role != "system" {
			return true
		}
		appendCodexInstructionContentText(&parts, item.Get("content"))
		return true
	})
	return strings.Join(parts, "\n\n")
}

func appendCodexInstructionContentText(parts *[]string, content gjson.Result) {
	appendText := func(text string) {
		text = strings.TrimSpace(text)
		if text != "" {
			*parts = append(*parts, text)
		}
	}

	if content.Type == gjson.String {
		appendText(content.String())
		return
	}
	if !content.IsArray() {
		return
	}
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "input_text", "text":
			appendText(part.Get("text").String())
		}
		return true
	})
}
