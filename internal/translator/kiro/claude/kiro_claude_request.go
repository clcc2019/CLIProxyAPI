// Package claude provides request translation functionality for Claude API to Kiro format.
// It handles parsing and transforming Claude API requests into the Kiro/Amazon Q API format,
// extracting model information, system instructions, message contents, and tool declarations.
package claude

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	kirocommon "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/common"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// remoteWebSearchDescription is a minimal fallback for when dynamic fetch from MCP tools/list hasn't completed yet.
const remoteWebSearchDescription = "WebSearch looks up information outside the model's training data. Supports multiple queries to gather comprehensive information."

// Kiro API request structs - field order determines JSON key order

// KiroPayload is the top-level request structure for Kiro API
type KiroPayload struct {
	ConversationState KiroConversationState `json:"conversationState"`
	ProfileArn        string                `json:"profileArn,omitempty"`
	InferenceConfig   *KiroInferenceConfig  `json:"inferenceConfig,omitempty"`
}

// KiroInferenceConfig contains inference parameters for the Kiro API.
type KiroInferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// KiroConversationState holds the conversation context
type KiroConversationState struct {
	AgentContinuationID string               `json:"agentContinuationId,omitempty"`
	AgentTaskType       string               `json:"agentTaskType,omitempty"`
	ChatTriggerType     string               `json:"chatTriggerType"` // Required: "MANUAL"
	ConversationID      string               `json:"conversationId"`
	CurrentMessage      KiroCurrentMessage   `json:"currentMessage"`
	History             []KiroHistoryMessage `json:"history,omitempty"`
}

// KiroCurrentMessage wraps the current user message
type KiroCurrentMessage struct {
	UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
}

// KiroHistoryMessage represents a message in the conversation history
type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

// KiroImage represents an image in Kiro API format
type KiroImage struct {
	Format string          `json:"format"`
	Source KiroImageSource `json:"source"`
}

// KiroImageSource contains the image data
type KiroImageSource struct {
	Bytes string `json:"bytes"` // base64 encoded image data
}

// KiroUserInputMessage represents a user message
type KiroUserInputMessage struct {
	Content                 string                       `json:"content"`
	ModelID                 string                       `json:"modelId"`
	Origin                  string                       `json:"origin"`
	Images                  []KiroImage                  `json:"images,omitempty"`
	UserInputMessageContext *KiroUserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

// KiroUserInputMessageContext contains tool-related context
type KiroUserInputMessageContext struct {
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
}

// KiroToolResult represents a tool execution result
type KiroToolResult struct {
	Content   []KiroTextContent `json:"content"`
	Status    string            `json:"status"`
	ToolUseID string            `json:"toolUseId"`
}

// KiroTextContent represents text content
type KiroTextContent struct {
	Text string `json:"text"`
}

// KiroToolWrapper wraps a tool specification
type KiroToolWrapper struct {
	ToolSpecification KiroToolSpecification `json:"toolSpecification"`
}

// KiroToolSpecification defines a tool's schema
type KiroToolSpecification struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema KiroInputSchema `json:"inputSchema"`
}

// KiroInputSchema wraps the JSON schema for tool input
type KiroInputSchema struct {
	JSON interface{} `json:"json"`
}

// KiroAssistantResponseMessage represents an assistant message
type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

// KiroToolUse represents a tool invocation by the assistant
type KiroToolUse struct {
	ToolUseID      string                 `json:"toolUseId"`
	Name           string                 `json:"name"`
	Input          map[string]interface{} `json:"input"`
	IsTruncated    bool                   `json:"-"` // Internal flag, not serialized
	TruncationInfo *TruncationInfo        `json:"-"` // Truncation details, not serialized
}

// ConvertClaudeRequestToKiro converts a Claude API request to Kiro format.
// This is the main entry point for request translation.
func ConvertClaudeRequestToKiro(modelName string, inputRawJSON []byte, stream bool) []byte {
	// For Kiro, we pass through the Claude format since buildKiroPayload
	// expects Claude format and does the conversion internally.
	// The actual conversion happens in the executor when building the HTTP request.
	return inputRawJSON
}

