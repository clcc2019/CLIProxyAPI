package claude

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodex_SystemMessageScenarios(t *testing.T) {
	tests := []struct {
		name             string
		inputJSON        string
		wantInstructions string
		wantHasDeveloper bool
		wantTexts        []string
	}{
		{
			name: "No system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantInstructions: "You are a helpful assistant.",
		},
		{
			name: "Empty string system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantInstructions: "You are a helpful assistant.",
		},
		{
			name: "String system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "Be helpful",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantInstructions: "Be helpful",
		},
		{
			name: "System role in messages",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [
					{"role": "system", "content": "Follow the project instructions"},
					{"role": "user", "content": "hello"}
				]
			}`,
			wantHasDeveloper: true,
			wantTexts:        []string{"Follow the project instructions"},
		},
		{
			name: "Array system field with filtered billing header",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": [
					{"type": "text", "text": "x-anthropic-billing-header: tenant-123"},
					{"type": "text", "text": "Block 1"},
					{"type": "text", "text": "Block 2"}
				],
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantInstructions: "Block 1\n\nBlock 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
			resultJSON := gjson.ParseBytes(result)

			if tt.wantInstructions != "" && resultJSON.Get("instructions").String() != tt.wantInstructions {
				got := resultJSON.Get("instructions").String()
				t.Fatalf("instructions = %q, want %q. Output: %s", got, tt.wantInstructions, string(result))
			}
			inputs := resultJSON.Get("input").Array()
			hasDeveloper := false
			var developerTexts []string
			for _, input := range inputs {
				if input.Get("role").String() != "developer" {
					continue
				}
				hasDeveloper = true
				for _, content := range input.Get("content").Array() {
					if text := content.Get("text").String(); text != "" {
						developerTexts = append(developerTexts, text)
					}
				}
			}
			if hasDeveloper != tt.wantHasDeveloper {
				t.Fatalf("developer input presence = %t, want %t. Output: %s", hasDeveloper, tt.wantHasDeveloper, resultJSON.Get("input").Raw)
			}
			for _, wantText := range tt.wantTexts {
				found := false
				for _, gotText := range developerTexts {
					if gotText == wantText {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("developer texts = %#v, missing %q. Output: %s", developerTexts, wantText, resultJSON.Get("input").Raw)
				}
			}
			if !tt.wantHasDeveloper && len(inputs) > 0 && inputs[0].Get("role").String() == "developer" {
				t.Fatalf("system instructions should not be duplicated as a developer input. Output: %s", resultJSON.Get("input").Raw)
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_ParallelToolCalls(t *testing.T) {
	tests := []struct {
		name                  string
		inputJSON             string
		wantParallelToolCalls bool
	}{
		{
			name: "Default to true when tool_choice.disable_parallel_tool_use is absent",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: true,
		},
		{
			name: "Disable parallel tool calls when client opts out",
			inputJSON: `{
				"model": "claude-3-opus",
				"tool_choice": {"disable_parallel_tool_use": true},
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: false,
		},
		{
			name: "Keep parallel tool calls enabled when client explicitly allows them",
			inputJSON: `{
				"model": "claude-3-opus",
				"tool_choice": {"disable_parallel_tool_use": false},
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
			resultJSON := gjson.ParseBytes(result)

			if got := resultJSON.Get("parallel_tool_calls").Bool(); got != tt.wantParallelToolCalls {
				t.Fatalf("parallel_tool_calls = %v, want %v. Output: %s", got, tt.wantParallelToolCalls, string(result))
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_ShortenLongToolUseIDs(t *testing.T) {
	longID := "toolu_" + strings.Repeat("a", 62)
	if len(longID) <= 64 {
		t.Fatalf("test setup error: longID length = %d, want > 64", len(longID))
	}

	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{"role": "user", "content": [{"type":"text","text":"run pwd"}]},
			{"role": "assistant", "content": [
				{"type":"tool_use","id":"` + longID + `","name":"Bash","input":{"cmd":"pwd"}}
			]},
			{"role": "user", "content": [
				{"type":"tool_result","tool_use_id":"` + longID + `","content":"ok"}
			]}
		]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	inputs := gjson.GetBytes(result, "input").Array()

	var callID string
	var outputCallID string
	for _, item := range inputs {
		switch item.Get("type").String() {
		case "function_call":
			callID = item.Get("call_id").String()
		case "function_call_output":
			outputCallID = item.Get("call_id").String()
		}
	}

	if callID == "" {
		t.Fatalf("missing function_call item. Output: %s", string(result))
	}
	if outputCallID == "" {
		t.Fatalf("missing function_call_output item. Output: %s", string(result))
	}
	if callID != outputCallID {
		t.Fatalf("call_id mismatch: function_call=%q function_call_output=%q. Output: %s", callID, outputCallID, string(result))
	}
	if len(callID) > 64 {
		t.Fatalf("call_id length = %d, want <= 64: %q", len(callID), callID)
	}
	if callID == longID {
		t.Fatalf("long call_id was not shortened: %q", callID)
	}
}

func TestConvertClaudeRequestToCodex_RestoresCodexNativeToolHistory(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{"role": "user", "content": [{"type":"text","text":"use tools"}]},
			{"role": "assistant", "content": [
				{"type":"tool_use","id":"call_patch","name":"apply_patch","input":{"input":"*** Begin Patch\n*** End Patch"}},
				{"type":"tool_use","id":"call_shell","name":"local_shell","input":{"type":"exec","command":["pwd"]}},
				{"type":"tool_use","id":"call_search","name":"tool_search","input":{"query":"calendar","limit":1}}
			]},
			{"role": "user", "content": [
				{"type":"tool_result","tool_use_id":"call_patch","content":"patched"},
				{"type":"tool_result","tool_use_id":"call_shell","content":"/tmp"},
				{"type":"tool_result","tool_use_id":"call_search","content":"{\"tools\":[{\"name\":\"calendar.create_event\"}]}"}
			]}
		]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	byCallIDAndType := map[string]gjson.Result{}
	for _, item := range gjson.GetBytes(result, "input").Array() {
		callID := item.Get("call_id").String()
		itemType := item.Get("type").String()
		if callID != "" && itemType != "" {
			byCallIDAndType[callID+"|"+itemType] = item
		}
	}

	if got := byCallIDAndType["call_patch|custom_tool_call"].Get("type").String(); got != "custom_tool_call" {
		t.Fatalf("call_patch type = %q, want custom_tool_call. Output: %s", got, string(result))
	}
	if got := byCallIDAndType["call_patch|custom_tool_call"].Get("input").String(); got != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom input = %q. Output: %s", got, string(result))
	}
	if got := byCallIDAndType["call_patch|custom_tool_call_output"].Get("type").String(); got != "custom_tool_call_output" {
		t.Fatalf("call_patch output type = %q, want custom_tool_call_output. Output: %s", got, string(result))
	}
	if got := byCallIDAndType["call_shell|local_shell_call"].Get("type").String(); got != "local_shell_call" {
		t.Fatalf("call_shell type = %q, want local_shell_call. Output: %s", got, string(result))
	}
	if got := byCallIDAndType["call_shell|local_shell_call"].Get("action.command.0").String(); got != "pwd" {
		t.Fatalf("local_shell command = %q, want pwd. Output: %s", got, string(result))
	}
	if got := byCallIDAndType["call_search|tool_search_call"].Get("type").String(); got != "tool_search_call" {
		t.Fatalf("call_search type = %q, want tool_search_call. Output: %s", got, string(result))
	}
	if got := byCallIDAndType["call_search|tool_search_call"].Get("arguments.query").String(); got != "calendar" {
		t.Fatalf("tool_search query = %q, want calendar. Output: %s", got, string(result))
	}
	if got := byCallIDAndType["call_search|tool_search_output"].Get("type").String(); got != "tool_search_output" {
		t.Fatalf("call_search output type = %q, want tool_search_output. Output: %s", got, string(result))
	}
	if got := byCallIDAndType["call_search|tool_search_output"].Get("tools.0.name").String(); got != "calendar.create_event" {
		t.Fatalf("tool_search output name = %q. Output: %s", got, string(result))
	}
}

func TestConvertClaudeRequestToCodex_ToolChoiceModeMapping(t *testing.T) {
	tests := []struct {
		name                string
		claudeToolChoice    string
		wantCodexToolChoice string
	}{
		{
			name:                "Any requires at least one tool",
			claudeToolChoice:    `{"type":"any"}`,
			wantCodexToolChoice: "required",
		},
		{
			name:                "None disables tools",
			claudeToolChoice:    `{"type":"none"}`,
			wantCodexToolChoice: "none",
		},
		{
			name:                "Auto stays auto",
			claudeToolChoice:    `{"type":"auto"}`,
			wantCodexToolChoice: "auto",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := `{
				"model": "claude-3-opus",
				"tools": [
					{"name": "lookup", "description": "Lookup", "input_schema": {"type":"object","properties":{}}}
				],
				"tool_choice": ` + tt.claudeToolChoice + `,
				"messages": [{"role": "user", "content": "hello"}]
			}`

			result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
			resultJSON := gjson.ParseBytes(result)

			if got := resultJSON.Get("tool_choice").String(); got != tt.wantCodexToolChoice {
				t.Fatalf("tool_choice = %q, want %q. Output: %s", got, tt.wantCodexToolChoice, string(result))
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_ToolChoiceSpecificFunctionUsesConvertedName(t *testing.T) {
	longName := "mcp__server_with_a_very_long_name_that_exceeds_sixty_four_characters__search"
	inputJSON := `{
		"model": "claude-3-opus",
		"tools": [
			{"name": "` + longName + `", "description": "Search", "input_schema": {"type":"object","properties":{}}}
		],
		"tool_choice": {"type":"tool","name":"` + longName + `"},
		"messages": [{"role": "user", "content": "hello"}]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if got := resultJSON.Get("tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function. Output: %s", got, string(result))
	}
	toolName := resultJSON.Get("tools.0.name").String()
	choiceName := resultJSON.Get("tool_choice.name").String()
	if choiceName != toolName {
		t.Fatalf("tool_choice.name = %q, want converted tool name %q. Output: %s", choiceName, toolName, string(result))
	}
	if choiceName == longName {
		t.Fatalf("tool_choice.name should use shortened Codex tool name. Output: %s", string(result))
	}
}

func TestConvertClaudeRequestToCodex_WebSearchToolMapping(t *testing.T) {
	inputJSON := `{
		"model": "claude-3-opus",
		"tools": [
			{
				"type": "web_search_20260209",
				"name": "web_search",
				"allowed_domains": ["example.com"],
				"blocked_domains": ["blocked.example"],
				"user_location": {
					"type": "approximate",
					"city": "Beijing",
					"country": "CN",
					"timezone": "Asia/Shanghai"
				}
			}
		],
		"tool_choice": {"type":"tool","name":"web_search"},
		"messages": [{"role": "user", "content": "hello"}]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if got := resultJSON.Get("tools.0.type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want web_search. Output: %s", got, string(result))
	}
	if got := resultJSON.Get("tools.0.filters.allowed_domains.0").String(); got != "example.com" {
		t.Fatalf("tools.0.filters.allowed_domains.0 = %q, want example.com. Output: %s", got, string(result))
	}
	if resultJSON.Get("tools.0.blocked_domains").Exists() {
		t.Fatalf("tools.0.blocked_domains should not be forwarded to Codex. Output: %s", string(result))
	}
	if got := resultJSON.Get("tools.0.user_location.city").String(); got != "Beijing" {
		t.Fatalf("tools.0.user_location.city = %q, want Beijing. Output: %s", got, string(result))
	}
	if got := resultJSON.Get("tool_choice.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.type = %q, want web_search. Output: %s", got, string(result))
	}
}

func TestConvertClaudeRequestToCodex_WebSearchToolChoiceUsesDeclaredTypedToolName(t *testing.T) {
	inputJSON := `{
		"model": "claude-opus-4-7",
		"tools": [
			{"type": "web_search_20250305", "name": "browser_search"},
			{"name": "web_search", "description": "Local search", "input_schema": {"type":"object","properties":{}}}
		],
		"tool_choice": {"type":"tool","name":"web_search"},
		"messages": [{"role": "user", "content": "hello"}]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if got := resultJSON.Get("tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function. Output: %s", got, string(result))
	}
	if got := resultJSON.Get("tool_choice.name").String(); got != "web_search" {
		t.Fatalf("tool_choice.name = %q, want web_search. Output: %s", got, string(result))
	}
}

func TestConvertClaudeRequestToCodex_AssistantThinkingSignatureToReasoningItem(t *testing.T) {
	signature := validCodexReasoningSignature()
	inputJSON := `{
		"model": "claude-3-opus",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": "visible summary must not be replayed",
						"signature": "` + signature + `"
					},
					{
						"type": "text",
						"text": "visible answer"
					}
				]
			},
			{
				"role": "user",
				"content": "continue"
			}
		]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	inputs := resultJSON.Get("input").Array()
	if len(inputs) != 3 {
		t.Fatalf("got %d input items, want 3. Output: %s", len(inputs), string(result))
	}

	reasoning := inputs[0]
	if got := reasoning.Get("type").String(); got != "reasoning" {
		t.Fatalf("first input type = %q, want reasoning. Output: %s", got, string(result))
	}
	if got := reasoning.Get("encrypted_content").String(); got != signature {
		t.Fatalf("encrypted_content = %q, want %q", got, signature)
	}
	if got := reasoning.Get("summary").Raw; got != "[]" {
		t.Fatalf("summary = %s, want []", got)
	}
	if got := reasoning.Get("content").Raw; got != "null" {
		t.Fatalf("content = %s, want null", got)
	}

	assistantMessage := inputs[1]
	if got := assistantMessage.Get("role").String(); got != "assistant" {
		t.Fatalf("second input role = %q, want assistant. Output: %s", got, string(result))
	}
	if got := assistantMessage.Get("content.0.type").String(); got != "output_text" {
		t.Fatalf("assistant content type = %q, want output_text", got)
	}
	if got := assistantMessage.Get("content.0.text").String(); got != "visible answer" {
		t.Fatalf("assistant text = %q, want visible answer", got)
	}
	if strings.Contains(string(result), "visible summary must not be replayed") {
		t.Fatalf("thinking text should not be replayed into Codex input. Output: %s", string(result))
	}
}

func TestConvertClaudeRequestToCodex_IgnoresNonCodexThinkingSignatures(t *testing.T) {
	tests := []struct {
		name      string
		inputJSON string
	}{
		{
			name: "Ignore user thinking even with Codex-shaped signature",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [
					{
						"role": "user",
						"content": [
							{
								"type": "thinking",
								"thinking": "user supplied thinking",
								"signature": "` + validCodexReasoningSignature() + `"
							},
							{
								"type": "text",
								"text": "hello"
							}
						]
					}
				]
			}`,
		},
		{
			name: "Ignore Anthropic native signature",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [
					{
						"role": "assistant",
						"content": [
							{
								"type": "thinking",
								"thinking": "anthropic thinking",
								"signature": "Eo8Canthropic-state"
							},
							{
								"type": "text",
								"text": "visible answer"
							}
						]
					}
				]
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
			if got := countRequestInputItemsByType(result, "reasoning"); got != 0 {
				t.Fatalf("got %d reasoning items, want 0. Output: %s", got, string(result))
			}
		})
	}
}

