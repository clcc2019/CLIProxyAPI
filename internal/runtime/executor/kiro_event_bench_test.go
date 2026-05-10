package executor

import (
	"testing"
)

// kiroBenchContentFrame is the overwhelmingly common high-volume event: a
// text delta from the model. Usage and tool_use events are rare relative to
// content deltas, so benchmark optimisations must keep this path fast.
var kiroBenchContentFrame = []byte(`{
  "assistantResponseEvent": {
    "content": "Here is a sample chunk of assistant output that might be sent mid-stream.",
    "text": ""
  }
}`)

// kiroBenchToolUseFrame exercises the tool_use path, which cannot use the
// content fast-path and must still build a map.
var kiroBenchToolUseFrame = []byte(`{
  "toolUseEvent": {
    "toolUseId": "toolu_abc",
    "name": "shell",
    "input": "{\"command\":\"ls -la\"}",
    "stop": true
  }
}`)

// kiroBenchMetadataFrame has a nested tokenUsage container, which the
// usage extractor must fully walk.
var kiroBenchMetadataFrame = []byte(`{
  "messageMetadataEvent": {
    "tokenUsage": {
      "uncachedInputTokens": 420,
      "cacheReadInputTokens": 110,
      "cacheWriteInputTokens": 35,
      "outputTokens": 512,
      "totalTokens": 1077
    }
  }
}`)

func BenchmarkParseKiroEventPayload_Content(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = parseKiroEventPayload(kiroBenchContentFrame, "assistantResponseEvent")
	}
}

func BenchmarkParseKiroEventPayload_ToolUse(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = parseKiroEventPayload(kiroBenchToolUseFrame, "toolUseEvent")
	}
}

func BenchmarkParseKiroEventPayload_Metadata(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = parseKiroEventPayload(kiroBenchMetadataFrame, "messageMetadataEvent")
	}
}
