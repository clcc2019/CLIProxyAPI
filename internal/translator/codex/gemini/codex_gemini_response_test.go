package gemini

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToGemini_StreamEmptyOutputUsesOutputItemDoneMessageFallback(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)...)
	}

	found := false
	for _, out := range outputs {
		if gjson.GetBytes(out, "candidates.0.content.parts.0.text").String() == "ok" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected fallback content from response.output_item.done message; outputs=%q", outputs)
	}
}

func TestConvertCodexResponseToGemini_StreamPartialImageEmitsInlineData(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunk := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)
	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.inlineData.data").String()
	if got != "aGVsbG8=" {
		t.Fatalf("expected inlineData.data %q, got %q; chunk=%s", "aGVsbG8=", got, string(out[0]))
	}

	gotMime := gjson.GetBytes(out[0], "candidates.0.content.parts.0.inlineData.mimeType").String()
	if gotMime != "image/png" {
		t.Fatalf("expected inlineData.mimeType %q, got %q; chunk=%s", "image/png", gotMime, string(out[0]))
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)
	if len(out) != 0 {
		t.Fatalf("expected duplicate image chunk to be suppressed, got %d", len(out))
	}
}

func TestConvertCodexResponseToGemini_StreamImageGenerationCallDoneEmitsInlineData(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"png","result":"aGVsbG8="}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected output_item.done to be suppressed when identical to last partial image, got %d", len(out))
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"jpeg","result":"Ymll"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.inlineData.data").String()
	if got != "Ymll" {
		t.Fatalf("expected inlineData.data %q, got %q; chunk=%s", "Ymll", got, string(out[0]))
	}

	gotMime := gjson.GetBytes(out[0], "candidates.0.content.parts.0.inlineData.mimeType").String()
	if gotMime != "image/jpeg" {
		t.Fatalf("expected inlineData.mimeType %q, got %q; chunk=%s", "image/jpeg", gotMime, string(out[0]))
	}
}

func TestConvertCodexResponseToGemini_StreamReasoningDoneFallbackUsesSummary(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"first "},{"type":"summary_text","text":"second"}]}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected reasoning fallback chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.text").String(); got != "first second" {
		t.Fatalf("reasoning text = %q, want joined summary; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.thought").Bool(); !got {
		t.Fatalf("reasoning part should be marked thought; chunk=%s", out[0])
	}
}

func TestConvertCodexResponseToGemini_StreamReasoningDoneFallbackUsesContent(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"hidden "},{"type":"text","text":"trace"}]}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected reasoning content fallback chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.text").String(); got != "hidden trace" {
		t.Fatalf("reasoning text = %q, want joined content; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToGemini_StreamReasoningTextDelta(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`), &param)
	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.reasoning_text.delta","delta":"raw trace","content_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected reasoning_text delta chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.text").String(); got != "raw trace" {
		t.Fatalf("reasoning text = %q, want raw trace; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.thought").Bool(); !got {
		t.Fatalf("reasoning part should be marked thought; chunk=%s", out[0])
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"raw trace"}]}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected done for streamed raw reasoning item to be suppressed, got %d chunks: %q", len(out), out)
	}
}

