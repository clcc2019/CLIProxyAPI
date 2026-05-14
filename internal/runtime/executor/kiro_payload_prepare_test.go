package executor

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPrepareKiroPayloadForUpstream_StripsToolContextWhenNoTools(t *testing.T) {
	payload := []byte(`{
		"conversationState":{
			"conversationId":"c",
			"history":[
				{"userInputMessage":{"content":"u","modelId":"m","origin":"AI_EDITOR"}},
				{"assistantResponseMessage":{"content":"a","toolUses":[{"toolUseId":"tu1","name":"Read","input":{"path":"x"}}]}},
				{"userInputMessage":{"content":"r","modelId":"m","origin":"AI_EDITOR","userInputMessageContext":{"toolResults":[{"toolUseId":"tu1","status":"success","content":[{"text":"history ok"}]}]}}}
			],
			"currentMessage":{"userInputMessage":{"content":"next","modelId":"m","origin":"AI_EDITOR","userInputMessageContext":{"toolResults":[{"toolUseId":"tu1","status":"success","content":[{"text":"current ok"}]}]}}}
		}
	}`)

	prepared, stats, err := prepareKiroPayloadForUpstreamWithLimit(payload, 16<<10)
	if err != nil {
		t.Fatalf("prepare error: %v", err)
	}
	if !stats.StrippedToolContext {
		t.Fatalf("expected tool context to be stripped: %+v", stats)
	}
	if strings.Contains(string(prepared), `"toolUses"`) || strings.Contains(string(prepared), `"toolResults"`) {
		t.Fatalf("tool context should be removed from payload: %s", prepared)
	}
	joined := string(prepared)
	if !strings.Contains(joined, "Tool call Read") || !strings.Contains(joined, "history ok") || !strings.Contains(joined, "current ok") {
		t.Fatalf("tool context text was not preserved: %s", joined)
	}
}

func TestPrepareKiroPayloadForUpstream_PreservesToolContextWhenToolsExist(t *testing.T) {
	payload := []byte(`{
		"conversationState":{
			"conversationId":"c",
			"history":[
				{"userInputMessage":{"content":"u","modelId":"m","origin":"AI_EDITOR"}},
				{"assistantResponseMessage":{"content":"a","toolUses":[{"toolUseId":"tu1","name":"Read","input":{"path":"x"}}]}}
			],
			"currentMessage":{"userInputMessage":{"content":"next","modelId":"m","origin":"AI_EDITOR","userInputMessageContext":{
				"tools":[{"toolSpecification":{"name":"Read","description":"Read file","inputSchema":{"json":{"type":"object"}}}}],
				"toolResults":[{"toolUseId":"tu1","status":"success","content":[{"text":"ok"}]}]
			}}}
		}
	}`)

	prepared, stats, err := prepareKiroPayloadForUpstreamWithLimit(payload, 16<<10)
	if err != nil {
		t.Fatalf("prepare error: %v", err)
	}
	if stats.StrippedToolContext {
		t.Fatalf("did not expect tool context stripping: %+v", stats)
	}
	if !gjson.GetBytes(prepared, `conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.0.toolSpecification.name`).Exists() {
		t.Fatalf("tools should be preserved: %s", prepared)
	}
	if !gjson.GetBytes(prepared, `conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults.0.toolUseId`).Exists() {
		t.Fatalf("toolResults should be preserved: %s", prepared)
	}
}

func TestPrepareKiroPayloadForUpstream_RepairsOrphanedToolResultsWithTools(t *testing.T) {
	payload := []byte(`{
		"conversationState":{
			"conversationId":"c",
			"currentMessage":{"userInputMessage":{"content":"next","modelId":"m","origin":"AI_EDITOR","userInputMessageContext":{
				"tools":[{"toolSpecification":{"name":"Read","description":"Read file","inputSchema":{"json":{"type":"object"}}}}],
				"toolResults":[{"toolUseId":"missing","status":"success","content":[{"text":"orphan result"}]}]
			}}}
		}
	}`)

	prepared, stats, err := prepareKiroPayloadForUpstreamWithLimit(payload, 16<<10)
	if err != nil {
		t.Fatalf("prepare error: %v", err)
	}
	if !stats.RepairedToolResults {
		t.Fatalf("expected orphaned tool results to be repaired: %+v", stats)
	}
	if gjson.GetBytes(prepared, `conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults`).Exists() {
		t.Fatalf("orphaned toolResults should be converted to text: %s", prepared)
	}
	if !gjson.GetBytes(prepared, `conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.0.toolSpecification.name`).Exists() {
		t.Fatalf("tools should still be preserved: %s", prepared)
	}
	if !strings.Contains(gjson.GetBytes(prepared, `conversationState.currentMessage.userInputMessage.content`).String(), "orphan result") {
		t.Fatalf("orphaned result text not preserved: %s", prepared)
	}
}

