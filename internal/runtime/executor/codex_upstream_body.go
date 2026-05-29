package executor

import (
	"bytes"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	codexcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/common"
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

const (
	codexDefaultOutputSchemaTextFormatName  = "codex_output_schema"
	codexDefaultImageGenerationOutputFormat = "png"
)

const codexDefaultToolSearchDescription = "# Tool discovery\n\nSearches over deferred tool metadata with BM25 and exposes matching tools for the next model call.\n\nYou have access to tools from the following sources:\nNone currently enabled.\nSome of the tools may not have been provided to you upfront, and you should use this tool (`tool_search`) to search for the required tools. For MCP tool discovery, always use `tool_search` instead of `list_mcp_resources` or `list_mcp_resource_templates`."

func codexDefaultNamespaceDescription(namespaceName string) string {
	return "Tools in the " + namespaceName + " namespace."
}

var codexDefaultToolSearchParametersRaw = []byte(`{"type":"object","properties":{"query":{"type":"string","description":"Search query for deferred tools."},"limit":{"type":"number","description":"Maximum number of tools to return (defaults to 8)."}},"required":["query"],"additionalProperties":false}`)

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

func codexEnsureFinalUpstreamBodyDefaults(body []byte, baseModel string, opts codexFinalUpstreamBodyOptions) []byte {
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
		switch current.Type {
		case gjson.True, gjson.False:
			return
		case gjson.String:
			switch strings.ToLower(strings.TrimSpace(current.String())) {
			case "true":
				edits = append(edits, helps.SetRawJSONEdit(field, []byte("true")))
				return
			case "false":
				edits = append(edits, helps.SetRawJSONEdit(field, []byte("false")))
				return
			}
		}
		edits = append(edits, helps.SetRawJSONEdit(field, rawValue))
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
	parallelToolCallsDefault := codexDefaultParallelToolCallsForModel(baseModel)

	switch opts.requestKind {
	case codexFinalUpstreamCompact:
		addDefault("input", []byte("[]"))
		addArrayDefault("tools")
		addBoolDefault("parallel_tool_calls", parallelToolCallsDefault)
	default:
		addDefault("input", []byte("[]"))
		addArrayDefault("tools")
		addDefault("tool_choice", []byte(`"auto"`))
		addBoolDefault("parallel_tool_calls", parallelToolCallsDefault)
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

func codexDefaultParallelToolCallsForModel(baseModel string) bool {
	if supported, ok := registry.CodexClientModelSupportsParallelToolCalls(baseModel); ok {
		return supported
	}
	return false
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

	body = codexEnsureFinalUpstreamBodyDefaults(body, baseModel, opts)
	body = normalizeCodexFinalUpstreamTools(body)
	body = normalizeCodexFinalUpstreamText(body, baseModel)
	body = normalizeCodexFinalUpstreamInputItems(body, opts)
	body = normalizeCodexFinalUpstreamModelControls(body, baseModel)

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
	body = codexEnsureResponsesContextField(body, opts.requestKind)
	body = codexEnsureReasoningEncryptedContentInclude(body, opts)
	return body
}

func codexEnsureResponsesContextField(body []byte, requestKind codexFinalUpstreamRequestKind) []byte {
	if requestKind != codexFinalUpstreamResponses || len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	if input := gjson.GetBytes(body, "input"); input.Exists() && input.Type == gjson.Null {
		if updated, err := helps.SetRawJSONBytes(body, "input", []byte("[]")); err == nil {
			return updated
		}
	}
	if codexResponsesContextFieldExists(body) {
		return body
	}
	if updated, ok := codexAppendTopLevelRawField(body, "input", []byte("[]")); ok {
		return updated
	}
	if updated, err := helps.SetRawJSONBytes(body, "input", []byte("[]")); err == nil {
		return updated
	}
	return body
}

func codexResponsesContextFieldExists(body []byte) bool {
	if input := gjson.GetBytes(body, "input"); input.Exists() && input.Type != gjson.Null {
		return true
	}
	for _, field := range []string{"previous_response_id", "prompt", "conversation_id"} {
		value := gjson.GetBytes(body, field)
		if value.Exists() && value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return true
		}
	}
	return false
}

func normalizeCodexFinalUpstreamText(body []byte, baseModel string) []byte {
	format := gjson.GetBytes(body, "text.format")
	if !format.IsObject() {
		return normalizeCodexFinalUpstreamTextVerbosity(body, baseModel)
	}
	formatType := format.Get("type")
	if formatType.Type != gjson.String || strings.TrimSpace(formatType.String()) != "json_schema" {
		return normalizeCodexFinalUpstreamTextVerbosity(body, baseModel)
	}
	schema := format.Get("schema")
	if !schema.Exists() || schema.Type == gjson.Null {
		return normalizeCodexFinalUpstreamTextVerbosity(body, baseModel)
	}
	name := format.Get("name")
	if name.Type == gjson.String && strings.TrimSpace(name.String()) != "" {
		return normalizeCodexFinalUpstreamTextVerbosity(body, baseModel)
	}
	body = helps.EditJSONBytes(body, helps.SetJSONEdit("text.format.name", codexDefaultOutputSchemaTextFormatName))
	return normalizeCodexFinalUpstreamTextVerbosity(body, baseModel)
}

func normalizeCodexFinalUpstreamInputItems(body []byte, opts codexFinalUpstreamBodyOptions) []byte {
	if opts.preservePreviousResponseID && strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return codexcommon.NormalizeResponseInputItems(body)
	}
	return codexcommon.NormalizeFullTranscriptResponseInputItems(body)
}

func normalizeCodexFinalUpstreamModelControls(body []byte, baseModel string) []byte {
	capabilities, _ := registry.CodexClientModelCapabilitiesForModel(baseModel)
	return normalizeCodexFinalUpstreamReasoning(body, capabilities)
}

func normalizeCodexFinalUpstreamReasoning(body []byte, capabilities registry.CodexClientModelCapabilities) []byte {
	reasoning := gjson.GetBytes(body, "reasoning")
	if !capabilities.SupportsReasoningSummaries {
		if reasoning.Exists() {
			return helps.EditJSONBytes(body, helps.DeleteJSONEdit("reasoning"))
		}
		return body
	}

	defaultReasoningLevel := strings.TrimSpace(capabilities.DefaultReasoningLevel)
	if !reasoning.Exists() || reasoning.Type == gjson.Null || !reasoning.IsObject() {
		rawReasoning := []byte("{}")
		if defaultReasoningLevel != "" {
			rawReasoning = []byte(`{"effort":` + strconv.Quote(defaultReasoningLevel) + `}`)
		}
		return helps.EditJSONBytes(body, helps.SetRawJSONEdit("reasoning", rawReasoning))
	}

	effort := reasoning.Get("effort")
	if defaultReasoningLevel != "" && (!effort.Exists() || effort.Type == gjson.Null || (effort.Type == gjson.String && strings.TrimSpace(effort.String()) == "")) {
		return helps.EditJSONBytes(body, helps.SetJSONEdit("reasoning.effort", defaultReasoningLevel))
	}
	return body
}

func normalizeCodexFinalUpstreamTextVerbosity(body []byte, baseModel string) []byte {
	capabilities, _ := registry.CodexClientModelCapabilitiesForModel(baseModel)
	verbosity := gjson.GetBytes(body, "text.verbosity")
	if !capabilities.SupportsVerbosity {
		if !verbosity.Exists() {
			return body
		}
		if gjson.GetBytes(body, "text.format").Exists() {
			return helps.EditJSONBytes(body, helps.DeleteJSONEdit("text.verbosity"))
		}
		return helps.EditJSONBytes(body, helps.DeleteJSONEdit("text"))
	}

	defaultVerbosity := strings.TrimSpace(capabilities.DefaultVerbosity)
	if defaultVerbosity == "" {
		return body
	}
	if !verbosity.Exists() || verbosity.Type == gjson.Null || (verbosity.Type == gjson.String && strings.TrimSpace(verbosity.String()) == "") {
		return helps.EditJSONBytes(body, helps.SetJSONEdit("text.verbosity", defaultVerbosity))
	}
	return body
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
			if value == "reasoning.encrypted_content" && required == "" {
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
	if coalescedItems, coalescedChanged := coalesceCodexFinalUpstreamNamespaceTools(items); coalescedChanged {
		items = coalescedItems
		changed = true
	}
	if !changed {
		return nil, false
	}

	return codexRawJSONArray(items), true
}

func coalesceCodexFinalUpstreamNamespaceTools(items [][]byte) ([][]byte, bool) {
	if len(items) == 0 {
		return items, false
	}

	merged := make([][]byte, 0, len(items))
	namespaceIndices := make(map[string]int)
	changed := false
	for _, rawItem := range items {
		tool := gjson.ParseBytes(rawItem)
		if !tool.IsObject() || tool.Get("type").String() != "namespace" {
			merged = append(merged, rawItem)
			continue
		}

		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			merged = append(merged, rawItem)
			continue
		}

		if existingIndex, ok := namespaceIndices[name]; ok {
			existingRaw := merged[existingIndex]
			existing := gjson.ParseBytes(existingRaw)
			updated := existingRaw
			if strings.TrimSpace(existing.Get("description").String()) == "" {
				if description := tool.Get("description"); description.Type == gjson.String && strings.TrimSpace(description.String()) != "" {
					updated = codexSetJSONStringIfDifferent(updated, gjson.GetBytes(updated, "description"), "description", description.String())
				}
			}
			existingTools := codexFinalUpstreamNamespaceToolItems(gjson.GetBytes(updated, "tools"))
			nextTools := codexFinalUpstreamNamespaceToolItems(tool.Get("tools"))
			existingTools = append(existingTools, nextTools...)
			updated, _ = helps.SetRawJSONBytes(updated, "tools", codexRawJSONArray(existingTools))
			merged[existingIndex] = updated
			changed = true
			continue
		}

		namespaceIndices[name] = len(merged)
		merged = append(merged, rawItem)
	}

	for index, rawItem := range merged {
		tool := gjson.ParseBytes(rawItem)
		if !tool.IsObject() || tool.Get("type").String() != "namespace" {
			continue
		}
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			continue
		}

		updated := rawItem
		description := gjson.GetBytes(updated, "description")
		if description.Type != gjson.String || strings.TrimSpace(description.String()) == "" {
			updated = codexSetJSONStringIfDifferent(updated, description, "description", codexDefaultNamespaceDescription(name))
		}

		childTools := codexFinalUpstreamNamespaceToolItems(gjson.GetBytes(updated, "tools"))
		if len(childTools) > 1 {
			before := codexRawJSONArray(childTools)
			sort.SliceStable(childTools, func(i, j int) bool {
				return strings.Compare(codexFinalUpstreamToolName(childTools[i]), codexFinalUpstreamToolName(childTools[j])) < 0
			})
			after := codexRawJSONArray(childTools)
			if !codexRawJSONEqual(before, after) {
				updated, _ = helps.SetRawJSONBytes(updated, "tools", after)
			}
		}

		if !codexRawJSONEqual(updated, rawItem) {
			merged[index] = updated
			changed = true
		}
	}

	return merged, changed
}

