package executor

import (
	"bytes"
	"strconv"
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

const codexDefaultOutputSchemaTextFormatName = "codex_output_schema"

type codexFinalUpstreamBodyOptions struct {
	requestKind                 codexFinalUpstreamRequestKind
	streamMode                  codexStreamFieldMode
	preservePreviousResponseID  bool
	preserveGenerate            bool
	store                       bool
	omitServiceTier             bool
	suppressDefaultInstructions bool
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

func codexShouldStoreResponses(auth *cliproxyauth.Auth, upstreamURL string) bool {
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "azure") {
		return true
	}
	return codexMatchesAzureResponsesBaseURL(upstreamURL)
}

func codexMatchesAzureResponsesBaseURL(rawURL string) bool {
	lower := strings.ToLower(strings.TrimSpace(rawURL))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"openai.azure.",
		"cognitiveservices.azure.",
		"aoai.azure.",
		"azure-api.",
		"azurefd.",
		"windows.net/openai",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
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
	addArrayDefault := func(field string) {
		current := gjson.GetBytes(body, field)
		if !current.Exists() {
			appendFields = append(appendFields, codexTopLevelRawField{field: field, rawValue: []byte("[]")})
			return
		}
		if !current.IsArray() {
			edits = append(edits, helps.SetRawJSONEdit(field, []byte("[]")))
		}
	}
	addBoolDefault := func(field string, value bool) {
		current := gjson.GetBytes(body, field)
		rawValue := []byte("false")
		if value {
			rawValue = []byte("true")
		}
		if !current.Exists() {
			appendFields = append(appendFields, codexTopLevelRawField{field: field, rawValue: rawValue})
			return
		}
		if current.Type != gjson.True && current.Type != gjson.False {
			edits = append(edits, helps.SetRawJSONEdit(field, rawValue))
		}
	}

	switch opts.requestKind {
	case codexFinalUpstreamCompact:
		addArrayDefault("tools")
		addBoolDefault("parallel_tool_calls", true)
	default:
		addArrayDefault("tools")
		addDefault("tool_choice", []byte(`"auto"`))
		addBoolDefault("parallel_tool_calls", true)
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
		if field == "previous_response_id" && opts.preservePreviousResponseID && opts.requestKind != codexFinalUpstreamCompact {
			return true
		}
		if field == "generate" && opts.preserveGenerate && opts.requestKind != codexFinalUpstreamCompact {
			return true
		}
		if field == "service_tier" && opts.omitServiceTier {
			edits = append(edits, helps.DeleteJSONEdit(field))
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
	for _, field := range []string{"service_tier", "prompt_cache_key"} {
		value := gjson.GetBytes(body, field)
		if value.Exists() && (value.Type != gjson.String || strings.TrimSpace(value.String()) == "") {
			edits = append(edits, helps.DeleteJSONEdit(field))
		}
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
	body = normalizeCodexFinalUpstreamText(body)

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
		storeValue := []byte("false")
		if opts.store {
			storeValue = []byte("true")
		}
		if !store.Exists() {
			if updated, ok := codexAppendTopLevelRawField(body, "store", storeValue); ok {
				body = updated
			} else {
				edits = append(edits, helps.SetRawJSONEdit("store", storeValue))
			}
		} else if (!opts.store && store.Type != gjson.False) || (opts.store && store.Type != gjson.True) {
			edits = append(edits, helps.SetRawJSONEdit("store", storeValue))
		}
	}
	if !opts.suppressDefaultInstructions && (!instructions.Exists() || instructions.Type == gjson.Null || (instructions.Type == gjson.String && strings.TrimSpace(instructions.String()) == "")) {
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
	body = codexEnsureReasoningEncryptedContentInclude(body, opts)
	return body
}

func normalizeCodexFinalUpstreamText(body []byte) []byte {
	format := gjson.GetBytes(body, "text.format")
	if !format.IsObject() {
		return body
	}
	formatType := format.Get("type")
	if formatType.Type != gjson.String || strings.TrimSpace(formatType.String()) != "json_schema" {
		return body
	}
	schema := format.Get("schema")
	if !schema.Exists() || schema.Type == gjson.Null {
		return body
	}
	name := format.Get("name")
	if name.Type == gjson.String && strings.TrimSpace(name.String()) != "" {
		return body
	}
	return helps.EditJSONBytes(body, helps.SetJSONEdit("text.format.name", codexDefaultOutputSchemaTextFormatName))
}

func codexEnsureReasoningEncryptedContentInclude(body []byte, opts codexFinalUpstreamBodyOptions) []byte {
	if opts.requestKind != codexFinalUpstreamResponses {
		return body
	}
	requiredInclude := ""
	reasoning := gjson.GetBytes(body, "reasoning")
	if reasoning.Exists() && reasoning.Type != gjson.Null {
		requiredInclude = "reasoning.encrypted_content"
	}

	include := gjson.GetBytes(body, "include")
	rawInclude, changed := codexFinalIncludeStringsRaw(include, requiredInclude)
	if !changed {
		return body
	}
	if updated, err := helps.SetRawJSONBytes(body, "include", rawInclude); err == nil {
		return updated
	}
	return body
}

func codexFinalIncludeStringsRaw(include gjson.Result, required string) ([]byte, bool) {
	required = strings.TrimSpace(required)
	if !include.Exists() && required == "" {
		return []byte("[]"), false
	}

	seen := make(map[string]struct{})
	items := make([][]byte, 0, 2)
	changed := false
	if include.Exists() && include.IsArray() {
		include.ForEach(func(_, item gjson.Result) bool {
			if item.Type != gjson.String {
				changed = true
				return true
			}
			value := strings.TrimSpace(item.String())
			if value == "" {
				changed = true
				return true
			}
			if value != item.String() {
				changed = true
			}
			if _, ok := seen[value]; ok {
				changed = true
				return true
			}
			seen[value] = struct{}{}
			items = append(items, strconv.AppendQuote(nil, value))
			return true
		})
	} else if include.Exists() {
		changed = true
	}

	if required != "" {
		if _, ok := seen[required]; !ok {
			seen[required] = struct{}{}
			items = append(items, strconv.AppendQuote(nil, required))
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	return codexRawJSONArray(items), true
}

func normalizeCodexFinalUpstreamTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		if rawTools, changed := normalizeCodexFinalUpstreamToolsArray(tools); changed {
			if updated, err := helps.SetRawJSONBytes(body, "tools", rawTools); err == nil {
				body = updated
			}
		}
	}

	toolChoice := gjson.GetBytes(body, "tool_choice")
	if toolChoice.Exists() {
		if rawChoice, ok := normalizeCodexFinalUpstreamToolChoice(toolChoice); ok && !codexRawJSONEqual(rawChoice, []byte(toolChoice.Raw)) {
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

	items := make([][]byte, 0)
	changed := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		if rawTool, keep := normalizeCodexFinalUpstreamTool(tool); keep {
			if !codexRawJSONEqual(rawTool, []byte(tool.Raw)) {
				changed = true
			}
			items = append(items, rawTool)
		} else {
			changed = true
		}
		return true
	})
	if !changed {
		return nil, false
	}

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
	case "namespace":
		return normalizeCodexFinalUpstreamNamespaceTool(tool)
	case "tool_search":
		return normalizeCodexFinalUpstreamTypedTool(tool, "tool_search"), true
	case "custom":
		return normalizeCodexFinalUpstreamNamedTypedTool(tool, "custom")
	case "web_search":
		return normalizeCodexFinalUpstreamWebSearchTool(tool), true
	case "image_generation":
		return normalizeCodexFinalUpstreamTypedTool(tool, "image_generation"), true
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
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("type"), "type", "function")
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("name"), "name", name)
	parameters := tool.Get("parameters")
	if !parameters.Exists() || parameters.Type == gjson.Null {
		if inputSchema := tool.Get("input_schema"); inputSchema.Exists() && inputSchema.Type != gjson.Null {
			raw = codexSetRawJSONIfDifferent(raw, parameters, "parameters", []byte(inputSchema.Raw))
		} else {
			raw = codexSetRawJSONIfDifferent(raw, parameters, "parameters", []byte(`{"type":"object","properties":{}}`))
		}
	}
	if strict := tool.Get("strict"); !strict.Exists() || strict.Type == gjson.Null {
		raw = codexSetRawJSONIfDifferent(raw, strict, "strict", []byte("false"))
	}
	raw = codexDeleteJSONIfExists(raw, tool.Get("input_schema"), "input_schema")
	raw = codexDeleteJSONIfExists(raw, gjson.GetBytes(raw, "parameters.$schema"), "parameters.$schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("cache_control"), "cache_control")
	return raw, true
}

func normalizeCodexFinalUpstreamNamespaceTool(tool gjson.Result) ([]byte, bool) {
	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		return nil, false
	}

	raw := []byte(tool.Raw)
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("type"), "type", "namespace")
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("name"), "name", name)
	if tools := tool.Get("tools"); tools.IsArray() {
		if rawTools, changed := normalizeCodexFinalUpstreamToolsArray(tools); changed {
			raw, _ = helps.SetRawJSONBytes(raw, "tools", rawTools)
		}
	}
	raw = codexDeleteJSONIfExists(raw, tool.Get("input_schema"), "input_schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("parameters"), "parameters")
	raw = codexDeleteJSONIfExists(raw, tool.Get("cache_control"), "cache_control")
	return raw, true
}

func normalizeCodexFinalUpstreamTypedTool(tool gjson.Result, toolType string) []byte {
	raw := []byte(tool.Raw)
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("type"), "type", toolType)
	raw = codexDeleteJSONIfExists(raw, tool.Get("cache_control"), "cache_control")
	return raw
}

