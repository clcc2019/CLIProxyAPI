package helps

import (
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

func TestCountClaudeInputTokensUsesSemanticFields(t *testing.T) {
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatalf("tokenizer.Get() error = %v", err)
	}

	semantic := []byte(`{
		"system":"System text.",
		"messages":[{"role":"user","content":[{"type":"text","text":"User text."}]}],
		"tools":[{"name":"lookup","description":"Looks up data.","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"lookup"}
	}`)
	withControlFields := []byte(`{
		"model":"claude-test","max_tokens":8192,"stream":true,
		"metadata":{"large_wrapper":"ignored"},"thinking":{"type":"enabled","budget_tokens":4096},
		"system":"System text.",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"User text."},
			{"type":"image","source":{"type":"base64","data":"large-image"}},
			{"type":"document","source":{"type":"base64","data":"large-document"}}
		]}],
		"tools":[{"name":"lookup","description":"Looks up data.","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"tool_choice":{"type":"tool","name":"lookup"}
	}`)

	semanticCount, err := countClaudeInputTokens(enc, semantic)
	if err != nil {
		t.Fatalf("countClaudeInputTokens(semantic) error = %v", err)
	}
	controlCount, err := countClaudeInputTokens(enc, withControlFields)
	if err != nil {
		t.Fatalf("countClaudeInputTokens(control) error = %v", err)
	}
	if controlCount != semanticCount {
		t.Fatalf("control count = %d, want semantic count %d", controlCount, semanticCount)
	}

	segments, err := collectClaudeInputTokenSegments(semantic)
	if err != nil {
		t.Fatalf("collectClaudeInputTokenSegments() error = %v", err)
	}
	expected := "System text.\nuser\nUser text.\nlookup\nLooks up data.\n{\"type\":\"object\"}\ntool\nlookup"
	if got := strings.Join(segments, "\n"); got != expected {
		t.Fatalf("semantic segments = %q, want %q", got, expected)
	}
}

func TestClaudeInputTokenStatePatchesMessageStartOnce(t *testing.T) {
	original := []byte(`{"system":"System text.","messages":[{"role":"user","content":"Hello."}]}`)
	state := NewClaudeInputTokenState(sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, original)
	chunk := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0}}}\n\n")

	got := state.apply(nil, [][]byte{chunk})
	inputTokens := gjson.GetBytes([]byte(strings.TrimSpace(strings.TrimPrefix(strings.Split(string(got[0]), "data: ")[1], ""))), "message.usage.input_tokens")
	if inputTokens.Int() <= 0 {
		t.Fatalf("message_start input_tokens = %d, want positive: %q", inputTokens.Int(), got[0])
	}
	if !state.handled {
		t.Fatal("state.handled = false, want true")
	}

	second := []byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0}}}\n\n")
	secondGot := state.apply(nil, [][]byte{second})
	if gjson.GetBytes(secondGot[0], "message.usage.input_tokens").Int() != 0 {
		t.Fatalf("second message_start was patched: %q", secondGot[0])
	}
}

func TestCountClaudeInputTokensRejectsInvalidJSON(t *testing.T) {
	if _, err := CountClaudeInputTokens([]byte(`{"messages":`)); err == nil {
		t.Fatal("CountClaudeInputTokens() error = nil, want invalid JSON error")
	}
}
