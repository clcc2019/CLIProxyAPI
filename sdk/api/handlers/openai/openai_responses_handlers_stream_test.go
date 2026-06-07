package openai

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type responsesFirstByteHTTPExecutor struct {
	finish chan struct{}
}

func (e *responsesFirstByteHTTPExecutor) Identifier() string { return "firstbyte" }

func (e *responsesFirstByteHTTPExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *responsesFirstByteHTTPExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.created","response":{"id":"resp-first-byte"}}` + "\n\n")}
	go func() {
		defer close(chunks)
		select {
		case <-ctx.Done():
			return
		case <-e.finish:
		}
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"id":"resp-first-byte","output":[]}}` + "\n\n")}
	}()
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *responsesFirstByteHTTPExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *responsesFirstByteHTTPExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *responsesFirstByteHTTPExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newResponsesStreamTestHandler(t *testing.T) (*OpenAIResponsesAPIHandler, *httptest.ResponseRecorder, *gin.Context, http.Flusher) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	return h, recorder, c, flusher
}

func TestResponsesEndpointFlushesCreatedBeforeStreamCompletes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &responsesFirstByteHTTPExecutor{finish: make(chan struct{})}
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "firstbyte-auth", Provider: "firstbyte", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register(): %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	server := httptest.NewServer(router)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"test-model","stream":true,"input":"hello"}`))
	if err != nil {
		t.Fatalf("NewRequestWithContext(): %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() returned before first streaming byte: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, errRead := bufio.NewReader(resp.Body).ReadString('\n')
		if errRead != nil {
			errCh <- errRead
			return
		}
		lineCh <- line
	}()

	select {
	case line := <-lineCh:
		if !strings.Contains(line, `"response.created"`) {
			t.Fatalf("first line = %q, want response.created", line)
		}
	case errRead := <-errCh:
		t.Fatalf("reading first SSE line: %v", errRead)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for response.created before upstream completion")
	}

	close(executor.finish)
}

func TestForwardResponsesStreamSeparatesDataOnlySSEChunks(t *testing.T) {
	h, recorder, c, flusher := newResponsesStreamTestHandler(t)

	data := make(chan []byte, 2)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"arguments\":\"{}\"}}")
	data <- []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}")
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	body := recorder.Body.String()
	parts := strings.Split(strings.TrimSpace(body), "\n\n")
	if len(parts) != 2 {
		t.Fatalf("expected 2 SSE events, got %d. Body: %q", len(parts), body)
	}

	expectedPart1 := "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"arguments\":\"{}\"}}"
	if parts[0] != expectedPart1 {
		t.Errorf("unexpected first event.\nGot: %q\nWant: %q", parts[0], expectedPart1)
	}

	expectedPart2 := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"function_call\",\"arguments\":\"{}\"}]}}"
	if parts[1] != expectedPart2 {
		t.Errorf("unexpected second event.\nGot: %q\nWant: %q", parts[1], expectedPart2)
	}
}

