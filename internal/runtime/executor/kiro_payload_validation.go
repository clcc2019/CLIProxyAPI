package executor

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

func validateKiroGeneratePayload(payload []byte) error {
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return malformedKiroPayloadError("payload must be a JSON object")
	}
	state := root.Get("conversationState")
	if !state.IsObject() {
		return malformedKiroPayloadError("conversationState must be an object")
	}
	if strings.TrimSpace(state.Get("conversationId").String()) == "" {
		return malformedKiroPayloadError("conversationState.conversationId is required")
	}
	current := state.Get("currentMessage.userInputMessage")
	if !current.IsObject() {
		return malformedKiroPayloadError("conversationState.currentMessage.userInputMessage is required")
	}
	if err := validateKiroUserInputMessage(current, "conversationState.currentMessage.userInputMessage"); err != nil {
		return err
	}
	if history := state.Get("history"); history.Exists() {
		if !history.IsArray() {
			return malformedKiroPayloadError("conversationState.history must be an array")
		}
		for i, item := range history.Array() {
			path := fmt.Sprintf("conversationState.history.%d", i)
			userMsg := item.Get("userInputMessage")
			assistantMsg := item.Get("assistantResponseMessage")
			switch {
			case userMsg.Exists():
				if !userMsg.IsObject() {
					return malformedKiroPayloadError(path + ".userInputMessage must be an object")
				}
				if err := validateKiroUserInputMessage(userMsg, path+".userInputMessage"); err != nil {
					return err
				}
			case assistantMsg.Exists():
				if !assistantMsg.IsObject() {
					return malformedKiroPayloadError(path + ".assistantResponseMessage must be an object")
				}
				if strings.TrimSpace(assistantMsg.Get("content").String()) == "" {
					return malformedKiroPayloadError(path + ".assistantResponseMessage.content is required")
				}
			default:
				return malformedKiroPayloadError(path + " must contain userInputMessage or assistantResponseMessage")
			}
		}
	}
	return nil
}

func validateKiroUserInputMessage(msg gjson.Result, path string) error {
	if strings.TrimSpace(msg.Get("content").String()) == "" {
		return malformedKiroPayloadError(path + ".content is required")
	}
	if strings.TrimSpace(msg.Get("modelId").String()) == "" {
		return malformedKiroPayloadError(path + ".modelId is required")
	}
	if strings.TrimSpace(msg.Get("origin").String()) == "" {
		return malformedKiroPayloadError(path + ".origin is required")
	}
	ctx := msg.Get("userInputMessageContext")
	if ctx.Exists() && !ctx.IsObject() {
		return malformedKiroPayloadError(path + ".userInputMessageContext must be an object")
	}
	if err := validateKiroToolResults(ctx.Get("toolResults"), path+".userInputMessageContext.toolResults"); err != nil {
		return err
	}
	return validateKiroTools(ctx.Get("tools"), path+".userInputMessageContext.tools")
}

func validateKiroToolResults(results gjson.Result, path string) error {
	if !results.Exists() {
		return nil
	}
	if !results.IsArray() {
		return malformedKiroPayloadError(path + " must be an array")
	}
	for i, result := range results.Array() {
		itemPath := fmt.Sprintf("%s.%d", path, i)
		if strings.TrimSpace(result.Get("toolUseId").String()) == "" {
			return malformedKiroPayloadError(itemPath + ".toolUseId is required")
		}
		if content := result.Get("content"); !content.IsArray() || len(content.Array()) == 0 {
			return malformedKiroPayloadError(itemPath + ".content must be a non-empty array")
		}
	}
	return nil
}

func validateKiroTools(tools gjson.Result, path string) error {
	if !tools.Exists() {
		return nil
	}
	if !tools.IsArray() {
		return malformedKiroPayloadError(path + " must be an array")
	}
	for i, tool := range tools.Array() {
		spec := tool.Get("toolSpecification")
		itemPath := fmt.Sprintf("%s.%d.toolSpecification", path, i)
		if !spec.IsObject() {
			return malformedKiroPayloadError(itemPath + " must be an object")
		}
		if strings.TrimSpace(spec.Get("name").String()) == "" {
			return malformedKiroPayloadError(itemPath + ".name is required")
		}
		if strings.TrimSpace(spec.Get("description").String()) == "" {
			return malformedKiroPayloadError(itemPath + ".description is required")
		}
	}
	return nil
}

func malformedKiroPayloadError(reason string) error {
	return statusErr{code: 400, msg: "kiro: malformed payload: " + reason}
}