// BuildKiroPayload constructs the Kiro API request payload from Claude format.
// Supports tool calling - tools are passed via userInputMessageContext.
// origin parameter determines which quota to use: "CLI" for Amazon Q, "AI_EDITOR" for Kiro IDE.
// isAgentic parameter enables chunked write optimization prompt for -agentic model variants.
// isChatOnly parameter disables tool calling for -chat model variants (pure conversation mode).
// headers parameter allows checking Anthropic-Beta header for thinking mode detection.
// metadata parameter is kept for API compatibility but no longer used for thinking configuration.
// Supports thinking mode - when enabled, injects thinking tags into system prompt.
// Returns the payload and a boolean indicating whether thinking mode was injected.
func BuildKiroPayload(claudeBody []byte, modelID, profileArn, origin string, isAgentic, isChatOnly bool, headers http.Header, metadata map[string]any) ([]byte, bool) {
	// Inference knobs — `-1` is normalised to the Kiro hard cap by the helper.
	params := kirocommon.ExtractInferenceParams(claudeBody)

	// Normalize origin value for Kiro API compatibility (quota routing).
	origin = kirocommon.NormalizeOrigin(origin)

	messages := gjson.GetBytes(claudeBody, "messages")

	// For chat-only mode, don't include tools
	var tools gjson.Result
	if !isChatOnly {
		tools = gjson.GetBytes(claudeBody, "tools")
	}

	// Extract system prompt from Claude-style top-level `system` field.
	systemPrompt := extractSystemPrompt(claudeBody)

	// Check for thinking mode using the comprehensive IsThinkingEnabledWithHeaders function
	// This supports Claude API format, OpenAI reasoning_effort, AMP/Cursor format, and Anthropic-Beta header
	thinkingEnabled := IsThinkingEnabledWithHeaders(claudeBody, headers)

	// Stage 1: wall-clock context — must come before any other injections
	// so agentic loops consistently see the same timestamp format.
	systemPrompt = kirocommon.InjectTimestampContext(systemPrompt, time.Now())

	// Stage 2: agentic optimisation hint (model-variant-specific).
	if isAgentic {
		systemPrompt = kirocommon.AppendSystemHint(systemPrompt, kirocommon.KiroAgenticSystemPrompt)
	}

	// Stage 3: tool_choice hint — Kiro doesn't accept tool_choice natively.
	// Claude tool_choice values: {"type": "auto/any/tool", "name": "..."}
	if hint := extractClaudeToolChoiceHint(claudeBody); hint != "" {
		systemPrompt = kirocommon.AppendSystemHint(systemPrompt, hint)
	}

	// Convert Claude tools to Kiro format (needed before thinking hint so we
	// can report has_tools in the log line).
	kiroTools := convertClaudeToolsToKiro(tools)

	// Stage 4: thinking mode — Kiro's official reasoning hint goes to the
	// front so the model sees it before the user prompt.
	if thinkingEnabled {
		systemPrompt = kirocommon.PrependThinkingHint(systemPrompt)
		log.Infof("kiro: injected thinking prompt (official mode), has_tools: %v", len(kiroTools) > 0)
	}

	// Process messages and build history
	history, currentUserMsg, currentToolResults := processMessages(messages, modelID, origin)

	// Build content with system prompt (only on first turn to avoid re-injection)
	if currentUserMsg != nil {
		effectiveSystemPrompt := systemPrompt
		if len(history) > 0 {
			effectiveSystemPrompt = "" // Don't re-inject on subsequent turns
		}
		currentUserMsg.Content = buildFinalContent(currentUserMsg.Content, effectiveSystemPrompt, currentToolResults)

		// Deduplicate currentToolResults
		currentToolResults = deduplicateToolResults(currentToolResults)

		// Build userInputMessageContext with tools and tool results
		if len(kiroTools) > 0 || len(currentToolResults) > 0 {
			currentUserMsg.UserInputMessageContext = &KiroUserInputMessageContext{
				Tools:       kiroTools,
				ToolResults: currentToolResults,
			}
		}
	}

	// Build payload
	var currentMessage KiroCurrentMessage
	if currentUserMsg != nil {
		currentMessage = KiroCurrentMessage{UserInputMessage: *currentUserMsg}
	} else {
		currentMessage = KiroCurrentMessage{UserInputMessage: KiroUserInputMessage{
			Content: kirocommon.BuildFallbackSystemPromptContent(systemPrompt),
			ModelID: modelID,
			Origin:  origin,
		}}
	}

	// Build inferenceConfig if we have any inference parameters
	// Note: Kiro API doesn't actually use max_tokens for thinking budget
	var inferenceConfig *KiroInferenceConfig
	if params.HasAnyInferenceConfig() {
		inferenceConfig = &KiroInferenceConfig{}
		if params.MaxTokens > 0 {
			inferenceConfig.MaxTokens = int(params.MaxTokens)
		}
		if params.HasTemperature {
			inferenceConfig.Temperature = params.Temperature
		}
		if params.HasTopP {
			inferenceConfig.TopP = params.TopP
		}
	}

	// Session IDs: extract from messages[].additional_kwargs (LangChain format) or random
	conversationID := extractMetadataFromMessages(messages, "conversationId")
	continuationID := extractMetadataFromMessages(messages, "continuationId")
	if conversationID == "" {
		conversationID = uuid.New().String()
	}

	payload := KiroPayload{
		ConversationState: KiroConversationState{
			AgentTaskType:   "vibe",
			ChatTriggerType: "MANUAL",
			ConversationID:  conversationID,
			CurrentMessage:  currentMessage,
			History:         history,
		},
		ProfileArn:      profileArn,
		InferenceConfig: inferenceConfig,
	}

	// Only set AgentContinuationID if client provided
	if continuationID != "" {
		payload.ConversationState.AgentContinuationID = continuationID
	}

	result, err := json.Marshal(payload)
	if err != nil {
		log.Debugf("kiro: failed to marshal payload: %v", err)
		return nil, false
	}

	return result, thinkingEnabled
}