func TestForwardResponsesStreamRepairsEmptyCompletedOutputFromDoneItems(t *testing.T) {
	h, recorder, c, flusher := newResponsesStreamTestHandler(t)

	data := make(chan []byte, 3)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs-1","summary":[]}}`)
	data <- []byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"shell","arguments":"{\"cmd\":\"pwd\"}","status":"completed"}}`)
	data <- []byte(`data: {"type":"response.completed","response":{"id":"resp-1","output":[]}}`)
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)

	parts := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n\n")
	if len(parts) != 3 {
		t.Fatalf("expected 3 SSE events, got %d. Body: %q", len(parts), recorder.Body.String())
	}

	payload := strings.TrimPrefix(parts[2], "data: ")
	output := gjson.Get(payload, "response.output")
	if !output.IsArray() || len(output.Array()) != 2 {
		t.Fatalf("expected repaired completed output with 2 items, got %s", output.Raw)
	}
	if got := gjson.Get(payload, "response.output.1.name").String(); got != "shell" {
		t.Fatalf("expected function_call name to be preserved, got %q in %s", got, payload)
	}
	if got := gjson.Get(payload, "response.output.1.arguments").String(); got != `{"cmd":"pwd"}` {
		t.Fatalf("expected function_call arguments to be preserved, got %q in %s", got, payload)
	}
}

func TestForwardResponsesStreamRepairsMixedIndexedAndUnindexedDoneItems(t *testing.T) {
	h, recorder, c, flusher := newResponsesStreamTestHandler(t)

	data := make(chan []byte, 3)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"shell","arguments":"{}","status":"completed"}}`)
	data <- []byte(`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`)
	data <- []byte(`data: {"type":"response.completed","response":{"id":"resp-1","output":[]}}`)
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)

	parts := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n\n")
	if len(parts) != 3 {
		t.Fatalf("expected 3 SSE events, got %d. Body: %q", len(parts), recorder.Body.String())
	}

	payload := strings.TrimPrefix(parts[2], "data: ")
	output := gjson.Get(payload, "response.output")
	if !output.IsArray() || len(output.Array()) != 2 {
		t.Fatalf("expected repaired completed output with 2 items, got %s", output.Raw)
	}
	if got := gjson.Get(payload, "response.output.0.name").String(); got != "shell" {
		t.Fatalf("expected indexed function_call to be preserved first, got %q in %s", got, payload)
	}
	if got := gjson.Get(payload, "response.output.1.id").String(); got != "msg-1" {
		t.Fatalf("expected unindexed message to be appended, got %q in %s", got, payload)
	}
}

func TestForwardResponsesStreamRepairsMultilineCompletedOutputAsSSEDataLines(t *testing.T) {
	h, recorder, c, flusher := newResponsesStreamTestHandler(t)

	data := make(chan []byte, 2)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","arguments":"{}"}}`)
	data <- []byte("data: {\"type\":\"response.completed\",\ndata: \"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n")
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)

	parts := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n\n")
	if len(parts) != 2 {
		t.Fatalf("expected 2 SSE events, got %d. Body: %q", len(parts), recorder.Body.String())
	}

	completedFrame := []byte(parts[1])
	for _, line := range strings.Split(parts[1], "\n") {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("expected every completed payload line to be an SSE data line, got %q in %q", line, parts[1])
		}
	}

	payload, ok := responsesSSEDataPayload(completedFrame)
	if !ok {
		t.Fatalf("expected completed frame to contain data payload: %q", parts[1])
	}
	output := gjson.GetBytes(payload, "response.output")
	if !output.IsArray() || len(output.Array()) != 1 {
		t.Fatalf("expected repaired completed output with 1 item, got %s from %q", output.Raw, payload)
	}
}

func TestResponsesSSEDataPayloadHandlesSingleAndMultipleDataLines(t *testing.T) {
	singleFrame := []byte("event: response.created\r\ndata: {\"type\":\"response.created\"}\r\n\r\n")
	single, ok := responsesSSEDataPayload(singleFrame)
	if !ok || string(single) != `{"type":"response.created"}` {
		t.Fatalf("single payload = %q, %t", single, ok)
	}

	multiple, ok := responsesSSEDataPayload([]byte("data: {\"type\":\"response.completed\",\ndata: \"response\":{\"output\":[]}}\n\n"))
	if !ok || string(multiple) != `{"type":"response.completed","response":{"output":[]}}` {
		t.Fatalf("multiple payload = %q, %t", multiple, ok)
	}

	if payload, ok := responsesSSEDataPayload([]byte("event: response.created\n\n")); ok || payload != nil {
		t.Fatalf("non-data payload = %q, %t; want nil, false", payload, ok)
	}
}