func normalizeCodexFinalUpstreamNamedTypedTool(tool gjson.Result, toolType string) ([]byte, bool) {
	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		return nil, false
	}
	raw := normalizeCodexFinalUpstreamTypedTool(tool, toolType)
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("name"), "name", name)
	return raw, true
}

func normalizeCodexFinalUpstreamWebSearchTool(tool gjson.Result) []byte {
	raw := []byte(tool.Raw)
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("type"), "type", "web_search")
	if allowedDomains := tool.Get("allowed_domains"); allowedDomains.Exists() && allowedDomains.IsArray() && !tool.Get("filters.allowed_domains").Exists() {
		raw, _ = helps.SetRawJSONBytes(raw, "filters.allowed_domains", []byte(allowedDomains.Raw))
	}
	raw = codexDeleteJSONIfExists(raw, tool.Get("name"), "name")
	raw = codexDeleteJSONIfExists(raw, tool.Get("input_schema"), "input_schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("allowed_domains"), "allowed_domains")
	raw = codexDeleteJSONIfExists(raw, tool.Get("blocked_domains"), "blocked_domains")
	raw = codexDeleteJSONIfExists(raw, tool.Get("cache_control"), "cache_control")
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
	case "custom":
		name := strings.TrimSpace(toolChoice.Get("name").String())
		if name == "" {
			return []byte(`"auto"`), true
		}
		raw := []byte(`{"type":"custom"}`)
		raw, _ = helps.SetJSONBytes(raw, "name", name)
		return raw, true
	case "allowed_tools":
		return normalizeCodexFinalUpstreamAllowedToolsChoice(toolChoice), true
	default:
		return []byte(`"auto"`), true
	}
}