// normalizeOrigin was inlined into kirocommon.NormalizeOrigin. The wrapper is
// kept as a thin alias so downstream callers (tests, other packages) that
// reach into this package still compile.
func normalizeOrigin(origin string) string {
	return kirocommon.NormalizeOrigin(origin)
}

// extractMetadataFromMessages extracts metadata from messages[].additional_kwargs (LangChain format).
// Searches from the last message backwards, returns empty string if not found.
func extractMetadataFromMessages(messages gjson.Result, key string) string {
	arr := messages.Array()
	for i := len(arr) - 1; i >= 0; i-- {
		if val := arr[i].Get("additional_kwargs." + key); val.Exists() && val.String() != "" {
			return val.String()
		}
	}
	return ""
}

// extractSystemPrompt extracts system prompt from Claude request
func extractSystemPrompt(claudeBody []byte) string {
	systemField := gjson.GetBytes(claudeBody, "system")
	if systemField.IsArray() {
		var sb strings.Builder
		for _, block := range systemField.Array() {
			if block.Get("type").String() == "text" {
				sb.WriteString(block.Get("text").String())
			} else if block.Type == gjson.String {
				sb.WriteString(block.String())
			}
		}
		return sb.String()
	}
	return systemField.String()
}

// checkThinkingMode checks if thinking mode is enabled in the Claude request
func checkThinkingMode(claudeBody []byte) (bool, int64) {
	thinkingEnabled := false
	var budgetTokens int64 = 24000

	thinkingField := gjson.GetBytes(claudeBody, "thinking")
	if thinkingField.Exists() {
		thinkingType := thinkingField.Get("type").String()
		if thinkingType == "enabled" {
			thinkingEnabled = true
			if bt := thinkingField.Get("budget_tokens"); bt.Exists() {
				budgetTokens = bt.Int()
				if budgetTokens <= 0 {
					thinkingEnabled = false
					log.Debugf("kiro: thinking mode disabled via budget_tokens <= 0")
				}
			}
			if thinkingEnabled {
				log.Debugf("kiro: thinking mode enabled via Claude API parameter, budget_tokens: %d", budgetTokens)
			}
		}
	}

	return thinkingEnabled, budgetTokens
}