func TestResponsesSSEFrameWithDataPayload(t *testing.T) {
	tests := []struct {
		name    string
		frame   string
		payload string
		want    string
	}{
		{
			name:    "replaces first data and removes remaining data",
			frame:   "event: response.completed\ndata: old-1\nid: resp-1\ndata: old-2\n\n",
			payload: "{\"type\":\"response.completed\"}",
			want:    "event: response.completed\ndata: {\"type\":\"response.completed\"}\nid: resp-1\n\n",
		},
		{
			name:    "appends data when absent",
			frame:   "event: response.completed\r\nid: resp-1\r\n\r\n",
			payload: "{\"type\":\"response.completed\"}",
			want:    "event: response.completed\nid: resp-1\ndata: {\"type\":\"response.completed\"}\n\n",
		},
		{
			name:    "writes multiline payload",
			frame:   "event: response.completed\ndata: old\n\n",
			payload: "{\n\"type\":\"response.completed\"\n}",
			want:    "event: response.completed\ndata: {\ndata: \"type\":\"response.completed\"\ndata: }\n\n",
		},
		{
			name:    "preserves trailing empty payload line",
			frame:   "data: old\n\n",
			payload: "first\n",
			want:    "data: first\ndata: \n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := responsesSSEFrameWithDataPayload([]byte(tt.frame), []byte(tt.payload))
			if string(got) != tt.want {
				t.Fatalf("responsesSSEFrameWithDataPayload() = %q, want %q", got, tt.want)
			}
		})
	}
}

var responsesSSEFrameWithDataPayloadBenchmarkSink []byte

func BenchmarkResponsesSSEFrameWithDataPayload(b *testing.B) {
	frame := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n")
	payload := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"id\":\"msg-1\",\"type\":\"message\"}]}}")
	b.ReportAllocs()
	for b.Loop() {
		responsesSSEFrameWithDataPayloadBenchmarkSink = responsesSSEFrameWithDataPayload(frame, payload)
	}
}

func TestForwardResponsesStreamReassemblesSplitSSEEventChunks(t *testing.T) {
	h, recorder, c, flusher := newResponsesStreamTestHandler(t)

	data := make(chan []byte, 3)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("event: response.created")
	data <- []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}")
	data <- []byte("\n")
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)

	got := strings.TrimSuffix(recorder.Body.String(), "\n")
	want := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n"
	if got != want {
		t.Fatalf("unexpected split-event framing.\nGot:  %q\nWant: %q", got, want)
	}
}

func TestForwardResponsesStreamPreservesValidFullSSEEventChunks(t *testing.T) {
	h, recorder, c, flusher := newResponsesStreamTestHandler(t)

	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	chunk := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	data <- chunk
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)

	got := strings.TrimSuffix(recorder.Body.String(), "\n")
	if got != string(chunk) {
		t.Fatalf("unexpected full-event framing.\nGot:  %q\nWant: %q", got, string(chunk))
	}
}

func TestForwardResponsesStreamBuffersSplitDataPayloadChunks(t *testing.T) {
	h, recorder, c, flusher := newResponsesStreamTestHandler(t)

	data := make(chan []byte, 2)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("data: {\"type\":\"response.created\"")
	data <- []byte(",\"response\":{\"id\":\"resp-1\"}}")
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)

	got := recorder.Body.String()
	want := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n\n"
	if got != want {
		t.Fatalf("unexpected split-data framing.\nGot:  %q\nWant: %q", got, want)
	}
}

func TestResponsesSSENeedsLineBreakSkipsChunksThatAlreadyStartWithNewline(t *testing.T) {
	if responsesSSENeedsLineBreak([]byte("event: response.created"), []byte("\n")) {
		t.Fatal("expected no injected newline before newline-only chunk")
	}
	if responsesSSENeedsLineBreak([]byte("event: response.created"), []byte("\r\n")) {
		t.Fatal("expected no injected newline before CRLF chunk")
	}
}

func TestForwardResponsesStreamDropsIncompleteTrailingDataChunkOnFlush(t *testing.T) {
	h, recorder, c, flusher := newResponsesStreamTestHandler(t)

	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("data: {\"type\":\"response.created\"")
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)

	if got := recorder.Body.String(); got != "\n" {
		t.Fatalf("expected incomplete trailing data to be dropped on flush.\nGot: %q", got)
	}
}