func TestPrepareKiroPayloadForUpstream_TrimsHistoryAndRepairsCurrentToolResults(t *testing.T) {
	payload := []byte(`{
		"conversationState":{
			"conversationId":"c",
			"history":[
				{"userInputMessage":{"content":"` + strings.Repeat("x", 1200) + `","modelId":"m","origin":"AI_EDITOR"}},
				{"assistantResponseMessage":{"content":"a","toolUses":[{"toolUseId":"tu1","name":"Read","input":{"path":"x"}}]}}
			],
			"currentMessage":{"userInputMessage":{"content":"next","modelId":"m","origin":"AI_EDITOR","userInputMessageContext":{
				"tools":[{"toolSpecification":{"name":"Read","description":"Read file","inputSchema":{"json":{"type":"object"}}}}],
				"toolResults":[{"toolUseId":"tu1","status":"success","content":[{"text":"result after trim"}]}]
			}}}
		}
	}`)

	prepared, stats, err := prepareKiroPayloadForUpstreamWithLimit(payload, 900)
	if err != nil {
		t.Fatalf("prepare error: %v", err)
	}
	if !stats.TrimmedHistory {
		t.Fatalf("expected history trimming: %+v", stats)
	}
	if got := len(gjson.GetBytes(prepared, `conversationState.history`).Array()); got != 0 {
		t.Fatalf("history length = %d, want 0: %s", got, prepared)
	}
	if gjson.GetBytes(prepared, `conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults`).Exists() {
		t.Fatalf("orphaned current toolResults should be converted to text: %s", prepared)
	}
	if !strings.Contains(gjson.GetBytes(prepared, `conversationState.currentMessage.userInputMessage.content`).String(), "result after trim") {
		t.Fatalf("current content did not retain orphaned tool result: %s", prepared)
	}
}

func TestPrepareKiroPayloadForUpstream_RejectsOversizeCurrentMessage(t *testing.T) {
	payload := []byte(`{"conversationState":{"conversationId":"c","currentMessage":{"userInputMessage":{"content":"` + strings.Repeat("x", 512) + `","modelId":"m","origin":"AI_EDITOR"}}}}`)

	_, _, err := prepareKiroPayloadForUpstreamWithLimit(payload, 256)
	if err == nil {
		t.Fatal("expected oversize payload error")
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) {
		t.Fatalf("expected status error, got %T", err)
	}
	if got := status.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
}

func TestPrepareKiroPayloadForUpstream_NormalizesHistoryShape(t *testing.T) {
	payload := []byte(`{
		"conversationState":{
			"conversationId":"c",
			"history":[
				{"assistantResponseMessage":{"content":"starts with assistant"}},
				{"userInputMessage":{"content":"u1","modelId":"m","origin":"AI_EDITOR"}},
				{"userInputMessage":{"content":"u2","modelId":"m","origin":"AI_EDITOR"}}
			],
			"currentMessage":{"userInputMessage":{"content":"next","modelId":"m","origin":"AI_EDITOR"}}
		}
	}`)

	prepared, stats, err := prepareKiroPayloadForUpstreamWithLimit(payload, 16<<10)
	if err != nil {
		t.Fatalf("prepare error: %v", err)
	}
	if !stats.NormalizedHistory {
		t.Fatalf("expected history normalization: %+v", stats)
	}
	history := gjson.GetBytes(prepared, `conversationState.history`).Array()
	for i, entry := range history {
		wantUser := i%2 == 0
		if wantUser && !entry.Get("userInputMessage").Exists() {
			t.Fatalf("history[%d] should be user: %s", i, prepared)
		}
		if !wantUser && !entry.Get("assistantResponseMessage").Exists() {
			t.Fatalf("history[%d] should be assistant: %s", i, prepared)
		}
	}
}