// IsThinkingEnabledFromHeader checks if thinking mode is enabled via Anthropic-Beta header.
// Claude CLI uses "Anthropic-Beta: interleaved-thinking-2025-05-14" to enable thinking.
func IsThinkingEnabledFromHeader(headers http.Header) bool {
	if headers == nil {
		return false
	}
	betaHeader := headers.Get("Anthropic-Beta")
	if betaHeader == "" {
		return false
	}
	// Check for interleaved-thinking beta feature
	if strings.Contains(betaHeader, "interleaved-thinking") {
		log.Debugf("kiro: thinking mode enabled via Anthropic-Beta header: %s", betaHeader)
		return true
	}
	return false
}

// IsThinkingEnabled is a public wrapper to check if thinking mode is enabled.
// This is used by the executor to determine whether to parse <thinking> tags in responses.
// When thinking is NOT enabled in the request, <thinking> tags in responses should be
// treated as regular text content, not as thinking blocks.
//
// Supports multiple formats:
// - Claude API format: thinking.type = "enabled"
// - OpenAI format: reasoning_effort parameter
// - AMP/Cursor format: <thinking_mode>interleaved</thinking_mode> in system prompt
func IsThinkingEnabled(body []byte) bool {
	return IsThinkingEnabledWithHeaders(body, nil)
}

// IsThinkingEnabledWithHeaders checks if thinking mode is enabled from body or headers.
// This is the comprehensive check that supports all thinking detection methods:
// - Claude API format: thinking.type = "enabled"
// - OpenAI format: reasoning_effort parameter
// - AMP/Cursor format: <thinking_mode>interleaved</thinking_mode> in system prompt
// - Anthropic-Beta header: interleaved-thinking-2025-05-14
func IsThinkingEnabledWithHeaders(body []byte, headers http.Header) bool {
	// Check Anthropic-Beta header first (Claude Code uses this)
	if IsThinkingEnabledFromHeader(headers) {
		return true
	}

	// Check Claude API format first (thinking.type = "enabled")
	enabled, _ := checkThinkingMode(body)
	if enabled {
		log.Debugf("kiro: IsThinkingEnabled returning true (Claude API format)")
		return true
	}

	// Check OpenAI format: reasoning_effort parameter
	// Valid values: "low", "medium", "high", "auto" (not "none")
	reasoningEffort := gjson.GetBytes(body, "reasoning_effort")
	if reasoningEffort.Exists() {
		effort := reasoningEffort.String()
		if effort != "" && effort != "none" {
			log.Debugf("kiro: thinking mode enabled via OpenAI reasoning_effort: %s", effort)
			return true
		}
	}

	// Check AMP/Cursor format: <thinking_mode>interleaved</thinking_mode> in system prompt
	// This is how AMP client passes thinking configuration
	bodyStr := string(body)
	if strings.Contains(bodyStr, "<thinking_mode>") && strings.Contains(bodyStr, "</thinking_mode>") {
		// Extract thinking mode value
		startTag := "<thinking_mode>"
		endTag := "</thinking_mode>"
		startIdx := strings.Index(bodyStr, startTag)
		if startIdx >= 0 {
			startIdx += len(startTag)
			endIdx := strings.Index(bodyStr[startIdx:], endTag)
			if endIdx >= 0 {
				thinkingMode := bodyStr[startIdx : startIdx+endIdx]
				if thinkingMode == "interleaved" || thinkingMode == "enabled" {
					log.Debugf("kiro: thinking mode enabled via AMP/Cursor format: %s", thinkingMode)
					return true
				}
			}
		}
	}

	// Check OpenAI format: max_completion_tokens with reasoning (o1-style)
	// Some clients use this to indicate reasoning mode
	if gjson.GetBytes(body, "max_completion_tokens").Exists() {
		// If max_completion_tokens is set, check if model name suggests reasoning
		model := gjson.GetBytes(body, "model").String()
		if strings.Contains(strings.ToLower(model), "thinking") ||
			strings.Contains(strings.ToLower(model), "reason") {
			log.Debugf("kiro: thinking mode enabled via model name hint: %s", model)
			return true
		}
	}

	log.Debugf("kiro: IsThinkingEnabled returning false (no thinking mode detected)")
	return false
}

