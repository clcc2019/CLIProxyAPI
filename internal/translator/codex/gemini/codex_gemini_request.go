// Package gemini provides request translation functionality for Codex to Gemini API compatibility.
// It handles parsing and transforming Codex API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Codex API format and Gemini API's expected format.
package gemini

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	codexcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToCodex parses and transforms a Gemini API request into Codex API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Codex API.
// The function performs comprehensive transformation including:
// 1. Model name mapping and generation configuration extraction
// 2. System instruction conversion to Codex format
// 3. Message content conversion with proper role mapping
// 4. Tool call and tool result handling with FIFO queue for ID matching
// 5. Tool declaration and tool choice configuration mapping
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Gemini API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Codex API format
func ConvertGeminiRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	// Base template
	out := []byte(`{"model":"","instructions":"","input":[]}`)

	root := gjson.ParseBytes(rawJSON)

	// Pre-compute tool name shortening map from declared functionDeclarations
	shortMap := map[string]string{}
	if tools := root.Get("tools"); tools.IsArray() {
		var names []string
		tarr := tools.Array()
		for i := 0; i < len(tarr); i++ {
			fns := tarr[i].Get("functionDeclarations")
			if !fns.IsArray() {
				continue
			}
			for _, fn := range fns.Array() {
				if v := fn.Get("name"); v.Exists() {
					names = append(names, v.String())
				}
			}
		}
		if len(names) > 0 {
			shortMap = buildShortNameMap(names)
		}
	}

	// helper for generating paired call IDs in the form: call_<alphanum>
	// Gemini uses sequential pairing across possibly multiple in-flight
	// functionCalls, so we keep a FIFO queue of generated call IDs and
	// consume them in order when functionResponses arrive.
	var pendingCalls []codexGeminiPendingToolCall

	// genCallID creates a random call id like: call_<8chars>
	genCallID := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		var b strings.Builder
		// 8 chars random suffix
		for i := 0; i < 24; i++ {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			b.WriteByte(letters[n.Int64()])
		}
		return "call_" + b.String()
	}

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// System instruction -> as a user message with input_text parts
	sysParts := root.Get("system_instruction.parts")
	if sysParts.IsArray() {
		msg := []byte(`{"type":"message","role":"developer","content":[]}`)
		arr := sysParts.Array()
		for i := 0; i < len(arr); i++ {
			p := arr[i]
			if t := p.Get("text"); t.Exists() {
				part := []byte(`{}`)
				part, _ = sjson.SetBytes(part, "type", "input_text")
				part, _ = sjson.SetBytes(part, "text", t.String())
				msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
			}
		}
		if len(gjson.GetBytes(msg, "content").Array()) > 0 {
			out, _ = sjson.SetRawBytes(out, "input.-1", msg)
		}
	}

	// Contents -> messages and function calls/results
	contents := root.Get("contents")
	if contents.IsArray() {
		items := contents.Array()
		for i := 0; i < len(items); i++ {
			item := items[i]
			role := item.Get("role").String()
			if role == "model" {
				role = "assistant"
			}

			parts := item.Get("parts")
			if !parts.IsArray() {
				continue
			}
			parr := parts.Array()
			for j := 0; j < len(parr); j++ {
				p := parr[j]
				// text part
				if t := p.Get("text"); t.Exists() {
					msg := []byte(`{"type":"message","role":"","content":[]}`)
					msg, _ = sjson.SetBytes(msg, "role", role)
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := []byte(`{}`)
					part, _ = sjson.SetBytes(part, "type", partType)
					part, _ = sjson.SetBytes(part, "text", t.String())
					msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
					out, _ = sjson.SetRawBytes(out, "input.-1", msg)
					continue
				}

				// function call from model
				if fc := p.Get("functionCall"); fc.Exists() {
					// generate a paired random call_id and enqueue it so the
					// corresponding functionResponse can pop the earliest id
					// to preserve ordering when multiple calls are present.
					id := genCallID()
					kind := codexGeminiToolCallKindForName(fc.Get("name").String(), fc.Get("args"))
					pendingCalls = append(pendingCalls, codexGeminiPendingToolCall{CallID: id, Kind: kind})
					out, _ = sjson.SetRawBytes(out, "input.-1", codexGeminiFunctionCallToInputItem(fc, id, kind, shortMap))
					continue
				}

				// function response from user
				if fr := p.Get("functionResponse"); fr.Exists() {
					// attach the oldest queued call_id to pair the response
					// with its call. If the queue is empty, generate a new id.
					pending := codexGeminiPendingToolCall{}
					if len(pendingCalls) > 0 {
						pending = pendingCalls[0]
						// pop the first element
						pendingCalls = pendingCalls[1:]
					} else {
						pending = codexGeminiPendingToolCall{
							CallID: genCallID(),
							Kind:   codexGeminiToolCallKindForName(fr.Get("name").String(), gjson.Result{}),
						}
					}
					out, _ = sjson.SetRawBytes(out, "input.-1", codexGeminiFunctionResponseToInputItem(fr, pending))
					continue
				}
			}
		}
	}

	// Tools mapping: Gemini functionDeclarations -> Codex tools
	tools := root.Get("tools")
	if tools.IsArray() {
		out, _ = sjson.SetRawBytes(out, "tools", []byte(`[]`))
		out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		tarr := tools.Array()
		for i := 0; i < len(tarr); i++ {
			td := tarr[i]
			fns := td.Get("functionDeclarations")
			if !fns.IsArray() {
				continue
			}
			farr := fns.Array()
			for j := 0; j < len(farr); j++ {
				fn := farr[j]
				tool := []byte(`{}`)
				tool, _ = sjson.SetBytes(tool, "type", "function")
				if v := fn.Get("name"); v.Exists() {
					name := v.String()
					if short, ok := shortMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
					tool, _ = sjson.SetBytes(tool, "name", name)
				}
				if v := fn.Get("description"); v.Exists() {
					tool, _ = sjson.SetBytes(tool, "description", v.String())
				}
				if prm := fn.Get("parameters"); prm.Exists() {
					// Remove optional $schema field if present
					cleaned := []byte(prm.Raw)
					cleaned, _ = sjson.DeleteBytes(cleaned, "$schema")
					cleaned, _ = sjson.SetBytes(cleaned, "additionalProperties", false)
					tool, _ = sjson.SetRawBytes(tool, "parameters", cleaned)
				} else if prm = fn.Get("parametersJsonSchema"); prm.Exists() {
					// Remove optional $schema field if present
					cleaned := []byte(prm.Raw)
					cleaned, _ = sjson.DeleteBytes(cleaned, "$schema")
					cleaned, _ = sjson.SetBytes(cleaned, "additionalProperties", false)
					tool, _ = sjson.SetRawBytes(tool, "parameters", cleaned)
				}
				tool, _ = sjson.SetBytes(tool, "strict", false)
				out, _ = sjson.SetRawBytes(out, "tools.-1", tool)
			}
		}
	}

	// Fixed flags aligning with Codex expectations
	out, _ = sjson.SetBytes(out, "parallel_tool_calls", true)

	// Convert Gemini thinkingConfig to Codex reasoning.effort.
	// Note: Google official Python SDK sends snake_case fields (thinking_level/thinking_budget).
	effortSet := false
	if genConfig := root.Get("generationConfig"); genConfig.Exists() {
		if thinkingConfig := genConfig.Get("thinkingConfig"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
			thinkingLevel := thinkingConfig.Get("thinkingLevel")
			if !thinkingLevel.Exists() {
				thinkingLevel = thinkingConfig.Get("thinking_level")
			}
			if thinkingLevel.Exists() {
				effort := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
					effortSet = true
				}
			} else {
				thinkingBudget := thinkingConfig.Get("thinkingBudget")
				if !thinkingBudget.Exists() {
					thinkingBudget = thinkingConfig.Get("thinking_budget")
				}
				if thinkingBudget.Exists() {
					if effort, ok := thinking.ConvertBudgetToLevel(int(thinkingBudget.Int())); ok {
						out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
						effortSet = true
					}
				}
			}
		}
	}
	if !effortSet {
		// No thinking config, set default effort
		out, _ = sjson.SetBytes(out, "reasoning.effort", "medium")
	}
	out, _ = sjson.SetBytes(out, "reasoning.summary", "auto")
	out, _ = sjson.SetBytes(out, "stream", true)
	out, _ = sjson.SetBytes(out, "store", false)
	out, _ = sjson.SetBytes(out, "include", []string{"reasoning.encrypted_content"})

	var pathsToLower []string
	toolsResult := gjson.GetBytes(out, "tools")
	util.Walk(toolsResult, "", "type", &pathsToLower)
	for _, p := range pathsToLower {
		fullPath := fmt.Sprintf("tools.%s", p)
		typeValue := gjson.GetBytes(out, fullPath)
		if typeValue.Type != gjson.String {
			continue
		}
		out, _ = sjson.SetBytes(out, fullPath, strings.ToLower(typeValue.String()))
	}

	return out
}

