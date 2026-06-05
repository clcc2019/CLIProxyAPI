package main

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

func main() {
	rawRequest := []byte(`{"messages":[{"content":"Hello! Claude","role":"user"}],"model":"claude-sonnet-4-5","stream":false}`)
	fmt.Println("Has claude->openai response translator:", translator.HasResponseTransformerByFormatName(
		translator.FormatClaude,
		translator.FormatOpenAI,
	))

	translatedRequest := translator.TranslateRequestByFormatName(
		translator.FormatOpenAI,
		translator.FormatClaude,
		"claude-sonnet-4-5",
		rawRequest,
		false,
	)

	fmt.Printf("Translated request to Claude format:\n%s\n\n", translatedRequest)

	claudeResponse := []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"model\":\"claude-sonnet-4-5\"}}\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello from Claude\"}}\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}\n")

	convertedResponse := translator.TranslateNonStreamByFormatName(
		context.Background(),
		translator.FormatClaude,
		translator.FormatOpenAI,
		"claude-sonnet-4-5",
		rawRequest,
		translatedRequest,
		claudeResponse,
		nil,
	)

	fmt.Printf("Converted response for OpenAI clients:\n%s\n", convertedResponse)
}