// shortenToolNameIfNeeded shortens tool names that exceed 64 characters.
// MCP tools often have long names like "mcp__server-name__tool-name".
// This preserves the "mcp__" prefix and last segment when possible.
func shortenToolNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	// For MCP tools, try to preserve prefix and last segment
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > limit {
				return cand[:limit]
			}
			return cand
		}
	}
	return name[:limit]
}

func ensureKiroInputSchema(parameters interface{}) interface{} {
	if parameters != nil {
		return parameters
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

// convertClaudeToolsToKiro converts Claude tools to Kiro format
func convertClaudeToolsToKiro(tools gjson.Result) []KiroToolWrapper {
	var kiroTools []KiroToolWrapper
	if !tools.IsArray() {
		return kiroTools
	}

	toolsArray := tools.Array()
	for _, tool := range toolsArray {
		name := tool.Get("name").String()
		toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
		description := tool.Get("description").String()
		inputSchemaResult := tool.Get("input_schema")
		var inputSchema interface{}
		if inputSchemaResult.Exists() && inputSchemaResult.Type != gjson.Null {
			inputSchema = inputSchemaResult.Value()
		}
		inputSchema = ensureKiroInputSchema(inputSchema)

		// Shorten tool name if it exceeds 64 characters (common with MCP tools)
		originalName := name
		name = shortenToolNameIfNeeded(name)
		if name != originalName {
			log.Debugf("kiro: shortened tool name from '%s' to '%s'", originalName, name)
		}

		// CRITICAL FIX: Kiro API requires non-empty description
		if strings.TrimSpace(description) == "" {
			description = fmt.Sprintf("Tool: %s", name)
			log.Debugf("kiro: tool '%s' has empty description, using default: %s", name, description)
		}

		// Claude built-in web_search tools can appear alongside normal tools.
		// In mixed-tool requests, skip the built-in entry to avoid upstream 400 errors.
		if strings.HasPrefix(toolType, "web_search") && len(toolsArray) > 1 {
			log.Infof("kiro: skipping Claude built-in web_search tool in mixed-tool request (type=%s)", toolType)
			continue
		}

		// Rename web_search → remote_web_search for Kiro API compatibility
		if name == "web_search" || strings.HasPrefix(toolType, "web_search") {
			name = "remote_web_search"
			// Prefer dynamically fetched description, fall back to hardcoded constant
			if cached := GetWebSearchDescription(); cached != "" {
				description = cached
			} else {
				description = remoteWebSearchDescription
			}
			log.Debugf("kiro: renamed tool web_search → remote_web_search")
		}

		// Truncate long descriptions (individual tool limit)
		if len(description) > kirocommon.KiroMaxToolDescLen {
			truncLen := kirocommon.KiroMaxToolDescLen - 30
			for truncLen > 0 && !utf8.RuneStart(description[truncLen]) {
				truncLen--
			}
			description = description[:truncLen] + "... (description truncated)"
		}

		kiroTools = append(kiroTools, KiroToolWrapper{
			ToolSpecification: KiroToolSpecification{
				Name:        name,
				Description: description,
				InputSchema: KiroInputSchema{JSON: inputSchema},
			},
		})
	}

	return kiroTools
}

// processMessages processes Claude messages and builds Kiro history
func processMessages(messages gjson.Result, modelID, origin string) ([]KiroHistoryMessage, *KiroUserInputMessage, []KiroToolResult) {
	var history []KiroHistoryMessage
	var currentUserMsg *KiroUserInputMessage
	var currentToolResults []KiroToolResult

	// Merge adjacent messages with the same role
	messagesArray := kirocommon.MergeAdjacentMessages(messages.Array())

	// FIX: Kiro API requires history to start with a user message.
	// Some clients (e.g., OpenClaw) send conversations starting with an assistant message,
	// which is valid for the Claude API but causes "Improperly formed request" on Kiro.
	// Prepend a placeholder user message so the history alternation is correct.
	if len(messagesArray) > 0 && messagesArray[0].Get("role").String() == "assistant" {
		placeholder := `{"role":"user","content":"."}`
		messagesArray = append([]gjson.Result{gjson.Parse(placeholder)}, messagesArray...)
		log.Infof("kiro: messages started with assistant role, prepended placeholder user message for Kiro API compatibility")
	}

	for i, msg := range messagesArray {
		role := msg.Get("role").String()
		isLastMessage := i == len(messagesArray)-1

		switch role {
		case "user":
			userMsg, toolResults := BuildUserMessageStruct(msg, modelID, origin)
			// CRITICAL: Kiro API requires content to be non-empty for ALL user messages
			// This includes both history messages and the current message.
			// When user message contains only tool_result (no text), content will be empty.
			// This commonly happens in compaction requests from OpenCode.
			if strings.TrimSpace(userMsg.Content) == "" {
				if len(toolResults) > 0 {
					userMsg.Content = kirocommon.DefaultUserContentWithToolResults
				} else {
					userMsg.Content = kirocommon.DefaultUserContent
				}
				log.Debugf("kiro: user content was empty, using default: %s", userMsg.Content)
			}
			if isLastMessage {
				currentUserMsg = &userMsg
				currentToolResults = toolResults
			} else {
				// For history messages, embed tool results in context
				if len(toolResults) > 0 {
					userMsg.UserInputMessageContext = &KiroUserInputMessageContext{
						ToolResults: toolResults,
					}
				}
				history = append(history, KiroHistoryMessage{
					UserInputMessage: &userMsg,
				})
			}
		case "assistant":
			assistantMsg := BuildAssistantMessageStruct(msg)
			if isLastMessage {
				history = append(history, KiroHistoryMessage{
					AssistantResponseMessage: &assistantMsg,
				})
				// Create a "Continue" user message as currentMessage
				currentUserMsg = &KiroUserInputMessage{
					Content: "Continue",
					ModelID: modelID,
					Origin:  origin,
				}
			} else {
				history = append(history, KiroHistoryMessage{
					AssistantResponseMessage: &assistantMsg,
				})
			}
		}
	}

	// POST-PROCESSING: Remove orphaned tool_results that have no matching tool_use
	// in any assistant message. This happens when Claude Code compaction truncates
	// the conversation and removes the assistant message containing the tool_use,
	// but keeps the user message with the corresponding tool_result.
	// Without this fix, Kiro API returns "Improperly formed request".
	validToolUseIDs := make(map[string]bool)
	for _, h := range history {
		if h.AssistantResponseMessage != nil {
			for _, tu := range h.AssistantResponseMessage.ToolUses {
				validToolUseIDs[tu.ToolUseID] = true
			}
		}
	}

	// Filter orphaned tool results from history user messages
	for i, h := range history {
		if h.UserInputMessage != nil && h.UserInputMessage.UserInputMessageContext != nil {
			ctx := h.UserInputMessage.UserInputMessageContext
			if len(ctx.ToolResults) > 0 {
				filtered := make([]KiroToolResult, 0, len(ctx.ToolResults))
				for _, tr := range ctx.ToolResults {
					if validToolUseIDs[tr.ToolUseID] {
						filtered = append(filtered, tr)
					} else {
						log.Debugf("kiro: dropping orphaned tool_result in history[%d]: toolUseId=%s (no matching tool_use)", i, tr.ToolUseID)
					}
				}
				ctx.ToolResults = filtered
				if len(ctx.ToolResults) == 0 && len(ctx.Tools) == 0 {
					h.UserInputMessage.UserInputMessageContext = nil
				}
			}
		}
	}

	// Filter orphaned tool results from current message
	if len(currentToolResults) > 0 {
		filtered := make([]KiroToolResult, 0, len(currentToolResults))
		for _, tr := range currentToolResults {
			if validToolUseIDs[tr.ToolUseID] {
				filtered = append(filtered, tr)
			} else {
				log.Debugf("kiro: dropping orphaned tool_result in currentMessage: toolUseId=%s (no matching tool_use)", tr.ToolUseID)
			}
		}
		if len(filtered) != len(currentToolResults) {
			log.Infof("kiro: dropped %d orphaned tool_result(s) from currentMessage (compaction artifact)", len(currentToolResults)-len(filtered))
		}
		currentToolResults = filtered
	}

	return history, currentUserMsg, currentToolResults
}

// buildFinalContent builds the final content with system prompt
func buildFinalContent(content, systemPrompt string, toolResults []KiroToolResult) string {
	var contentBuilder strings.Builder

	if systemPrompt != "" {
		contentBuilder.WriteString("--- SYSTEM PROMPT ---\n")
		contentBuilder.WriteString(systemPrompt)
		contentBuilder.WriteString("\n--- END SYSTEM PROMPT ---\n\n")
	}

	contentBuilder.WriteString(content)
	finalContent := contentBuilder.String()

	// CRITICAL: Kiro API requires content to be non-empty
	if strings.TrimSpace(finalContent) == "" {
		if len(toolResults) > 0 {
			finalContent = "Tool results provided."
		} else {
			finalContent = "Continue"
		}
		log.Debugf("kiro: content was empty, using default: %s", finalContent)
	}

	return finalContent
}

// deduplicateToolResults removes duplicate tool results
func deduplicateToolResults(toolResults []KiroToolResult) []KiroToolResult {
	if len(toolResults) == 0 {
		return toolResults
	}

	seenIDs := make(map[string]bool)
	unique := make([]KiroToolResult, 0, len(toolResults))
	for _, tr := range toolResults {
		if !seenIDs[tr.ToolUseID] {
			seenIDs[tr.ToolUseID] = true
			unique = append(unique, tr)
		} else {
			log.Debugf("kiro: skipping duplicate toolResult in currentMessage: %s", tr.ToolUseID)
		}
	}
	return unique
}

// extractClaudeToolChoiceHint extracts tool_choice from Claude request and returns a system prompt hint.
// Claude tool_choice values:
// - {"type": "auto"}: Model decides (default, no hint needed)
// - {"type": "any"}: Must use at least one tool
// - {"type": "tool", "name": "..."}: Must use specific tool
func extractClaudeToolChoiceHint(claudeBody []byte) string {
	toolChoice := gjson.GetBytes(claudeBody, "tool_choice")
	if !toolChoice.Exists() {
		return ""
	}

	toolChoiceType := toolChoice.Get("type").String()
	switch toolChoiceType {
	case "any":
		return "[INSTRUCTION: You MUST use at least one of the available tools to respond. Do not respond with text only - always make a tool call.]"
	case "tool":
		toolName := toolChoice.Get("name").String()
		if toolName != "" {
			return fmt.Sprintf("[INSTRUCTION: You MUST use the tool named '%s' to respond. Do not use any other tool or respond with text only.]", toolName)
		}
	case "auto":
		// Default behavior, no hint needed
		return ""
	}

	return ""
}

// BuildUserMessageStruct builds a user message and extracts tool results
func BuildUserMessageStruct(msg gjson.Result, modelID, origin string) (KiroUserInputMessage, []KiroToolResult) {
	content := msg.Get("content")
	var contentBuilder strings.Builder
	var toolResults []KiroToolResult
	var images []KiroImage

	// Track seen toolUseIds to deduplicate
	seenToolUseIDs := make(map[string]bool)

	if content.IsArray() {
		for _, part := range content.Array() {
			partType := part.Get("type").String()
			switch partType {
			case "text":
				contentBuilder.WriteString(part.Get("text").String())
			case "image":
				mediaType := part.Get("source.media_type").String()
				data := part.Get("source.data").String()

				format := ""
				if idx := strings.LastIndex(mediaType, "/"); idx != -1 {
					format = mediaType[idx+1:]
				}

				if format != "" && data != "" {
					images = append(images, KiroImage{
						Format: format,
						Source: KiroImageSource{
							Bytes: data,
						},
					})
				}
			case "tool_result":
				toolUseID := part.Get("tool_use_id").String()

				// Skip duplicate toolUseIds
				if seenToolUseIDs[toolUseID] {
					log.Debugf("kiro: skipping duplicate tool_result with toolUseId: %s", toolUseID)
					continue
				}
				seenToolUseIDs[toolUseID] = true

				isError := part.Get("is_error").Bool()
				resultContent := part.Get("content")

				var textContents []KiroTextContent

				if resultContent.IsArray() {
					for _, item := range resultContent.Array() {
						if item.Get("type").String() == "text" {
							textContents = append(textContents, KiroTextContent{Text: item.Get("text").String()})
						} else if item.Type == gjson.String {
							textContents = append(textContents, KiroTextContent{Text: item.String()})
						}
					}
				} else if resultContent.Type == gjson.String {
					textContents = append(textContents, KiroTextContent{Text: resultContent.String()})
				}

				if len(textContents) == 0 {
					textContents = append(textContents, KiroTextContent{Text: "Tool use was cancelled by the user"})
				}

				status := "success"
				if isError {
					status = "error"
				}

				toolResults = append(toolResults, KiroToolResult{
					ToolUseID: toolUseID,
					Content:   textContents,
					Status:    status,
				})
			}
		}
	} else {
		contentBuilder.WriteString(content.String())
	}

	userMsg := KiroUserInputMessage{
		Content: contentBuilder.String(),
		ModelID: modelID,
		Origin:  origin,
	}

	if len(images) > 0 {
		userMsg.Images = images
	}

	return userMsg, toolResults
}

// BuildAssistantMessageStruct builds an assistant message with tool uses
func BuildAssistantMessageStruct(msg gjson.Result) KiroAssistantResponseMessage {
	content := msg.Get("content")
	var contentBuilder strings.Builder
	var toolUses []KiroToolUse

	if content.IsArray() {
		for _, part := range content.Array() {
			partType := part.Get("type").String()
			switch partType {
			case "text":
				contentBuilder.WriteString(part.Get("text").String())
			case "tool_use":
				toolUseID := part.Get("id").String()
				toolName := part.Get("name").String()
				toolInput := part.Get("input")

				var inputMap map[string]interface{}
				if toolInput.IsObject() {
					inputMap = make(map[string]interface{})
					toolInput.ForEach(func(key, value gjson.Result) bool {
						inputMap[key.String()] = value.Value()
						return true
					})
				}

				// Rename web_search → remote_web_search to match convertClaudeToolsToKiro
				if toolName == "web_search" {
					toolName = "remote_web_search"
				}

				toolUses = append(toolUses, KiroToolUse{
					ToolUseID: toolUseID,
					Name:      toolName,
					Input:     inputMap,
				})
			}
		}
	} else {
		contentBuilder.WriteString(content.String())
	}

	// CRITICAL FIX: Kiro API requires non-empty content for assistant messages
	// This can happen with compaction requests where assistant messages have only tool_use
	// (no text content). Without this fix, Kiro API returns "Improperly formed request" error.
	finalContent := contentBuilder.String()
	if strings.TrimSpace(finalContent) == "" {
		if len(toolUses) > 0 {
			finalContent = kirocommon.DefaultAssistantContentWithTools
		} else {
			finalContent = kirocommon.DefaultAssistantContent
		}
		log.Debugf("kiro: assistant content was empty, using default: %s", finalContent)
	}

	return KiroAssistantResponseMessage{
		Content:  finalContent,
		ToolUses: toolUses,
	}
}