func normalizeCodexFinalUpstreamAllowedToolsChoice(toolChoice gjson.Result) []byte {
	mode := strings.ToLower(strings.TrimSpace(toolChoice.Get("mode").String()))
	switch mode {
	case "required", "any":
		mode = "required"
	default:
		mode = "auto"
	}

	rawTools := []byte("[]")
	if tools := toolChoice.Get("tools"); tools.IsArray() {
		rawTools = normalizeCodexFinalUpstreamAllowedToolRefsArray(tools)
	}

	raw := []byte(`{"type":"allowed_tools"}`)
	raw, _ = helps.SetJSONBytes(raw, "mode", mode)
	raw, _ = helps.SetRawJSONBytes(raw, "tools", rawTools)
	return raw
}

func normalizeCodexFinalUpstreamAllowedToolRefsArray(tools gjson.Result) []byte {
	items := make([][]byte, 0)
	tools.ForEach(func(_, tool gjson.Result) bool {
		if rawTool, keep := normalizeCodexFinalUpstreamAllowedToolRef(tool); keep {
			items = append(items, rawTool)
		}
		return true
	})
	return codexRawJSONArray(items)
}

func normalizeCodexFinalUpstreamAllowedToolRef(tool gjson.Result) ([]byte, bool) {
	if !tool.IsObject() {
		return nil, false
	}

	toolType := normalizeCodexFinalUpstreamToolType(tool.Get("type").String())
	if toolType == "" || toolType == "function" || toolType == "tool" {
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(tool.Get("function.name").String())
		}
		if name == "" {
			return nil, false
		}
		raw := []byte(`{"type":"function"}`)
		raw, _ = helps.SetJSONBytes(raw, "name", name)
		return raw, true
	}

	switch toolType {
	case "web_search":
		return []byte(`{"type":"web_search"}`), true
	case "image_generation":
		return []byte(`{"type":"image_generation"}`), true
	case "tool_search":
		return []byte(`{"type":"tool_search"}`), true
	case "custom":
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			return nil, false
		}
		raw := []byte(`{"type":"custom"}`)
		raw, _ = helps.SetJSONBytes(raw, "name", name)
		return raw, true
	case "mcp":
		serverLabel := strings.TrimSpace(tool.Get("server_label").String())
		if serverLabel == "" {
			return nil, false
		}
		raw := []byte(`{"type":"mcp"}`)
		raw, _ = helps.SetJSONBytes(raw, "server_label", serverLabel)
		if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
			raw, _ = helps.SetJSONBytes(raw, "name", name)
		}
		return raw, true
	case "namespace":
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			return nil, false
		}
		raw := []byte(`{"type":"namespace"}`)
		raw, _ = helps.SetJSONBytes(raw, "name", name)
		return raw, true
	case "file_search", "computer", "computer_use", "computer_use_preview", "code_interpreter", "shell", "apply_patch":
		raw := []byte(`{}`)
		raw, _ = helps.SetJSONBytes(raw, "type", toolType)
		return raw, true
	default:
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			return nil, false
		}
		raw := []byte(`{"type":"function"}`)
		raw, _ = helps.SetJSONBytes(raw, "name", name)
		return raw, true
	}
}