func codexFinalUpstreamNamespaceToolItems(tools gjson.Result) [][]byte {
	items := make([][]byte, 0)
	if !tools.IsArray() {
		return items
	}
	tools.ForEach(func(_, tool gjson.Result) bool {
		items = append(items, []byte(tool.Raw))
		return true
	})
	return items
}

func codexFinalUpstreamToolName(rawTool []byte) string {
	return strings.TrimSpace(gjson.GetBytes(rawTool, "name").String())
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
		return normalizeCodexFinalUpstreamToolSearchTool(tool), true
	case "custom":
		return normalizeCodexFinalUpstreamCustomTool(tool)
	case "web_search":
		return normalizeCodexFinalUpstreamWebSearchTool(tool), true
	case "image_generation":
		return normalizeCodexFinalUpstreamImageGenerationTool(tool), true
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
	if description := tool.Get("description"); description.Type != gjson.String {
		raw = codexSetJSONStringIfDifferent(raw, description, "description", "")
	}
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
	raw = codexNormalizeFunctionToolDeferLoading(raw)
	raw = codexDeleteJSONIfExists(raw, tool.Get("input_schema"), "input_schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("output_schema"), "output_schema")
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
		if rawTools, changed := normalizeCodexFinalUpstreamNamespaceToolsArray(tools); changed {
			raw, _ = helps.SetRawJSONBytes(raw, "tools", rawTools)
		}
	} else {
		raw = codexSetRawJSONIfDifferent(raw, tools, "tools", []byte("[]"))
	}
	raw = codexDeleteJSONIfExists(raw, tool.Get("input_schema"), "input_schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("output_schema"), "output_schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("parameters"), "parameters")
	raw = codexDeleteJSONIfExists(raw, tool.Get("strict"), "strict")
	raw = codexDeleteJSONIfExists(raw, tool.Get("defer_loading"), "defer_loading")
	raw = codexDeleteJSONIfExists(raw, tool.Get("cache_control"), "cache_control")
	return raw, true
}