func TestConvertCodexResponseToGemini_StreamReasoningDeltaStateResetsPerItem(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`), &param)
	ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"streamed"}`), &param)
	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"streamed"}]}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected done for streamed reasoning item to be suppressed, got %d chunks", len(out))
	}

	ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`), &param)
	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"fallback"}]}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected second reasoning item fallback chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.text").String(); got != "fallback" {
		t.Fatalf("reasoning text = %q, want fallback; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToGemini_UsageWithoutTotalDoesNotDoubleCountReasoning(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":50,"output_tokens_details":{"reasoning_tokens":15}}}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 usage chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "usageMetadata.totalTokenCount").Int(); got != 150 {
		t.Fatalf("totalTokenCount = %d, want 150. Output: %s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "usageMetadata.thoughtsTokenCount").Int(); got != 15 {
		t.Fatalf("thoughtsTokenCount = %d, want 15. Output: %s", got, out[0])
	}
}

func TestConvertCodexResponseToGemini_NonStreamImageGenerationCallAddsInlineDataPart(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","output_format":"png","result":"aGVsbG8="}]}}`)
	out := ConvertCodexResponseToGeminiNonStream(ctx, "gemini-2.5-pro", originalRequest, nil, raw, nil)

	got := gjson.GetBytes(out, "candidates.0.content.parts.1.inlineData.data").String()
	if got != "aGVsbG8=" {
		t.Fatalf("expected inlineData.data %q, got %q; chunk=%s", "aGVsbG8=", got, string(out))
	}

	gotMime := gjson.GetBytes(out, "candidates.0.content.parts.1.inlineData.mimeType").String()
	if gotMime != "image/png" {
		t.Fatalf("expected inlineData.mimeType %q, got %q; chunk=%s", "image/png", gotMime, string(out))
	}
}

func TestConvertCodexResponseToGemini_NonStreamOfficialToolCallVariants(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	raw := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"created_at":1700000000,
			"usage":{"input_tokens":1,"output_tokens":1},
			"output":[
				{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
				{"type":"local_shell_call","call_id":"call_shell","status":"completed","action":{"type":"exec","command":["echo","hi"]}},
				{"type":"tool_search_call","call_id":"call_search","execution":"client","arguments":{"query":"calendar","limit":1}}
			]
		}
	}`)

	out := ConvertCodexResponseToGeminiNonStream(ctx, "gemini-2.5-pro", originalRequest, nil, raw, nil)
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.functionCall.name").String(); got != "apply_patch" {
		t.Fatalf("custom tool name = %q, want apply_patch. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.functionCall.args.input").String(); got != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom tool input = %q. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.1.functionCall.name").String(); got != "local_shell" {
		t.Fatalf("local shell name = %q, want local_shell. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.1.functionCall.args.command.0").String(); got != "echo" {
		t.Fatalf("local shell command[0] = %q, want echo. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.2.functionCall.name").String(); got != "tool_search" {
		t.Fatalf("tool search name = %q, want tool_search. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.2.functionCall.args.query").String(); got != "calendar" {
		t.Fatalf("tool search query = %q, want calendar. Output: %s", got, string(out))
	}
}

func TestConvertCodexResponseToGemini_ServerToolSearchIsInternal(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	raw := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"created_at":1700000000,
			"usage":{"input_tokens":1,"output_tokens":1},
			"output":[
				{"type":"tool_search_call","call_id":"server_search","execution":"server","status":"completed","arguments":{"paths":["crm"]}},
				{"type":"message","content":[{"type":"output_text","text":"done"}]}
			]
		}
	}`)

	out := ConvertCodexResponseToGeminiNonStream(ctx, "gemini-2.5-pro", originalRequest, nil, raw, nil)
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.text").String(); got != "done" {
		t.Fatalf("content text = %q, want done. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "candidates.0.content.parts.0.functionCall").Exists() {
		t.Fatalf("server-side tool search should not emit a Gemini functionCall. Output: %s", string(out))
	}
	if gjson.GetBytes(out, "candidates.0.content.parts.1").Exists() {
		t.Fatalf("unexpected second Gemini part for server-side tool search. Output: %s", string(out))
	}
}