func TestResponsesSSEFramerTrustedDataPassesThroughCompleteFrames(t *testing.T) {
	var out bytes.Buffer
	framer := &responsesSSEFramer{
		noticeFilter: newResponsesNoticeFilter(),
		trustedData:  true,
	}
	chunk := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")

	framer.WriteChunk(&out, chunk)

	if got := out.String(); got != string(chunk) {
		t.Fatalf("trusted framer should preserve complete frame.\nGot:  %q\nWant: %q", got, string(chunk))
	}
}

func TestResponsesSSEFramerTrustedDataStillFiltersUsageWarnings(t *testing.T) {
	var out bytes.Buffer
	framer := &responsesSSEFramer{
		noticeFilter: newResponsesNoticeFilter(),
		trustedData:  true,
	}

	framer.WriteChunk(&out, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-1\",\"delta\":\"Heads up, you have less than 25% of your 5h limit left. Run /status for a breakdown.\"}\n\n"))
	framer.WriteChunk(&out, []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"id\":\"msg-2\",\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"real output\"}]}]}}\n\n"))

	if bytes.Contains(out.Bytes(), []byte("5h limit left")) {
		t.Fatalf("usage warning should still be filtered in trusted mode")
	}
	if !bytes.Contains(out.Bytes(), []byte("real output")) {
		t.Fatalf("normal payload should remain in trusted mode")
	}
}

func TestResponsesSSEFramerTrustedDataFiltersUsageWarningSplitInsidePayload(t *testing.T) {
	const warning = "Heads up, you have less than 25% of your 5h limit left. Run /status for a breakdown."
	baseFrame := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-1\",\"delta\":\"" + warning + "\"}")

	for _, suffix := range []string{"\n\n", ""} {
		frame := append(append([]byte(nil), baseFrame...), suffix...)
		warningStart := bytes.Index(frame, []byte(warning))
		if warningStart < 0 {
			t.Fatal("warning text missing from test frame")
		}

		for split := warningStart + 1; split < warningStart+len(warning); split++ {
			var out bytes.Buffer
			framer := &responsesSSEFramer{
				noticeFilter: newResponsesNoticeFilter(),
				trustedData:  true,
			}
			if framer.WriteChunk(&out, frame[:split]) {
				t.Fatalf("suffix %q split %d wrote incomplete warning prefix: %q", suffix, split, out.String())
			}
			if framer.WriteChunk(&out, frame[split:]) {
				t.Fatalf("suffix %q split %d wrote filtered warning frame: %q", suffix, split, out.String())
			}
			if framer.Flush(&out) {
				t.Fatalf("suffix %q split %d wrote filtered warning on flush: %q", suffix, split, out.String())
			}
			if out.Len() != 0 {
				t.Fatalf("suffix %q split %d leaked warning frame: %q", suffix, split, out.String())
			}
		}
	}
}

func TestResponsesSSEFramerTrustedDataBuffersNormalFrameSplitInsidePayload(t *testing.T) {
	const text = "normal output"
	baseFrame := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg-1\",\"delta\":\"" + text + "\"}")

	for _, suffix := range []string{"\n\n", ""} {
		frame := append(append([]byte(nil), baseFrame...), suffix...)
		want := append(append([]byte(nil), baseFrame...), '\n', '\n')
		textStart := bytes.Index(frame, []byte(text))
		if textStart < 0 {
			t.Fatal("normal output missing from test frame")
		}

		for split := textStart + 1; split < textStart+len(text); split++ {
			var out bytes.Buffer
			framer := &responsesSSEFramer{
				noticeFilter: newResponsesNoticeFilter(),
				trustedData:  true,
			}
			if framer.WriteChunk(&out, frame[:split]) {
				t.Fatalf("suffix %q split %d wrote incomplete normal frame: %q", suffix, split, out.String())
			}
			if !framer.WriteChunk(&out, frame[split:]) {
				t.Fatalf("suffix %q split %d did not write completed normal frame", suffix, split)
			}
			if got := out.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("suffix %q split %d changed normal frame.\nGot:  %q\nWant: %q", suffix, split, got, want)
			}
		}
	}
}