func normalizeCodexFinalUpstreamToolType(toolType string) string {
	switch strings.ToLower(strings.TrimSpace(toolType)) {
	case "", "none", "null":
		return ""
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_20250305", "web_search_20260209":
		return "web_search"
	case "namespace", "tool_search", "custom", "image_generation", "function":
		return strings.ToLower(strings.TrimSpace(toolType))
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

func codexRawJSONEqual(left, right []byte) bool {
	return bytes.Equal(bytes.TrimSpace(left), bytes.TrimSpace(right))
}

func codexSetJSONStringIfDifferent(raw []byte, current gjson.Result, path string, value string) []byte {
	if current.Exists() && current.Type == gjson.String && current.String() == value {
		return raw
	}
	updated, err := helps.SetJSONBytes(raw, path, value)
	if err != nil {
		return raw
	}
	return updated
}

func codexSetRawJSONIfDifferent(raw []byte, current gjson.Result, path string, value []byte) []byte {
	if current.Exists() && codexRawJSONEqual([]byte(current.Raw), value) {
		return raw
	}
	updated, err := helps.SetRawJSONBytes(raw, path, value)
	if err != nil {
		return raw
	}
	return updated
}

func codexDeleteJSONIfExists(raw []byte, current gjson.Result, path string) []byte {
	if !current.Exists() {
		return raw
	}
	updated, err := helps.DeleteJSONBytes(raw, path)
	if err != nil {
		return raw
	}
	return updated
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