func TestConvertCodexResponseToGemini_NonStreamReasoningTextFromSummaryAndContent(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	raw := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"created_at":1700000000,
			"usage":{"input_tokens":1,"output_tokens":1},
			"output":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"first "},{"type":"summary_text","text":"second"}]},
				{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":" third"}]},
				{"type":"message","content":[{"type":"output_text","text":"ok"}]}
			]
		}
	}`)

	out := ConvertCodexResponseToGeminiNonStream(ctx, "gemini-2.5-pro", originalRequest, nil, raw, nil)
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.text").String(); got != "first second" {
		t.Fatalf("first reasoning text = %q, want joined summary. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.thought").Bool(); !got {
		t.Fatalf("first reasoning part should be thought. Output: %s", string(out))
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.1.text").String(); got != " third" {
		t.Fatalf("second reasoning text = %q, want content fallback. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.2.text").String(); got != "ok" {
		t.Fatalf("message text = %q, want ok. Output: %s", got, string(out))
	}
}

func TestConvertCodexResponseToGemini_StreamCustomToolCallDoneStored(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected custom tool call to be stored until completed, got %d", len(out))
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`), &param)
	if len(out) != 2 {
		t.Fatalf("expected stored tool call plus completed chunk, got %d: %q", len(out), out)
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.name").String(); got != "apply_patch" {
		t.Fatalf("stream custom tool name = %q, want apply_patch. Output: %s", got, string(out[0]))
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.args.input").String(); got != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("stream custom tool input = %q. Output: %s", got, string(out[0]))
	}
}

func TestConvertCodexResponseToGemini_StreamMultipleToolCallDoneEventsStored(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{\"query\":\"calendar\"}"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected first tool call to be stored until completed, got %d", len(out))
	}
	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"tool_search_call","call_id":"call_search","execution":"client","arguments":{"query":"calendar","limit":1}}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected second tool call to be stored until completed, got %d", len(out))
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`), &param)
	if len(out) != 3 {
		t.Fatalf("expected two stored tool calls plus completed chunk, got %d: %q", len(out), out)
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.name").String(); got != "lookup" {
		t.Fatalf("first stored tool name = %q, want lookup. Output: %s", got, string(out[0]))
	}
	if got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.args.query").String(); got != "calendar" {
		t.Fatalf("first stored query = %q, want calendar. Output: %s", got, string(out[0]))
	}
	if got := gjson.GetBytes(out[1], "candidates.0.content.parts.0.functionCall.name").String(); got != "tool_search" {
		t.Fatalf("second stored tool name = %q, want tool_search. Output: %s", got, string(out[1]))
	}
	if got := gjson.GetBytes(out[1], "candidates.0.content.parts.0.functionCall.args.limit").Int(); got != 1 {
		t.Fatalf("second stored limit = %d, want 1. Output: %s", got, string(out[1]))
	}
	if got := gjson.GetBytes(out[2], "usageMetadata.promptTokenCount").Int(); got != 1 {
		t.Fatalf("completed usage promptTokenCount = %d, want 1. Output: %s", got, string(out[2]))
	}
}

func TestConvertCodexResponseToGemini_StreamUsageMapsOfficialCacheAndReasoningFields(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15,"total_tokens":150}}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected completed chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "usageMetadata.promptTokenCount").Int(); got != 100 {
		t.Fatalf("promptTokenCount = %d, want 100. Output: %s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "usageMetadata.cachedContentTokenCount").Int(); got != 40 {
		t.Fatalf("cachedContentTokenCount = %d, want 40. Output: %s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "usageMetadata.thoughtsTokenCount").Int(); got != 15 {
		t.Fatalf("thoughtsTokenCount = %d, want 15. Output: %s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "usageMetadata.totalTokenCount").Int(); got != 150 {
		t.Fatalf("totalTokenCount = %d, want official total 150. Output: %s", got, out[0])
	}
}

func TestConvertCodexResponseToGemini_NonStreamUsageMapsOfficialCacheAndReasoningFields(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15,"total_tokens":150},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	out := ConvertCodexResponseToGeminiNonStream(ctx, "gemini-2.5-pro", originalRequest, nil, raw, nil)
	if got := gjson.GetBytes(out, "usageMetadata.cachedContentTokenCount").Int(); got != 40 {
		t.Fatalf("cachedContentTokenCount = %d, want 40. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "usageMetadata.thoughtsTokenCount").Int(); got != 15 {
		t.Fatalf("thoughtsTokenCount = %d, want 15. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "usageMetadata.totalTokenCount").Int(); got != 150 {
		t.Fatalf("totalTokenCount = %d, want official total 150. Output: %s", got, string(out))
	}
}