func TestConvertClaudeRequestToCodex_NormalizesAdaptiveEffort(t *testing.T) {
	inputJSON := `{
		"thinking": {"type":"adaptive"},
		"output_config": {"effort":" XHIGH "},
		"messages": [{"role":"user","content":[{"type":"text","text":"hi"}]}]
	}`

	result := ConvertClaudeRequestToCodex("test-model", []byte(inputJSON), false)
	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh. Output: %s", got, string(result))
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: " ", want: ""},
		{name: "known mixed case", value: "\tAdaptive\r\n", want: "adaptive"},
		{name: "unknown lower fallback", value: "Custom-Effort", want: "custom-effort"},
	}

	for i := range tests {
		if got := normalizeReasoningEffort(tests[i].value); got != tests[i].want {
			t.Fatalf("%s: got %q, want %q", tests[i].name, got, tests[i].want)
		}
	}
}

func BenchmarkNormalizeReasoningEffort(b *testing.B) {
	for b.Loop() {
		if got := normalizeReasoningEffort(" XHIGH "); got != "xhigh" {
			b.Fatalf("normalizeReasoningEffort() = %q", got)
		}
	}
}

func countRequestInputItemsByType(result []byte, itemType string) int {
	count := 0
	gjson.GetBytes(result, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == itemType {
			count++
		}
		return true
	})
	return count
}

func validCodexReasoningSignature() string {
	raw := make([]byte, 1+8+16+16+32)
	raw[0] = 0x80
	raw[8] = 1
	return base64.URLEncoding.EncodeToString(raw)
}