func normalizeCodexFinalUpstreamNamespaceToolsArray(tools gjson.Result) ([]byte, bool) {
	items := make([][]byte, 0)
	changed := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		if !tool.IsObject() {
			changed = true
			return true
		}
		toolType := normalizeCodexFinalUpstreamToolType(tool.Get("type").String())
		if toolType == "" {
			if strings.TrimSpace(tool.Get("name").String()) == "" && !tool.Get("input_schema").Exists() && !tool.Get("parameters").Exists() {
				changed = true
				return true
			}
			toolType = "function"
		}
		if toolType != "function" {
			changed = true
			return true
		}
		rawTool, keep := normalizeCodexFinalUpstreamFunctionTool(tool)
		if !keep {
			changed = true
			return true
		}
		if !codexRawJSONEqual(rawTool, []byte(tool.Raw)) {
			changed = true
		}
		items = append(items, rawTool)
		return true
	})
	if !changed {
		return nil, false
	}
	return codexRawJSONArray(items), true
}

func codexNormalizeFunctionToolDeferLoading(raw []byte) []byte {
	deferLoading := gjson.GetBytes(raw, "defer_loading")
	if !deferLoading.Exists() {
		return raw
	}
	if deferLoading.Type == gjson.True {
		return raw
	}
	if deferLoading.Type == gjson.String && strings.EqualFold(strings.TrimSpace(deferLoading.String()), "true") {
		return codexSetRawJSONIfDifferent(raw, deferLoading, "defer_loading", []byte("true"))
	}
	return codexDeleteJSONIfExists(raw, deferLoading, "defer_loading")
}