type codexGeminiToolCallKind struct {
	ItemType string
	Name     string
}

type codexGeminiPendingToolCall struct {
	CallID string
	Kind   codexGeminiToolCallKind
}

func codexGeminiToolCallKindForName(name string, args gjson.Result) codexGeminiToolCallKind {
	switch name {
	case "local_shell":
		return codexGeminiToolCallKind{ItemType: "local_shell_call", Name: name}
	case "tool_search":
		return codexGeminiToolCallKind{ItemType: "tool_search_call", Name: name}
	case "apply_patch":
		return codexGeminiToolCallKind{ItemType: "custom_tool_call", Name: name}
	}
	if args.IsObject() {
		fields := args.Map()
		if len(fields) == 1 && args.Get("input").Type == gjson.String {
			return codexGeminiToolCallKind{ItemType: "custom_tool_call", Name: name}
		}
	}
	return codexGeminiToolCallKind{ItemType: "function_call", Name: name}
}

func codexGeminiFunctionCallToInputItem(functionCall gjson.Result, callID string, kind codexGeminiToolCallKind, shortMap map[string]string) []byte {
	args := functionCall.Get("args")
	switch kind.ItemType {
	case "custom_tool_call":
		item := []byte(`{"type":"custom_tool_call"}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		item, _ = sjson.SetBytes(item, "name", kind.Name)
		if payload := args.Get("input"); payload.Exists() {
			item, _ = sjson.SetBytes(item, "input", payload.String())
		} else {
			item, _ = sjson.SetBytes(item, "input", args.Raw)
		}
		return item
	case "local_shell_call":
		item := []byte(`{"type":"local_shell_call","status":"completed","action":{}}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		if args.IsObject() {
			item, _ = sjson.SetRawBytes(item, "action", []byte(args.Raw))
		}
		return item
	case "tool_search_call":
		item := []byte(`{"type":"tool_search_call","status":"completed","execution":"client","arguments":{}}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		if args.IsObject() {
			item, _ = sjson.SetRawBytes(item, "arguments", []byte(args.Raw))
		}
		return item
	default:
		item := []byte(`{"type":"function_call"}`)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		name := functionCall.Get("name").String()
		if short, ok := shortMap[name]; ok {
			name = short
		} else {
			name = shortenNameIfNeeded(name)
		}
		item, _ = sjson.SetBytes(item, "name", name)
		if args.Exists() {
			item, _ = sjson.SetBytes(item, "arguments", args.Raw)
		}
		return item
	}
}

func codexGeminiFunctionResponseToInputItem(functionResponse gjson.Result, pending codexGeminiPendingToolCall) []byte {
	switch pending.Kind.ItemType {
	case "custom_tool_call":
		item := []byte(`{"type":"custom_tool_call_output"}`)
		item, _ = sjson.SetBytes(item, "call_id", pending.CallID)
		if pending.Kind.Name != "" {
			item, _ = sjson.SetBytes(item, "name", pending.Kind.Name)
		}
		return codexGeminiSetFunctionResponseOutput(item, functionResponse)
	case "tool_search_call":
		item := []byte(`{"type":"tool_search_output","status":"completed","execution":"client","tools":[]}`)
		item, _ = sjson.SetBytes(item, "call_id", pending.CallID)
		item, _ = sjson.SetRawBytes(item, "tools", codexGeminiToolSearchOutputTools(functionResponse.Get("response")))
		return item
	default:
		item := []byte(`{"type":"function_call_output"}`)
		item, _ = sjson.SetBytes(item, "call_id", pending.CallID)
		return codexGeminiSetFunctionResponseOutput(item, functionResponse)
	}
}

func codexGeminiSetFunctionResponseOutput(item []byte, functionResponse gjson.Result) []byte {
	if res := functionResponse.Get("response.result"); res.Exists() {
		item, _ = sjson.SetBytes(item, "output", res.String())
	} else if resp := functionResponse.Get("response"); resp.Exists() {
		item, _ = sjson.SetBytes(item, "output", resp.Raw)
	}
	return item
}

func codexGeminiToolSearchOutputTools(response gjson.Result) []byte {
	if !response.Exists() || response.Type == gjson.Null {
		return []byte(`[]`)
	}
	if tools := response.Get("tools"); tools.IsArray() {
		return []byte(tools.Raw)
	}
	if result := response.Get("result"); result.Exists() {
		return codexGeminiToolSearchOutputTools(result)
	}
	if response.IsArray() {
		return []byte(response.Raw)
	}
	if response.IsObject() {
		wrapped, _ := json.Marshal([]any{json.RawMessage(response.Raw)})
		return wrapped
	}
	text := strings.TrimSpace(response.String())
	if text == "" {
		return []byte(`[]`)
	}
	if gjson.Valid(text) {
		parsed := gjson.Parse(text)
		return codexGeminiToolSearchOutputTools(parsed)
	}
	wrapped, _ := json.Marshal([]string{text})
	return wrapped
}

// shortenNameIfNeeded applies the simple shortening rule for a single name.
// Delegates to the shared codex translator helper so all four translators
// stay in sync.
func shortenNameIfNeeded(name string) string {
	return codexcommon.ShortenNameIfNeeded(name)
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	return codexcommon.BuildShortNameMap(names)
}