func TestResponsesSSEFrameLenFindsLFAndCRLFDelimiters(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
		want  int
	}{
		{
			name:  "lf",
			chunk: "data: {}\n\nrest",
			want:  len("data: {}\n\n"),
		},
		{
			name:  "crlf",
			chunk: "data: {}\r\n\r\nrest",
			want:  len("data: {}\r\n\r\n"),
		},
		{
			name:  "none",
			chunk: "data: {}",
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responsesSSEFrameLen([]byte(tt.chunk)); got != tt.want {
				t.Fatalf("frame len = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSSEFrameAccumulatorAddChunkPreservesMultipleFrames(t *testing.T) {
	var acc sseFrameAccumulator
	frames := acc.AddChunk([]byte("data: {\"type\":\"first\"}\n\ndata: {\"type\":\"second\"}\n\n"))
	if len(frames) != 2 {
		t.Fatalf("len(frames)=%d, want 2", len(frames))
	}
	if got, want := string(frames[0]), "data: {\"type\":\"first\"}\n\n"; got != want {
		t.Fatalf("first frame = %q, want %q", got, want)
	}
	if got, want := string(frames[1]), "data: {\"type\":\"second\"}\n\n"; got != want {
		t.Fatalf("second frame = %q, want %q", got, want)
	}
}

func TestSSEFrameAccumulatorDirectPathPreservesPendingAfterStop(t *testing.T) {
	var acc sseFrameAccumulator
	chunk := []byte("data: {\"type\":\"first\"}\n\ndata: {\"type\":\"second\"}\n\n")
	count := 0
	if acc.ForEachChunkFrame(chunk, func(frame []byte) bool {
		count++
		return false
	}) {
		t.Fatalf("ForEachChunkFrame returned true after callback stopped")
	}
	if count != 1 {
		t.Fatalf("callback count = %d, want 1", count)
	}

	var flushed [][]byte
	if !acc.FlushFrames(func(frame []byte) bool {
		flushed = append(flushed, append([]byte(nil), frame...))
		return true
	}) {
		t.Fatalf("FlushFrames returned false")
	}
	if len(flushed) != 1 {
		t.Fatalf("flushed frame count = %d, want 1", len(flushed))
	}
	if got, want := string(flushed[0]), "data: {\"type\":\"second\"}\n\n"; got != want {
		t.Fatalf("flushed frame = %q, want %q", got, want)
	}
}

func BenchmarkResponsesSSEFrameLen(b *testing.B) {
	chunk := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if responsesSSEFrameLen(chunk) != len(chunk) {
			b.Fatal("unexpected frame len")
		}
	}
}

func BenchmarkResponsesSSEDataPayloadSingleLine(b *testing.B) {
	frame := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	b.ReportAllocs()
	for b.Loop() {
		payload, ok := responsesSSEDataPayload(frame)
		if !ok || len(payload) == 0 {
			b.Fatal("expected data payload")
		}
	}
}

func BenchmarkResponsesSSEDataPayloadMultipleLines(b *testing.B) {
	frame := []byte("data: {\"type\":\"response.completed\",\ndata: \"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n")
	b.ReportAllocs()
	for b.Loop() {
		payload, ok := responsesSSEDataPayload(frame)
		if !ok || len(payload) == 0 {
			b.Fatal("expected data payload")
		}
	}
}

func BenchmarkResponsesSSEFramerWriteChunkManyFrames(b *testing.B) {
	frame := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	chunk := bytes.Repeat(frame, 16)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		framer := &responsesSSEFramer{trustedData: true}
		if !framer.WriteChunk(io.Discard, chunk) {
			b.Fatal("expected write")
		}
	}
}

func BenchmarkSSEFrameAccumulatorForEachChunkFrameManyFrames(b *testing.B) {
	frame := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	chunk := bytes.Repeat(frame, 16)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var acc sseFrameAccumulator
		count := 0
		acc.ForEachChunkFrame(chunk, func(frame []byte) bool {
			count++
			return true
		})
		if count != 16 {
			b.Fatalf("count=%d, want 16", count)
		}
	}
}