func normalizeCodexFinalUpstreamTypedTool(tool gjson.Result, toolType string) []byte {
	raw := []byte(tool.Raw)
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("type"), "type", toolType)
	raw = codexDeleteJSONIfExists(raw, tool.Get("output_schema"), "output_schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("cache_control"), "cache_control")
	return raw
}

func normalizeCodexFinalUpstreamToolSearchTool(tool gjson.Result) []byte {
	raw := normalizeCodexFinalUpstreamTypedTool(tool, "tool_search")
	if execution := gjson.GetBytes(raw, "execution"); execution.Type != gjson.String || strings.TrimSpace(execution.String()) == "" {
		raw = codexSetJSONStringIfDifferent(raw, execution, "execution", "client")
	}
	if description := gjson.GetBytes(raw, "description"); description.Type != gjson.String || strings.TrimSpace(description.String()) == "" {
		raw = codexSetJSONStringIfDifferent(raw, description, "description", codexDefaultToolSearchDescription)
	}
	if parameters := gjson.GetBytes(raw, "parameters"); !codexToolSearchParametersValid(parameters) {
		raw = codexSetRawJSONIfDifferent(raw, parameters, "parameters", codexDefaultToolSearchParametersRaw)
	}
	return raw
}

func codexToolSearchParametersValid(parameters gjson.Result) bool {
	if !parameters.IsObject() {
		return false
	}
	if parameters.Get("type").Type != gjson.String || parameters.Get("type").String() != "object" {
		return false
	}
	query := parameters.Get("properties.query")
	if !query.IsObject() || query.Get("type").String() != "string" {
		return false
	}
	limit := parameters.Get("properties.limit")
	if !limit.IsObject() || limit.Get("type").String() != "number" {
		return false
	}
	requiredHasQuery := false
	required := parameters.Get("required")
	if !required.IsArray() {
		return false
	}
	required.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String && item.String() == "query" {
			requiredHasQuery = true
			return false
		}
		return true
	})
	if !requiredHasQuery {
		return false
	}
	return parameters.Get("additionalProperties").Type == gjson.False
}

