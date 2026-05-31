package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAI_StreamSetsModelFromResponseCreated(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.3-codex"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected no output for response.created, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_FirstChunkUsesRequestModelName(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallChunkOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls").Exists() {
		t.Fatalf("expected tool_calls to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallArgumentsDeltaOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected tool call announcement chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"query\":\"OpenAI\"}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").Exists() {
		t.Fatalf("expected tool call arguments delta to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_CustomToolCallStreamsAsToolCall(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":""}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected custom tool call announcement chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.id").String(); got != "call_patch" {
		t.Fatalf("tool call id = %q, want call_patch; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "apply_patch" {
		t.Fatalf("tool call name = %q, want apply_patch; chunk=%s", got, out[0])
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.custom_tool_call_input.delta","call_id":"call_patch","delta":"*** Begin Patch\n"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected custom tool input delta chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "*** Begin Patch\n" {
		t.Fatalf("custom tool arguments delta = %q; chunk=%s", got, out[0])
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.completed","response":{"status":"completed"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected completion chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_ToolSearchCallStreamsAsToolCall(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"tool_search_call","call_id":"search_1","execution":"client","arguments":{"query":"calendar","limit":1}}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected tool search announcement chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.id").String(); got != "search_1" {
		t.Fatalf("tool call id = %q, want search_1; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "tool_search" {
		t.Fatalf("tool call name = %q, want tool_search; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != `{"query":"calendar","limit":1}` {
		t.Fatalf("tool search arguments = %q; chunk=%s", got, out[0])
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.completed","response":{"status":"completed"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected completion chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_ServerToolSearchIsInternal(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"tool_search_call","execution":"server","call_id":"server_search","status":"completed","arguments":{"paths":["crm"]}}}`), &param)
	if len(out) != 0 {
		t.Fatalf("server-side tool search should not become a client tool call, got %d chunks: %q", len(out), out)
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.completed","response":{"status":"completed"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected completion chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_MultipleAnnouncedToolCallDoneEventsAreNotDuplicated(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected first tool call announcement chunk, got %d", len(out))
	}
	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected second tool call announcement chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("first announced done event should not duplicate tool call, got %d: %q", len(out), out)
	}
	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read","arguments":"{}"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("second announced done event should not duplicate tool call, got %d: %q", len(out), out)
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.completed","response":{"status":"completed"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected completion chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallArgumentDeltasUseMatchingOutputIndex(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected first tool call announcement chunk, got %d", len(out))
	}
	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected second tool call announcement chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"query\":\"calendar\"}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected first tool argument delta, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("first tool argument delta index = %d, want 0; chunk=%s", got, out[0])
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_2","arguments":"{\"path\":\"README.md\"}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected second tool full arguments fallback, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
		t.Fatalf("second tool arguments done index = %d, want 1; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != `{"path":"README.md"}` {
		t.Fatalf("second tool arguments = %q, want README payload; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_StreamReasoningDoneFallbackUsesSummary(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"first "},{"type":"summary_text","text":"second"}]}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected reasoning fallback chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").String(); got != "first second" {
		t.Fatalf("reasoning_content = %q, want joined summary; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_StreamReasoningDoneFallbackUsesContent(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"hidden "},{"type":"text","text":"trace"}]}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected reasoning content fallback chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").String(); got != "hidden trace" {
		t.Fatalf("reasoning_content = %q, want joined content; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_StreamReasoningTextDelta(t *testing.T) {
	ctx := context.Background()
	var param any

	ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`), &param)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.reasoning_text.delta","delta":"raw trace","content_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected reasoning_text delta chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").String(); got != "raw trace" {
		t.Fatalf("reasoning_content = %q, want raw trace; chunk=%s", got, out[0])
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"raw trace"}]}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected done for streamed raw reasoning item to be suppressed, got %d chunks: %q", len(out), out)
	}
}

func TestConvertCodexResponseToOpenAI_StreamReasoningDeltaStateResetsPerItem(t *testing.T) {
	ctx := context.Background()
	var param any

	ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`), &param)
	ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"streamed"}`), &param)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"streamed"}]}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected done for streamed reasoning item to be suppressed, got %d chunks", len(out))
	}

	ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`), &param)
	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"fallback"}]}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected second reasoning item fallback chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").String(); got != "fallback" {
		t.Fatalf("reasoning_content = %q, want fallback; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_StreamUsageMapsOfficialCacheAndReasoningFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15,"total_tokens":150}}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected completed chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "usage.prompt_tokens_details.cached_tokens").Int(); got != 40 {
		t.Fatalf("cached_tokens = %d, want 40; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "usage.completion_tokens_details.reasoning_tokens").Int(); got != 15 {
		t.Fatalf("reasoning_tokens = %d, want 15; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAINonStream_UsageMapsOfficialCacheAndReasoningFields(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":50,"reasoning_output_tokens":15,"total_tokens":150},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)
	if got := gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int(); got != 40 {
		t.Fatalf("cached_tokens = %d, want 40; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 15 {
		t.Fatalf("reasoning_tokens = %d, want 15; output=%s", got, out)
	}
}

func TestConvertCodexResponseToOpenAI_StreamPartialImageEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	chunk := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out[0]))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 0 {
		t.Fatalf("expected duplicate image chunk to be suppressed, got %d", len(out))
	}
}

func TestConvertCodexResponseToOpenAI_StreamImageGenerationCallDoneEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"png","result":"aGVsbG8="}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected output_item.done to be suppressed when identical to last partial image, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"jpeg","result":"Ymll"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/jpeg;base64,Ymll" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/jpeg;base64,Ymll", gotURL, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamImageGenerationCallAddsMessageImages(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","output_format":"png","result":"aGVsbG8="}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	gotURL := gjson.GetBytes(out, "choices.0.message.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out))
	}
}

func TestConvertCodexResponseToOpenAINonStream_CustomToolCallAddsToolCalls(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","output":[{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.id").String(); got != "call_patch" {
		t.Fatalf("tool call id = %q, want call_patch; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String(); got != "apply_patch" {
		t.Fatalf("tool call name = %q, want apply_patch; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(); got != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("tool call arguments = %q; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; body=%s", got, out)
	}
}

func TestConvertCodexResponseToOpenAINonStream_ToolSearchCallAddsToolCalls(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","output":[{"type":"tool_search_call","call_id":"search_1","execution":"client","arguments":{"query":"calendar","limit":1}}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.id").String(); got != "search_1" {
		t.Fatalf("tool call id = %q, want search_1; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String(); got != "tool_search" {
		t.Fatalf("tool call name = %q, want tool_search; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"query":"calendar","limit":1}` {
		t.Fatalf("tool call arguments = %q; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; body=%s", got, out)
	}
}

func TestConvertCodexResponseToOpenAINonStream_ServerToolSearchIsInternal(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","output":[{"type":"tool_search_call","execution":"server","call_id":"server_search","status":"completed","arguments":{"paths":["crm"]}}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	if toolCalls := gjson.GetBytes(out, "choices.0.message.tool_calls"); toolCalls.Exists() && toolCalls.Type != gjson.Null {
		t.Fatalf("server-side tool search should not become a client tool call; body=%s", out)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop; body=%s", got, out)
	}
}

func TestConvertCodexResponseToOpenAINonStream_ReasoningTextFromSummaryAndContent(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"first "},{"type":"summary_text","text":"second"}]},{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":" third"}]}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "first second third" {
		t.Fatalf("reasoning_content = %q, want combined reasoning; body=%s", got, out)
	}
}