func normalizeCodexFinalUpstreamImageGenerationTool(tool gjson.Result) []byte {
	raw := normalizeCodexFinalUpstreamTypedTool(tool, "image_generation")
	if outputFormat := gjson.GetBytes(raw, "output_format"); outputFormat.Type != gjson.String || strings.TrimSpace(outputFormat.String()) == "" {
		raw = codexSetJSONStringIfDifferent(raw, outputFormat, "output_format", codexDefaultImageGenerationOutputFormat)
	}
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

func normalizeCodexFinalUpstreamCustomTool(tool gjson.Result) ([]byte, bool) {
	source := tool
	if strings.TrimSpace(tool.Get("name").String()) == "" {
		if nested := tool.Get("custom"); nested.IsObject() {
			source = nested
		}
	}

	name := strings.TrimSpace(source.Get("name").String())
	if name == "" {
		return nil, false
	}

	raw := []byte(tool.Raw)
	if source.Raw != tool.Raw {
		raw = []byte(source.Raw)
	}
	raw = codexSetJSONStringIfDifferent(raw, gjson.GetBytes(raw, "type"), "type", "custom")
	raw = codexSetJSONStringIfDifferent(raw, gjson.GetBytes(raw, "name"), "name", name)
	if description := gjson.GetBytes(raw, "description"); description.Type != gjson.String {
		raw = codexSetJSONStringIfDifferent(raw, description, "description", "")
	}
	raw = codexDeleteJSONIfExists(raw, gjson.GetBytes(raw, "custom"), "custom")
	raw = codexDeleteJSONIfExists(raw, gjson.GetBytes(raw, "input_schema"), "input_schema")
	raw = codexDeleteJSONIfExists(raw, gjson.GetBytes(raw, "output_schema"), "output_schema")
	raw = codexDeleteJSONIfExists(raw, gjson.GetBytes(raw, "parameters"), "parameters")
	raw = codexDeleteJSONIfExists(raw, gjson.GetBytes(raw, "strict"), "strict")
	raw = codexDeleteJSONIfExists(raw, gjson.GetBytes(raw, "defer_loading"), "defer_loading")
	raw = codexDeleteJSONIfExists(raw, gjson.GetBytes(raw, "cache_control"), "cache_control")
	return raw, true
}

func normalizeCodexFinalUpstreamWebSearchTool(tool gjson.Result) []byte {
	raw := []byte(tool.Raw)
	raw = codexSetJSONStringIfDifferent(raw, tool.Get("type"), "type", "web_search")
	if !tool.Get("external_web_access").Exists() {
		raw, _ = helps.SetJSONBytes(raw, "external_web_access", false)
	}
	if allowedDomains := tool.Get("allowed_domains"); allowedDomains.Exists() && allowedDomains.IsArray() && !tool.Get("filters.allowed_domains").Exists() {
		raw, _ = helps.SetRawJSONBytes(raw, "filters.allowed_domains", []byte(allowedDomains.Raw))
	}
	raw = codexDeleteJSONIfExists(raw, tool.Get("name"), "name")
	raw = codexDeleteJSONIfExists(raw, tool.Get("input_schema"), "input_schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("output_schema"), "output_schema")
	raw = codexDeleteJSONIfExists(raw, tool.Get("allowed_domains"), "allowed_domains")
	raw = codexDeleteJSONIfExists(raw, tool.Get("blocked_domains"), "blocked_domains")
	raw = codexDeleteJSONIfExists(raw, tool.Get("enabled"), "enabled")
	raw = codexDeleteJSONIfExists(raw, tool.Get("max_uses"), "max_uses")
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
			name = strings.TrimSpace(toolChoice.Get("custom.name").String())
		}
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
			name = strings.TrimSpace(tool.Get("custom.name").String())
		}
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
